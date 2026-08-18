package transfer

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

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
	}

	// ---- 阶段三:整文件 PUT(随机名,失败重试在 dav 层)----
	blobName := newHexID()
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
		return err
	}
	m.mu.Lock()
	tr.UUID = uuid
	tr.Name = finalName
	m.mu.Unlock()
	m.emit("index:changed", map[string]any{"reason": "upload", "fileID": fileID})
	return nil
}
