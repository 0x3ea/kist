package e2e

// 出站箱(TODO-13)全链路:defer 记账 → 手工搬运/verify 收账、push 成功、
// push 失败两档政策、discard、无主产物清理与 gc 交叉。

import (
	"context"
	"os"
	"testing"

	"kist/internal/crypto"
	"kist/internal/index"
	"kist/internal/transfer"
)

// lastTr 取快照里最近一次传输:同一 Manager 会保留全部历史
// (defer 的终态永远是 deferred),断言新任务必须看最后一条。
func lastTr(t *testing.T, trs []transfer.Transfer) transfer.Transfer {
	t.Helper()
	if len(trs) == 0 {
		t.Fatal("无传输记录")
	}
	return trs[len(trs)-1]
}

// deferOne 对单个文件走 put --defer 等价路径,断言记账与产物就位,返回文件行与源路径。
func deferOne(t *testing.T, e *env, name string, size int) (index.FileRow, string) {
	t.Helper()
	src := makeFile(t, t.TempDir(), name, size)
	n, err := e.m.DeferPaths(context.Background(), []string{src}, 1)
	if err != nil || n != 1 {
		t.Fatalf("DeferPaths: n=%d err=%v", n, err)
	}
	trs := waitIdle(t, e.m)
	if trs[0].Phase != transfer.PhaseDeferred {
		t.Fatalf("defer 终态 = %s(%s),期望 deferred", trs[0].Phase, trs[0].Err)
	}
	hits, _ := e.db.Search(name, 5)
	if len(hits) != 1 {
		t.Fatalf("defer 后应可搜索到 %s: %+v", name, hits)
	}
	f, err := e.db.GetFile(hits[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if f.State != "uploading" {
		t.Fatalf("state = %s,期望 uploading", f.State)
	}
	if f.UploadedAt.Valid {
		t.Fatal("uploading 行不应有 uploaded_at")
	}
	if _, err := os.Stat(transfer.OutboxArtifactPath(f.BlobName)); err != nil {
		t.Fatalf("产物应落在出站箱: %v", err)
	}
	return f, src
}

// TestOutboxDeferThenVerify 手工搬运(直接 PUT 产物)→ verify 收账 → 下载可用。
func TestOutboxDeferThenVerify(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	f, src := deferOne(t, e, "待传.bin", 3000)

	// 待上传对象必须被明确拒绝,而不是 GET 404 疑云
	if _, err := e.m.DownloadTo(ctx, []int64{f.ID}, t.TempDir()); err != nil {
		t.Fatalf("入队不应报错: %v", err)
	}
	if last := lastTr(t, waitIdle(t, e.m)); last.Phase != transfer.PhaseError {
		t.Fatalf("待上传对象下载应失败,终态 %s", last.Phase)
	}

	// 模拟手工搬运:把产物原样 PUT 到远端
	af, err := os.Open(transfer.OutboxArtifactPath(f.BlobName))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.PutBlob(ctx, f.BlobName, af, nil); err != nil {
		t.Fatal(err)
	}
	af.Close()

	res, err := transfer.RunOutboxVerify(ctx, e.store, e.db)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Action != "ready" {
		t.Fatalf("verify 应收账: %+v", res)
	}
	got, _ := e.db.GetFile(f.ID)
	if got.State != "ready" || !got.UploadedAt.Valid {
		t.Fatalf("收账后 state=%s uploadedAt=%v", got.State, got.UploadedAt.Valid)
	}
	if _, err := os.Stat(transfer.OutboxArtifactPath(f.BlobName)); !os.IsNotExist(err) {
		t.Fatal("收账后本地产物应删除")
	}

	// 收账后下载解密,内容与源一致
	outDir := t.TempDir()
	if _, err := e.m.DownloadTo(ctx, []int64{f.ID}, outDir); err != nil {
		t.Fatal(err)
	}
	if last := lastTr(t, waitIdle(t, e.m)); last.Phase != transfer.PhaseDone {
		t.Fatalf("收账后下载终态 %s(%s)", last.Phase, last.Err)
	}
	if fileSHA(t, outDir+"/待传.bin") != fileSHA(t, src) {
		t.Fatal("收账后下载内容与源不一致")
	}

	// 二次 verify 幂等:无事可做
	res2, err := transfer.RunOutboxVerify(ctx, e.store, e.db)
	if err != nil || len(res2) != 0 {
		t.Fatalf("空出站箱 verify 应无结果: %+v %v", res2, err)
	}
}

// TestOutboxVerifySizeMismatch 手工传输不完整(远端大小不符)必须拒绝收账。
func TestOutboxVerifySizeMismatch(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	f, _ := deferOne(t, e, "半截.bin", 100)

	bad, err := os.CreateTemp("", "kist-bad-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.Write([]byte("short")); err != nil { // 5 字节 ≠ cipherSize
		t.Fatal(err)
	}
	bad.Close()
	bf, err := os.Open(bad.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer bf.Close()
	if err := e.store.PutBlob(ctx, f.BlobName, bf, nil); err != nil {
		t.Fatal(err)
	}

	res, err := transfer.RunOutboxVerify(ctx, e.store, e.db)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Action != "size-mismatch" {
		t.Fatalf("应判 size-mismatch: %+v", res)
	}
	got, _ := e.db.GetFile(f.ID)
	if got.State != "uploading" {
		t.Fatal("大小不符不得收账")
	}
	if _, err := os.Stat(transfer.OutboxArtifactPath(f.BlobName)); err != nil {
		t.Fatal("产物应保留待重传")
	}
}

// TestOutboxPushSuccess kist 自己重传:PUT → 收账 → 产物删除 → 下载可用。
func TestOutboxPushSuccess(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	f, src := deferOne(t, e, "重传.bin", 5000)

	if n := e.m.PushPending(ctx, []index.FileRow{f}, nil); n != 1 {
		t.Fatalf("push 入队 %d,期望 1", n)
	}
	if last := lastTr(t, waitIdle(t, e.m)); last.Phase != transfer.PhaseDone {
		t.Fatalf("push 终态 %s(%s)", last.Phase, last.Err)
	}

	got, _ := e.db.GetFile(f.ID)
	if got.State != "ready" {
		t.Fatalf("push 后 state=%s", got.State)
	}
	blobs, err := e.store.ListBlobs(ctx)
	if err != nil || len(blobs) != 1 || blobs[0] != f.BlobName {
		t.Fatalf("远端应有该 blob: %v %v", blobs, err)
	}
	if _, err := os.Stat(transfer.OutboxArtifactPath(f.BlobName)); !os.IsNotExist(err) {
		t.Fatal("push 成功后产物应删除")
	}
	outDir := t.TempDir()
	if _, err := e.m.DownloadTo(ctx, []int64{f.ID}, outDir); err != nil {
		t.Fatal(err)
	}
	if last := lastTr(t, waitIdle(t, e.m)); last.Phase != transfer.PhaseDone {
		t.Fatalf("下载终态 %s(%s)", last.Phase, last.Err)
	}
	if fileSHA(t, outDir+"/重传.bin") != fileSHA(t, src) {
		t.Fatal("push 后下载内容与源不一致")
	}
}

// TestOutboxPushFailKeep 默认 keep 档:push 失败后索引行与产物都保留。
func TestOutboxPushFailKeep(t *testing.T) {
	e := newEnv(t)
	f, _ := deferOne(t, e, "挂账.bin", 100)

	e.srv.Close() // 模拟网络彻底不可用
	if n := e.m.PushPending(context.Background(), []index.FileRow{f}, nil); n != 1 {
		t.Fatalf("push 入队 %d,期望 1", n)
	}
	if last := lastTr(t, waitIdle(t, e.m)); last.Phase != transfer.PhaseError {
		t.Fatalf("终态 %s,期望 error", last.Phase)
	}
	got, _ := e.db.GetFile(f.ID)
	if got.State != "uploading" {
		t.Fatal("keep 档:行应保持 uploading")
	}
	if _, err := os.Stat(transfer.OutboxArtifactPath(f.BlobName)); err != nil {
		t.Fatal("keep 档:产物应保留")
	}
}

// TestOutboxPushFailDiscard discard 档:push 失败后整笔回滚(行 + 产物)。
func TestOutboxPushFailDiscard(t *testing.T) {
	e := newEnv(t)
	f, _ := deferOne(t, e, "回滚.bin", 100)
	e.srv.Close()

	m2 := transfer.NewManager(transfer.Deps{
		Remote: e.store, DB: e.db,
		MK:              func() (crypto.MasterKey, bool) { return crypto.MasterKey{}, false },
		PushFailDiscard: func() bool { return true },
	})
	if n := m2.PushPending(context.Background(), []index.FileRow{f}, nil); n != 1 {
		t.Fatalf("push 入队 %d,期望 1", n)
	}
	trs := waitIdle(t, m2)
	if trs[0].Phase != transfer.PhaseError {
		t.Fatalf("终态 %s,期望 error", trs[0].Phase)
	}
	if _, err := e.db.GetFile(f.ID); err == nil {
		t.Fatal("discard 档:索引行应已删除")
	}
	if _, err := os.Stat(transfer.OutboxArtifactPath(f.BlobName)); !os.IsNotExist(err) {
		t.Fatal("discard 档:产物应已删除")
	}
}

// TestOutboxDiscardAndUnowned discard 守卫、rm 后的无主产物清理、gc 交叉。
func TestOutboxDiscardAndUnowned(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// discard 一笔待上传:行 + 产物一起消失
	f1, _ := deferOne(t, e, "放弃.bin", 100)
	if err := transfer.OutboxDiscard(e.db, f1.BlobName); err != nil {
		t.Fatalf("discard: %v", err)
	}
	if _, err := e.db.GetFile(f1.ID); err == nil {
		t.Fatal("discard 后行应删除")
	}
	if _, err := os.Stat(transfer.OutboxArtifactPath(f1.BlobName)); !os.IsNotExist(err) {
		t.Fatal("discard 后产物应删除")
	}

	// 已就绪文件必须被拒绝——绝不能经 outbox 通道无声消失
	src := makeFile(t, t.TempDir(), "就绪.bin", 50)
	if _, err := e.m.UploadPaths(ctx, []string{src}, 1); err != nil {
		t.Fatal(err)
	}
	if last := lastTr(t, waitIdle(t, e.m)); last.Phase != transfer.PhaseDone {
		t.Fatalf("普通上传终态 %s(%s)", last.Phase, last.Err)
	}
	hits, _ := e.db.Search("就绪.bin", 5)
	f2, _ := e.db.GetFile(hits[0].ID)
	if err := transfer.OutboxDiscard(e.db, f2.BlobName); err == nil {
		t.Fatal("放弃已就绪文件必须被拒绝")
	}

	// rm 掉 uploading 文件后:产物成无主,verify 清理;gc 不误报孤儿
	f3, _ := deferOne(t, e, "软删待传.bin", 100)
	if err := e.db.SoftDeleteFiles([]int64{f3.ID}); err != nil {
		t.Fatal(err)
	}
	if err := e.db.MarkBlobTrash([]string{f3.BlobName}); err != nil {
		t.Fatal(err)
	}
	res, err := transfer.RunOutboxVerify(ctx, e.store, e.db)
	if err != nil {
		t.Fatal(err)
	}
	foundClean := false
	for _, r := range res {
		if r.Blob == f3.BlobName && r.Action == "unowned-cleaned" {
			foundClean = true
		}
	}
	if !foundClean {
		t.Fatalf("无主产物应被清理: %+v", res)
	}
	if _, err := os.Stat(transfer.OutboxArtifactPath(f3.BlobName)); !os.IsNotExist(err) {
		t.Fatal("无主产物应已删除")
	}
	gcRes, err := transfer.RunGC(ctx, e.store, e.db, true)
	if err != nil || len(gcRes.Orphans) != 0 {
		t.Fatalf("gc 不应误报孤儿: %+v %v", gcRes.Orphans, err)
	}
}
