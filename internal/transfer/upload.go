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

// runUpload 单任务上传管线:流式加密到临时文件 → 整文件 PUT → 写索引。
// 源有两种(TODO-15):普通文件;目录打包任务——zip 流直挂 BlobWriter,
// 单遍流式,不落中间 zip。取消点:加密每个读取块、每次 PUT 请求(dav
// 层 ctx)、索引写入前。
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

	// ---- 阶段一:流式加密(内存 ≈ 一个块)----
	blobPath := filepath.Join(tmpDir, "blob")
	out, err := os.Create(blobPath)
	if err != nil {
		return err
	}
	chunk := m.chunkBytes()
	bw, err := crypto.NewBlobWriter(out, mk, crypto.EncryptOptions{ChunkSize: chunk, NoPadding: m.noPad()})
	if err != nil {
		out.Close()
		return err
	}
	m.setPhase(tr, PhaseEncrypting)

	// pack 的产物统计:条目数/原始字节进 user_meta,词法序第一页做封面
	var packFirstImage string
	var packEntries int
	var packOrigBytes int64
	singleSize := int64(0)

	if j.pack {
		m.setTotal(tr, j.sizeHint) // 先按源文件总字节预估,Close 后改为准确值
		packEntries, packOrigBytes, packFirstImage, err = writePackZip(ctx, bw, j.srcPath, func(done int64) {
			m.setProgress(tr, done)
		})
		if err != nil {
			out.Close()
			return err
		}
	} else {
		src, err := os.Open(j.srcPath)
		if err != nil {
			out.Close()
			return err
		}
		st, err := src.Stat()
		if err != nil {
			src.Close()
			out.Close()
			return err
		}
		if st.IsDir() {
			src.Close()
			out.Close()
			return fmt.Errorf("transfer: %s 是目录(应由 UploadPaths 展开)", j.srcPath)
		}
		singleSize = st.Size()
		m.setTotal(tr, singleSize) // 先按明文计,加密完成后改为明文+密文

		buf := make([]byte, 1<<20)
		var done int64
		for {
			if err := ctx.Err(); err != nil { // 取消点:每 MiB
				src.Close()
				out.Close()
				return err
			}
			n, rerr := src.Read(buf)
			if n > 0 {
				if _, werr := bw.Write(buf[:n]); werr != nil {
					src.Close()
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
				src.Close()
				out.Close()
				return rerr
			}
		}
		src.Close()
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
	m.setTotal(tr, int64(meta.OrigSize)+cipherSize) // 进度总数 = 加密字节 + 上传字节

	// ---- 阶段二:缩略图(仅图片;任何失败只忽略,绝不阻断上传)----
	// pack 用词法序第一页做封面(TODO-15);无图的 pack 不生成
	thumbSrc := j.srcPath
	if j.pack {
		thumbSrc = packFirstImage
	}
	var thumb *ThumbData
	if thumbSrc != "" {
		if td, terr := MakeThumbnail(thumbSrc); terr == nil {
			thumb = &td
		} else if !errors.Is(terr, errNotImage) {
			// 是图片却生成失败:留痕供排查(TODO-07 静默黑洞);非图片属预期,静默跳过
			slog.Warn("缩略图生成失败,已忽略", "path", thumbSrc, "err", terr)
		}
	}

	blobName := newHexID() // 两条路径共用:直接 PUT,或落出站箱待运(TODO-13)

	// 索引行:pack 的 size 记量化后明文区总长(显示值;真实 origSize 在
	// sealedMeta,不得用索引 size 推明文长度——TODO-15 坑记录)
	row := index.FileRow{
		UUID:        fmt.Sprintf("%x", meta.FileID[:]),
		FolderID:    j.folderID,
		Size:        singleSize,
		CipherSize:  cipherSize,
		SHA256:      fmt.Sprintf("%x", meta.PlainSHA[:]),
		ChunkSize:   int64(chunk),
		BlobName:    blobName,
		ModifiedAt:  j.mtime,
		EncryptedAt: sql.NullInt64{Int64: encryptedAt, Valid: true},
	}
	if j.pack {
		row.Pack = true
		row.Size = int64(bw.PlainTotal())
		row.UserMeta = sql.NullString{String: packUserMeta(packOrigBytes, packEntries), Valid: true}
	}

	if j.deferred {
		return m.deferUpload(j, row, thumb, blobPath)
	}

	// ---- 阶段三:整文件 PUT(随机名,失败重试在 dav 层)----
	m.setPhase(tr, PhaseUploading)
	bf, err := os.Open(blobPath)
	if err != nil {
		return err
	}
	err = m.remoteSnapshot().PutBlob(ctx, blobName, bf, func(sent int64) {
		m.setProgress(tr, int64(meta.OrigSize)+sent)
	})
	bf.Close()
	if err != nil {
		return err
	}

	// ---- 阶段四:写索引(同一事务:文件+缩略图+blob 登记+revision)。
	// 同名冲突在事务内消解——blob 名与文件名无关,并发上传也不会撞名;
	// 若此步失败而 PUT 已成功,blob 留在远端由孤儿清理处置。
	finalName := ""
	var fileID int64
	err = m.deps.DB.WithTx(func(tx *sql.Tx) error {
		name, err := m.deps.DB.UniqueFileName(tx, j.folderID, j.desiredName)
		if err != nil {
			return err
		}
		finalName = name
		row.Name = name
		row.UploadedAt = sql.NullInt64{Int64: time.Now().Unix(), Valid: true}
		fileID, err = m.deps.DB.InsertFile(tx, row)
		if err != nil {
			return err
		}
		if thumb != nil {
			if err := m.deps.DB.PutThumbnail(tx, fileID, thumb.Data, thumb.W, thumb.H, thumb.Mime); err != nil {
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
	tr.UUID = row.UUID
	tr.Name = finalName
	m.mu.Unlock()
	m.emit("index:changed", map[string]any{"reason": "upload", "fileID": fileID})
	return nil
}

// deferUpload 是 runUpload 的 defer 分支(TODO-13):只"加密 + 记账 + 产物入
// 出站箱",不发 PUT。索引即写 files.state='uploading' 与 blobs.state=
// 'pending';产物挪入 KIST_HOME/outbox——运输交给 push 或手工搬运,
// verify 收账。产物挪动失败时索引行已提交:留着 uploading 行,由 outbox
// list/verify 报告"产物缺失",用户可 discard 后重来,不会出现幽灵 ready。
func (m *Manager) deferUpload(j *job, row index.FileRow, thumb *ThumbData, blobPath string) error {
	tr := j.tr
	finalName := ""
	var fileID int64
	err := m.deps.DB.WithTx(func(tx *sql.Tx) error {
		name, err := m.deps.DB.UniqueFileName(tx, j.folderID, j.desiredName)
		if err != nil {
			return err
		}
		finalName = name
		row.Name = name
		row.State = "uploading"
		fileID, err = m.deps.DB.InsertFile(tx, row)
		if err != nil {
			return err
		}
		if thumb != nil {
			if err := m.deps.DB.PutThumbnail(tx, fileID, thumb.Data, thumb.W, thumb.H, thumb.Mime); err != nil {
				return err
			}
		}
		return m.deps.DB.RegisterBlobPending(tx, row.BlobName, "file", row.CipherSize)
	})
	if err != nil {
		return err
	}
	if err := moveArtifact(blobPath, config.OutboxDir(), row.BlobName); err != nil {
		return err
	}
	m.mu.Lock()
	tr.UUID = row.UUID
	tr.Name = finalName
	m.mu.Unlock()
	m.setPhase(tr, PhaseDeferred)
	m.emit("index:changed", map[string]any{"reason": "defer", "fileID": fileID})
	return nil
}
