package transfer

// 封面出库的共用路径(TODO-10):上传管线(derived)与 GUI 导入(custom)
// 都走"缩略图字节 → 加密成独立小 blob → PUT covers 命名空间 → 索引写引用"
// 的同一条路。引用规则:先传字节后写引用(不变量 8);PutCover 返回的
// prevBlob 必须同事务 trash,否则旧封面字节永久悬空(custom 保护下 gc
// 永不自动删"账上无、远端有"的对象)。

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"kist/internal/config"
	"kist/internal/crypto"
	"kist/internal/index"
)

// coverOut 是封面出库的中间产物:加密好的 blob 临时文件与其远端身份。
type coverOut struct {
	name string // 远端对象名(32hex 随机,与文件 blob 名同源 newHexID)
	size int64  // 密文大小(v2 量化后)
	path string // 临时文件路径(直传 PUT 或挪入出站箱)
	td   ThumbData
}

// encryptCoverArtifact 把封面字节加密成 blob 临时文件。≤128KB 恒单块,
// v2 量化归 4KiB 档,开销可忽略;分块参数沿用当前设置,与文件一致。
func (m *Manager) encryptCoverArtifact(mk crypto.MasterKey, td ThumbData, tmpDir string) (*coverOut, error) {
	name := newHexID()
	p := filepath.Join(tmpDir, "cover")
	f, err := os.Create(p)
	if err != nil {
		return nil, err
	}
	cw, err := crypto.NewBlobWriter(f, mk, crypto.EncryptOptions{ChunkSize: m.chunkBytes(), NoPadding: m.noPad()})
	if err != nil {
		f.Close()
		return nil, err
	}
	if _, err := cw.Write(td.Data); err != nil {
		f.Close()
		return nil, err
	}
	if err := cw.Close(); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	st, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	return &coverOut{name: name, size: st.Size(), path: p, td: td}, nil
}

// ImportCover 把 GUI 导入的文件封面出库(用户编辑,与 SetNote 同级计 revision)。
// f 是封面所属文件行(须未软删,调用方校验)。管线与语义见 importCover。
func (m *Manager) ImportCover(ctx context.Context, f index.FileRow, td ThumbData) (deferred bool, err error) {
	deferred, err = m.importCover(ctx, td, func(tx *sql.Tx, blobName string, size int64, state string) (string, error) {
		return m.deps.DB.PutCover(tx, index.CoverRow{
			FileID: f.ID, BlobName: blobName, Size: size,
			Width: td.W, Height: td.H, Mime: td.Mime,
			Source: index.CoverCustom, State: state, CreatedAt: time.Now().Unix(),
		})
	}, f.Name)
	if deferred {
		m.emit("index:changed", map[string]any{"reason": "cover-defer", "fileID": f.ID})
	}
	return deferred, err
}

// ImportFolderCover 把 GUI 导入的目录封面出库,属主从文件换成目录
// (v6 起目录封面持有式,不再引用库内文件)。folder 须未软删,调用方校验。
func (m *Manager) ImportFolderCover(ctx context.Context, folder index.FolderRow, td ThumbData) (deferred bool, err error) {
	deferred, err = m.importCover(ctx, td, func(tx *sql.Tx, blobName string, size int64, state string) (string, error) {
		return m.deps.DB.PutFolderCover(tx, index.FolderCoverRow{
			FolderID: folder.ID, BlobName: blobName, Size: size,
			Width: td.W, Height: td.H, Mime: td.Mime,
			Source: index.CoverCustom, State: state, CreatedAt: time.Now().Unix(),
		})
	}, folder.Name)
	if deferred {
		m.emit("index:changed", map[string]any{"reason": "cover-defer", "folderID": folder.ID})
	}
	return deferred, err
}

