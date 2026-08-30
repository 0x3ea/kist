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

// PushPending 把待上传文件与封面(TODO-10)入队为 push 任务(产物缺失者
// 跳过,留给 verify/list 报告);返回入队数。
func (m *Manager) PushPending(ctx context.Context, files []index.FileRow, covers []index.CoverRow) int {
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
	for _, c := range covers {
		if _, err := os.Stat(OutboxArtifactPath(c.BlobName)); err != nil {
			continue
		}
		jctx, cancel := context.WithCancel(ctx)
		id := newHexID()
		j := &job{ctx: jctx, cancel: cancel, cover: &c}
		j.tr = &Transfer{ID: id, Kind: "push", Name: "封面", Phase: PhaseQueued,
			BytesTotal: c.Size, StartedAt: time.Now().Unix()}
		m.add(j)
		queued++
	}
	return queued
}

// pushItem 归一 push 任务的账目两侧差异:文件账与封面账(TODO-10)只在
// 远端命名空间、期望密文大小与 emit 的 fileID 上不同。
type pushItem struct {
	blobName string
	size     int64
	isCover  bool
	fileID   int64 // index:changed 事件用
}

// runPush 单个产物重传:PUT(按 kind 分命名空间)→ MarkUploaded → 删产物。
// 失败按 Deps.PushFailDiscard 政策处置——keep(默认)挂账留证可再战,
// discard 整笔回滚(索引行 + 产物一起删,干净失败;文件账连其封面账一起)。
func (m *Manager) runPush(j *job) error {
	ctx, tr := j.ctx, j.tr
	it := pushItem{isCover: j.cover != nil}
	if it.isCover {
		it.blobName, it.size, it.fileID = j.cover.BlobName, j.cover.Size, j.cover.FileID
	} else {
		it.blobName, it.size, it.fileID = j.file.BlobName, j.file.CipherSize, j.file.ID
	}
	artifact := OutboxArtifactPath(it.blobName)
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
	put := func() error {
		rs := m.remoteSnapshot()
		if it.isCover {
			return rs.PutCoverBlob(ctx, it.blobName, bf, func(sent int64) { m.setProgress(tr, sent) })
		}
		return rs.PutBlob(ctx, it.blobName, bf, func(sent int64) { m.setProgress(tr, sent) })
	}
	err = put()
	if err != nil {
		if m.discardOnFail() {
			// discard 档:干净失败,不留悬置账目(TODO-13 两档政策);
			// 文件账会把封面账一并放弃(返回值即被放弃的封面 blob 名)
			coverBlobs, derr := m.deps.DB.DiscardPending(it.blobName)
			if derr != nil {
				slog.Warn("discard 档回滚索引失败", "blob", it.blobName, "err", derr)
			}
			for _, name := range append([]string{it.blobName}, coverBlobs...) {
				if rerr := os.Remove(OutboxArtifactPath(name)); rerr != nil && !os.IsNotExist(rerr) {
					slog.Warn("discard 档清理产物失败", "blob", name, "err", rerr)
				}
			}
			slog.Warn("push 失败(discard 档):已回滚记账并删除产物,可重新 put --defer 后手工搬运",
				"blob", it.blobName, "err", err)
		} else {
			slog.Warn("push 失败(keep 档):索引与产物保留,可重试 push / 手工搬运 / discard",
				"blob", it.blobName, "err", err)
		}
		return err
	}
	if err := m.deps.DB.MarkUploaded(it.blobName, time.Now().Unix()); err != nil {
		return err // 远端已有但没收上账:verify 可补收
	}
	if err := os.Remove(artifact); err != nil && !os.IsNotExist(err) {
		slog.Warn("清理出站箱产物失败", "blob", it.blobName, "err", err)
	}
	m.emit("index:changed", map[string]any{"reason": "push", "fileID": it.fileID})
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
// 挂账集 = 文件 ∪ 封面(TODO-10):封面产物若不进挂账集,会被下面的
// 无主清理当成无主文件误删——封面与文件同进退。
func RunOutboxVerify(ctx context.Context, store *remote.Store, db *index.DB) ([]VerifyResult, error) {
	files, err := db.ListUploading()
	if err != nil {
		return nil, err
	}
	covers, err := db.ListUploadingCovers()
	if err != nil {
		return nil, err
	}
	pending := map[string]bool{}
	var out []VerifyResult
	for _, f := range files {
		pending[f.BlobName] = true
		out, err = verifyOne(ctx, store, db, f.BlobName, f.CipherSize, false, out)
		if err != nil {
			return out, err
		}
	}
	for _, c := range covers {
		pending[c.BlobName] = true
		out, err = verifyOne(ctx, store, db, c.BlobName, c.Size, true, out)
		if err != nil {
			return out, err
		}
	}
	// 无主产物清理:文件在、对应的活跃挂账行不在(rm/discard 后的遗留)
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

// verifyOne 对单个挂账对象 Probe 收账;封面与文件的差异只剩探测的命名空间。
func verifyOne(ctx context.Context, store *remote.Store, db *index.DB, blobName string, want int64, isCover bool, out []VerifyResult) ([]VerifyResult, error) {
	artifact := OutboxArtifactPath(blobName)
	if _, err := os.Stat(artifact); err != nil {
		return append(out, VerifyResult{blobName, "no-artifact",
			"索引有待上传记录但本地产物缺失:discard 后重新 put --defer"}), nil
	}
	var found bool
	var size int64
	var err error
	if isCover {
		found, size, err = store.ProbeCoverBlob(ctx, blobName)
	} else {
		found, size, err = store.ProbeBlob(ctx, blobName)
	}
	if err != nil {
		return out, err
	}
	switch {
	case !found:
		out = append(out, VerifyResult{blobName, "missing",
			"远端尚无此对象:outbox push 或手工搬运后再 verify"})
	case size >= 0 && size != want:
		out = append(out, VerifyResult{blobName, "size-mismatch",
			"远端大小不符(疑似传输不完整):清掉远端对象后重传"})
	default:
		if err := db.MarkUploaded(blobName, time.Now().Unix()); err != nil {
			return out, err
		}
		if err := os.Remove(artifact); err != nil && !os.IsNotExist(err) {
			slog.Warn("verify 清理产物失败", "blob", blobName, "err", err)
		}
		detail := "已收账转 ready"
		if size < 0 {
			detail = "已收账转 ready(服务器未报大小,仅按存在收账)"
		}
		out = append(out, VerifyResult{blobName, "ready", detail})
	}
	return out, nil
}

// OutboxDiscard 回滚一笔待上传:删索引行与产物(仅 uploading 可放弃,
// 已就绪文件会被索引层拒绝)。文件账会连其封面账一并放弃(TODO-10),
// 返回的封面产物一并删除。
func OutboxDiscard(db *index.DB, blobName string) error {
	coverBlobs, err := db.DiscardPending(blobName)
	if err != nil {
		return err
	}
	if err := os.Remove(OutboxArtifactPath(blobName)); err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, name := range coverBlobs {
		if err := os.Remove(OutboxArtifactPath(name)); err != nil && !os.IsNotExist(err) {
			return err
		}
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
