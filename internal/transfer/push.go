package transfer

// 出站箱(TODO-13):put --defer 留下的加密产物,由 kist 择机重传(push)、
// 用户手工搬运后收账(verify)或整笔回滚(discard)。push 是纯密文搬运:
// 不解密、不加密,无需解锁口令。

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"kist/internal/config"
	"kist/internal/index"
	"kist/internal/remote"
)

// OutboxArtifactPath 出站箱内某个 blob 产物的路径。
func OutboxArtifactPath(blobName string) string {
	return filepath.Join(config.OutboxDir(), blobName)
}

// PushPending 把待上传文件入队为 push 任务(产物缺失者跳过,留给
// verify/list 报告);返回入队数。
func (m *Manager) PushPending(ctx context.Context, files []index.FileRow) int {
	queued := 0
	for _, f := range files {
		if _, err := os.Stat(OutboxArtifactPath(f.BlobName)); err != nil {
			continue
		}
		jctx, cancel := context.WithCancel(ctx)
		id := newHexID()
		j := &job{ctx: jctx, cancel: cancel, file: f}
		j.tr = &Transfer{ID: id, Kind: "push", Name: f.Name, UUID: f.UUID, Phase: PhaseQueued,
			BytesTotal: f.CipherSize, StartedAt: time.Now().Unix()}
		m.add(j)
		queued++
	}
	return queued
}

// runPush 单个产物重传:PUT → MarkUploaded → 删产物。失败按
// Deps.PushFailDiscard 政策处置——keep(默认)挂账留证可再战,discard
// 整笔回滚(索引行 + 产物一起删,干净失败)。
func (m *Manager) runPush(j *job) error {
	ctx, tr, f := j.ctx, j.tr, j.file
	artifact := OutboxArtifactPath(f.BlobName)
	st, err := os.Stat(artifact)
	if err != nil {
		return err // 产物缺失:无法重传,行留给 discard/重新 defer
	}
	bf, err := os.Open(artifact)
	if err != nil {
		return err
	}
	defer bf.Close()
	m.setPhase(tr, PhaseUploading)
	m.setTotal(tr, st.Size())
	err = m.remoteSnapshot().PutBlob(ctx, f.BlobName, bf, func(sent int64) {
		m.setProgress(tr, sent)
	})
	if err != nil {
		if m.discardOnFail() {
			// discard 档:干净失败,不留悬置账目(TODO-13 两档政策)
			if derr := m.deps.DB.DiscardPending(f.BlobName); derr != nil {
				slog.Warn("discard 档回滚索引失败", "blob", f.BlobName, "err", derr)
			}
			if rerr := os.Remove(artifact); rerr != nil && !os.IsNotExist(rerr) {
				slog.Warn("discard 档清理产物失败", "blob", f.BlobName, "err", rerr)
			}
			slog.Warn("push 失败(discard 档):已回滚记账并删除产物,可重新 put --defer 后手工搬运",
				"blob", f.BlobName, "err", err)
		} else {
			slog.Warn("push 失败(keep 档):索引与产物保留,可重试 push / 手工搬运 / discard",
				"blob", f.BlobName, "err", err)
		}
		return err
	}
	if err := m.deps.DB.MarkUploaded(f.BlobName, time.Now().Unix()); err != nil {
		return err // 远端已有但没收上账:verify 可补收
	}
	if err := os.Remove(artifact); err != nil && !os.IsNotExist(err) {
		slog.Warn("清理出站箱产物失败", "blob", f.BlobName, "err", err)
	}
	m.emit("index:changed", map[string]any{"reason": "push", "fileID": f.ID})
	return nil
}

func (m *Manager) discardOnFail() bool {
	return m.deps.PushFailDiscard != nil && m.deps.PushFailDiscard()
}

// VerifyResult 是 outbox verify 对单个对象的处置结果。
type VerifyResult struct {
	Blob   string
	Action string // ready | missing | size-mismatch | no-artifact | unowned-cleaned
	Detail string
}

// RunOutboxVerify 对全部待上传对象收账(TODO-11 的 O(1) Probe,万级也廉价):
// 远端已有且大小相符 → 转 ready 并删本地产物;大小不符 → 拒绝收账(疑似
// 手工传输不完整);顺带清理无主产物(索引行已删/软删的遗留文件)。
func RunOutboxVerify(ctx context.Context, store *remote.Store, db *index.DB) ([]VerifyResult, error) {
	files, err := db.ListUploading()
	if err != nil {
		return nil, err
	}
	pending := map[string]bool{}
	var out []VerifyResult
	for _, f := range files {
		pending[f.BlobName] = true
		artifact := OutboxArtifactPath(f.BlobName)
		if _, err := os.Stat(artifact); err != nil {
			out = append(out, VerifyResult{f.BlobName, "no-artifact",
				"索引有待上传记录但本地产物缺失:discard 后重新 put --defer"})
			continue
		}
		found, size, err := store.ProbeBlob(ctx, f.BlobName)
		if err != nil {
			return out, err
		}
		switch {
		case !found:
			out = append(out, VerifyResult{f.BlobName, "missing",
				"远端尚无此对象:outbox push 或手工搬运后再 verify"})
		case size >= 0 && size != f.CipherSize:
			out = append(out, VerifyResult{f.BlobName, "size-mismatch",
				"远端大小不符(疑似传输不完整):清掉远端对象后重传"})
		default:
			if err := db.MarkUploaded(f.BlobName, time.Now().Unix()); err != nil {
				return out, err
			}
			if err := os.Remove(artifact); err != nil && !os.IsNotExist(err) {
				slog.Warn("verify 清理产物失败", "blob", f.BlobName, "err", err)
			}
			detail := "已收账转 ready"
			if size < 0 {
				detail = "已收账转 ready(服务器未报大小,仅按存在收账)"
			}
			out = append(out, VerifyResult{f.BlobName, "ready", detail})
		}
	}
	// 无主产物清理:文件在、对应的活跃 uploading 行不在(rm/discard 后的遗留)
	ents, err := os.ReadDir(config.OutboxDir())
	if err != nil && !os.IsNotExist(err) {
		return out, err
	}
	for _, e := range ents {
		if e.IsDir() || pending[e.Name()] {
			continue
		}
		if err := os.Remove(OutboxArtifactPath(e.Name())); err != nil {
			slog.Warn("清理无主产物失败", "name", e.Name(), "err", err)
			continue
		}
		out = append(out, VerifyResult{e.Name(), "unowned-cleaned", "对应索引记录已不存在,产物已清理"})
	}
	slog.Info("出站箱 verify 完成", "对象数", len(out))
	return out, nil
}

// OutboxDiscard 回滚一笔待上传:删索引行与产物(仅 uploading 可放弃,
// 已就绪文件会被索引层拒绝)。
func OutboxDiscard(db *index.DB, blobName string) error {
	if err := db.DiscardPending(blobName); err != nil {
		return err
	}
	if err := os.Remove(OutboxArtifactPath(blobName)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// moveArtifact 把加密产物从临时目录挪进出站箱;/tmp 常见为 tmpfs,
// 跨设备 rename 会失败,回退为复制后删源。
func moveArtifact(src, dir, name string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	dst := filepath.Join(dir, name)
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Remove(src)
}