// importCover 是文件/目录封面导入的共用核心(用户编辑,与 SetNote 同级计
// revision):加密 → PUT covers 命名空间 → 同事务写引用(custom,旧引用
// blob 同事务 trash)。远端不可达时回退出站箱(引用 uploading 不可见、
// 产物待 push/verify 收账),返回 deferred=true 供壳层提示——用户内容
// 绝不静默丢弃。put 闭包只负责"往哪张表写引用"(covers/folder_covers),
// blob 身份与闭环记账在核心统一完成;ownerName 仅用于日志。
func (m *Manager) importCover(ctx context.Context, td ThumbData,
	put func(tx *sql.Tx, blobName string, size int64, state string) (prevBlob string, err error),
	ownerName string) (deferred bool, err error) {

	mk, ok := m.deps.MK()
	if !ok {
		return false, errsLocked()
	}
	tmpDir, err := os.MkdirTemp(tempRoot(), "cover-*")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(tmpDir)
	cover, err := m.encryptCoverArtifact(mk, td, tmpDir)
	if err != nil {
		return false, err
	}

	cf, err := os.Open(cover.path)
	if err != nil {
		return false, err
	}
	perr := m.remoteSnapshot().PutCoverBlob(ctx, cover.name, cf, nil)
	cf.Close()
	if perr == nil {
		// 先字节后引用:PUT 成功才写 ready 引用
		if err := m.deps.DB.WithTx(func(tx *sql.Tx) error {
			prev, err := put(tx, cover.name, cover.size, index.CoverReady)
			if err != nil {
				return err
			}
			if err := m.deps.DB.MarkBlobTrashTx(tx, []string{prev}); err != nil {
				return err
			}
			return m.deps.DB.RegisterBlob(tx, cover.name, "cover", cover.size)
		}); err != nil {
			// 引用写入失败:封面 blob 已成孤儿,gc 处置(与文件上传同款语义)
			slog.Warn("封面引用写入失败,远端 blob 已成孤儿", "cover", cover.name, "owner", ownerName, "err", err)
			return false, err
		}
		return false, nil
	}

	// 远端不可达:回退出站箱。引用 uploading(不可见),产物待运,
	// push/verify 收账翻 ready——弱网用户的自定义封面不因一次断网丢失。
	if err := m.deps.DB.WithTx(func(tx *sql.Tx) error {
		prev, err := put(tx, cover.name, cover.size, index.CoverUploading)
		if err != nil {
			return err
		}
		if err := m.deps.DB.MarkBlobTrashTx(tx, []string{prev}); err != nil {
			return err
		}
		return m.deps.DB.RegisterBlobPending(tx, cover.name, "cover", cover.size)
	}); err != nil {
		return false, err
	}
	if err := moveArtifact(cover.path, config.OutboxDir(), cover.name); err != nil {
		return false, err
	}
	return true, nil
}

// ClearCoverFile 清除文件封面:纯索引零网络——删引用行 + 旧 blob 同事务
// trash,物理删除交给 gc。挂账中(uploading)的封面同样可弃:blobs 行
// trash 后远端本无对象,gc 无事可做,产物由 verify 的无主清理收尾。
func (m *Manager) ClearCoverFile(fileID int64) error {
	err := m.deps.DB.WithTx(func(tx *sql.Tx) error {
		prev, err := m.deps.DB.ClearCover(tx, fileID)
		if err != nil {
			return err
		}
		return m.deps.DB.MarkBlobTrashTx(tx, []string{prev})
	})
	if err != nil {
		return err
	}
	m.emit("index:changed", map[string]any{"reason": "cover-clear", "fileID": fileID})
	return nil
}

// ClearFolderCover 清除目录封面,语义与 ClearCoverFile 逐句相同(持有式:
// 删引用行 + 旧 blob 同事务 trash,物理删除交给 gc;挂账中的同样可弃)。
func (m *Manager) ClearFolderCover(folderID int64) error {
	err := m.deps.DB.WithTx(func(tx *sql.Tx) error {
		prev, err := m.deps.DB.ClearFolderCover(tx, folderID)
		if err != nil {
			return err
		}
		return m.deps.DB.MarkBlobTrashTx(tx, []string{prev})
	})
	if err != nil {
		return err
	}
	m.emit("index:changed", map[string]any{"reason": "cover-clear", "folderID": folderID})
	return nil
}
