package transfer

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"kist/internal/config"
	"kist/internal/crypto"
	"kist/internal/index"
)

// runUpload 单文件上传管线:流式加密到临时文件 → 整文件 PUT → 写索引。
// 取消点:加密每个读取块、每次 PUT 请求(dav 层 ctx)、索引写入前。
func (m *Manager) runUpload(j *job) error {
	ctx := j.ctx
	tr := j.tr

	mk, ok := m.deps.MK()
	if !ok {
		return errsLocked()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// 每个传输独立的临时目录,结束即删
	tmpDir := filepath.Join(tempRoot(), tr.ID)
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	src, err := os.Open(j.srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	st, err := src.Stat()
	if err != nil {
		return err
	}
	if st.IsDir() {
		return fmt.Errorf("transfer: %s 是目录(应由 UploadPaths 展开)", j.srcPath)
	}

	// ---- 阶段一:流式加密(内存 ≈ 一个块)----
	blobPath := filepath.Join(tmpDir, "blob")
	out, err := os.Create(blobPath)
	if err != nil {
		return err
	}
	chunk := m.chunkBytes()
	bw, err := crypto.NewBlobWriter(out, mk, crypto.EncryptOptions{ChunkSize: chunk})
	if err != nil {
		out.Close()
		return err
	}
	m.setPhase(tr, PhaseEncrypting)
	m.setTotal(tr, st.Size()) // 先按明文计,加密完成后改为明文+密文

	buf := make([]byte, 1<<20)
	var done int64
	for {
		if err := ctx.Err(); err != nil { // 取消点:每 MiB
			out.Close()
			return err
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := bw.Write(buf[:n]); werr != nil {
				out.Close()
				return werr
			}
			done += int64(n)
			m.setProgress(tr, done)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			out.Close()
			return rerr
		}
	}
	if err := bw.Close(); err != nil {
		out.Close()
		return err
	}
	encryptedAt := time.Now().Unix()
	meta := bw.Meta()
	if err := out.Close(); err != nil {
		return err
	}

	cipherSt, err := os.Stat(blobPath)
	if err != nil {
		return err
	}
	cipherSize := cipherSt.Size()
	m.setTotal(tr, st.Size()+cipherSize) // 进度总数 = 加密字节 + 上传字节

	// ---- 阶段二:缩略图(仅图片;任何失败只忽略,绝不阻断上传)----
	var thumb *thumbData
	if td, terr := makeThumbnail(j.srcPath); terr == nil {
		thumb = &td
	} else if !errors.Is(terr, errNotImage) {
		// 是图片却生成失败:留痕供排查(TODO-07 静默黑洞);非图片属预期,静默跳过
		slog.Warn("缩略图生成失败,已忽略", "path", j.srcPath, "err", terr)
	}

	blobName := newHexID() // 两条路径共用:直接 PUT,或落出站箱待运(TODO-13)

	if j.deferred {
		return m.deferUpload(j, blobName, cipherSize, encryptedAt, meta, chunk, st, thumb, blobPath)
	}

	// ---- 阶段三:整文件 PUT(随机名,失败重试在 dav 层)----
	m.setPhase(tr, PhaseUploading)
	bf, err := os.Open(blobPath)
	if err != nil {
		return err
	}
	err = m.deps.Remote.PutBlob(ctx, blobName, bf, func(sent int64) {
		m.setProgress(tr, st.Size()+sent)
	})
	bf.Close()
	if err != nil {
		return err
	}

	// ---- 阶段四:写索引(同一事务:文件+缩略图+blob 登记+revision)。
	// 同名冲突在事务内消解——blob 名与文件名无关,并发上传也不会撞名;
	// 若此步失败而 PUT 已成功,blob 留在远端由孤儿清理处置。
	uuid := fmt.Sprintf("%x", meta.FileID[:])
	shaHex := fmt.Sprintf("%x", meta.PlainSHA[:])
	finalName := ""
	var fileID int64
	err = m.deps.DB.WithTx(func(tx *sql.Tx) error {
		name, err := m.deps.DB.UniqueFileName(tx, j.folderID, j.desiredName)
		if err != nil {
			return err
		}
		finalName = name
		fileID, err = m.deps.DB.InsertFile(tx, index.FileRow{
			UUID: uuid, FolderID: j.folderID, Name: name,
			Size: st.Size(), CipherSize: cipherSize,
			SHA256: shaHex, ChunkSize: int64(chunk), BlobName: blobName,
			ModifiedAt:  j.mtime,
			EncryptedAt: sql.NullInt64{Int64: encryptedAt, Valid: true},
			UploadedAt:  sql.NullInt64{Int64: time.Now().Unix(), Valid: true},
		})
		if err != nil {
			return err
		}
		if thumb != nil {
			if err := m.deps.DB.PutThumbnail(tx, fileID, thumb.data, thumb.w, thumb.h, thumb.mime); err != nil {
				return err
			}
		}
		return m.deps.DB.RegisterBlob(tx, blobName, "file", cipherSize)
	})
	if err != nil {
		// PUT 已成功而索引写入失败:远端留下无主 blob 待 gc 处置,必须留痕
		// (TODO-07 静默黑洞)
		slog.Warn("索引写入失败,远端 blob 已成孤儿", "blob", blobName, "name", j.desiredName, "err", err)
		return err
	}
	m.mu.Lock()
	tr.UUID = uuid
	tr.Name = finalName
	m.mu.Unlock()
	m.emit("index:changed", map[string]any{"reason": "upload", "fileID": fileID})
	return nil
}

// deferUpload 是 runUpload 的 defer 分支(TODO-13):只"加密 + 记账 + 产物入
// 出站箱",不发 PUT。索引即写 files.state='uploading'(v1 schema 预留)与
// blobs.state='pending';产物挪入 KIST_HOME/outbox——运输交给 push 或手工搬运,
// verify 收账。产物挪动失败时索引行已提交:留着 uploading 行,由 outbox
// list/verify 报告"产物缺失",用户可 discard 后重来,不会出现幽灵 ready。
func (m *Manager) deferUpload(j *job, blobName string, cipherSize int64, encryptedAt int64,
	meta crypto.Meta, chunk uint32, st os.FileInfo, thumb *thumbData, blobPath string) error {
	tr := j.tr
	uuid := fmt.Sprintf("%x", meta.FileID[:])
	shaHex := fmt.Sprintf("%x", meta.PlainSHA[:])
	finalName := ""
	var fileID int64
	err := m.deps.DB.WithTx(func(tx *sql.Tx) error {
		name, err := m.deps.DB.UniqueFileName(tx, j.folderID, j.desiredName)
		if err != nil {
			return err
		}
		finalName = name
		fileID, err = m.deps.DB.InsertFile(tx, index.FileRow{
			UUID: uuid, FolderID: j.folderID, Name: name,
			Size: st.Size(), CipherSize: cipherSize,
			SHA256: shaHex, ChunkSize: int64(chunk), BlobName: blobName,
			State:       "uploading",
			ModifiedAt:  j.mtime,
			EncryptedAt: sql.NullInt64{Int64: encryptedAt, Valid: true},
		})
		if err != nil {
			return err
		}
		if thumb != nil {
			if err := m.deps.DB.PutThumbnail(tx, fileID, thumb.data, thumb.w, thumb.h, thumb.mime); err != nil {
				return err
			}
		}
		return m.deps.DB.RegisterBlobPending(tx, blobName, "file", cipherSize)
	})
	if err != nil {
		return err
	}
	if err := moveArtifact(blobPath, config.OutboxDir(), blobName); err != nil {
		return err
	}
	m.mu.Lock()
	tr.UUID = uuid
	tr.Name = finalName
	m.mu.Unlock()
	m.setPhase(tr, PhaseDeferred)
	m.emit("index:changed", map[string]any{"reason": "defer", "fileID": fileID})
	return nil
}
