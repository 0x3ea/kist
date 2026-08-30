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

// ImportCover 把 GUI 导入的封面出库(用户编辑,与 SetNote 同级计 revision):
// 加密 → PUT covers 命名空间 → 同事务写引用(custom/ready,旧引用 blob 同
// 事务 trash)。远端不可达时回退出站箱(引用 uploading 不可见、产物待
// push/verify 收账),返回 deferred=true 供壳层提示——用户内容绝不静默丢弃。
// f 是封面所属文件行(须未软删,调用方校验)。
func (m *Manager) ImportCover(ctx context.Context, f index.FileRow, td ThumbData) (deferred bool, err error) {
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

	newRow := index.CoverRow{
		FileID: f.ID, BlobName: cover.name, Size: cover.size,
		Width: td.W, Height: td.H, Mime: td.Mime,
		Source: index.CoverCustom, CreatedAt: time.Now().Unix(),
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
			newRow.State = index.CoverReady
			prev, err := m.deps.DB.PutCover(tx, newRow)
			if err != nil {
				return err
			}
			if err := m.deps.DB.MarkBlobTrashTx(tx, []string{prev}); err != nil {
				return err
			}
			return m.deps.DB.RegisterBlob(tx, cover.name, "cover", cover.size)
		}); err != nil {
			// 引用写入失败:封面 blob 已成孤儿,gc 处置(与文件上传同款语义)
			slog.Warn("封面引用写入失败,远端 blob 已成孤儿", "cover", cover.name, "file", f.Name, "err", err)
			return false, err
		}
		return false, nil
	}

	// 远端不可达:回退出站箱。引用 uploading(不可见),产物待运,
	// push/verify 收账翻 ready——弱网用户的自定义封面不因一次断网丢失。
	if err := m.deps.DB.WithTx(func(tx *sql.Tx) error {
		newRow.State = index.CoverUploading
		prev, err := m.deps.DB.PutCover(tx, newRow)
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
	m.emit("index:changed", map[string]any{"reason": "cover-defer", "fileID": f.ID})
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
