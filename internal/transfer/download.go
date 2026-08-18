package transfer

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"kist/internal/crypto"
	"kist/internal/errs"
)

// runDownload 单文件下载管线:GET 到临时文件 → 流式解密到 .part →
// 校验通过后原子落盘。失败/取消不留半截文件。
func (m *Manager) runDownload(j *job) error {
	ctx := j.ctx
	tr := j.tr
	f := j.file

	mk, ok := m.deps.MK()
	if !ok {
		return errsLocked()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	tmpDir := filepath.Join(tempRoot(), tr.ID)
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	// ---- 阶段一:GET blob 到临时文件 ----
	blobPath := filepath.Join(tmpDir, "blob")
	m.setPhase(tr, PhaseDownloading)
	if err := m.deps.Remote.GetBlob(ctx, f.BlobName, blobPath, func(got int64) {
		m.setProgress(tr, got)
	}); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// ---- 阶段二:流式解密到 .part ----
	final := uniqueLocalName(j.destDir, f.Name)
	partPath := filepath.Join(j.destDir, "."+final+".kistpart")
	success := false
	defer func() {
		if !success {
			os.Remove(partPath) // 失败/取消不留半截文件
		}
	}()

	in, err := os.Open(blobPath)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}

	m.setPhase(tr, PhaseDecrypting)
	br, err := crypto.NewBlobReader(in, st.Size(), mk)
	if err != nil {
		return errs.From(err) // WRONG_KEY / CORRUPT
	}
	part, err := os.OpenFile(partPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	buf := make([]byte, 1<<20)
	var delivered int64
	base := f.CipherSize // 下载阶段已累计的进度基准
	for {
		if err := ctx.Err(); err != nil { // 取消点:每 MiB
			part.Close()
			return err
		}
		n, rerr := br.Read(buf)
		if n > 0 {
			if _, werr := part.Write(buf[:n]); werr != nil {
				part.Close()
				return werr
			}
			delivered += int64(n)
			m.setProgress(tr, base+delivered)
		}
		if rerr == io.EOF {
			break // EOF 内含终检:长度、SHA-256 全通过才会到这里
		}
		if rerr != nil {
			part.Close()
			return errs.From(rerr)
		}
	}
	if err := part.Close(); err != nil {
		return err
	}

	// ---- 阶段三:原子落盘 ----
	if err := os.Rename(partPath, filepath.Join(j.destDir, final)); err != nil {
		return err
	}
	success = true
	m.emit("index:changed", map[string]any{"reason": "download", "fileID": f.ID})
	return nil
}

// uniqueLocalName 目标已存在时追加 "(1)"、"(2)"…(下载侧文件系统消解)。
func uniqueLocalName(dir, name string) string {
	if _, err := os.Stat(filepath.Join(dir, name)); os.IsNotExist(err) {
		return name
	}
	ext := ""
	if i := strings.LastIndex(name, "."); i > 0 {
		ext = name[i:]
	}
	base := strings.TrimSuffix(name, ext)
	for i := 1; i <= 9999; i++ {
		c := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(filepath.Join(dir, c)); os.IsNotExist(err) {
			return c
		}
	}
	return name
}

func errsLocked() error {
	return errs.New(errs.Locked, "未解锁:请先用正确的口令解锁")
}
