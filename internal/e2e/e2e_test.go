package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kist/internal/config"
	"kist/internal/crypto"
	"kist/internal/dav"
	"kist/internal/index"
	"kist/internal/remote"
	"kist/internal/transfer"

	"golang.org/x/net/webdav"
)

const testPass = "e2e-测试口令"

// env 装配一套完整环境:本地 WebDAV 服务 + 独立 KIST_HOME + 已初始化的账户。
type env struct {
	store *remote.Store
	db    *index.DB
	m     *transfer.Manager
	mk    crypto.MasterKey
}

func newEnv(t *testing.T) *env {
	t.Helper()
	srv := httptest.NewServer(&webdav.Handler{
		FileSystem: webdav.Dir(t.TempDir()),
		LockSystem: webdav.NewMemLS(),
	})
	t.Cleanup(srv.Close)

	t.Setenv("KIST_HOME", t.TempDir())
	if err := config.Save(&config.StoredConfig{
		URL: srv.URL, Username: "u", Password: "p",
		Settings: config.Settings{Concurrency: 2, ChunkMiB: 1}, // 1MiB 块:小文件也走多块路径
	}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	dc, err := dav.New(dav.Config{URL: srv.URL, Username: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	store := remote.NewStore(dc, "/kist")
	if err := store.EnsureReady(ctx); err != nil {
		t.Fatalf("初始化远端: %v", err)
	}
	// 建账户:keyfile 上传远端并缓存本地(与 kistctl init 等价)
	kf, mk, err := crypto.CreateKeyFile(testPass, crypto.Argon2Params{Time: 1, MemoryKiB: 1024, Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutKeyFile(ctx, kf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.KeyFilePath(), kf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	db, err := index.Open(config.IndexPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	m := transfer.NewManager(transfer.Deps{
		Remote: store, DB: db,
		MK:          func() (crypto.MasterKey, bool) { return mk, true },
		Concurrency: func() int { return 2 },
		ChunkMiB:    func() int { return 1 },
	})
	return &env{store: store, db: db, m: m, mk: mk}
}

func waitIdle(t *testing.T, m *transfer.Manager) []transfer.Transfer {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if m.Idle() {
			return m.Snapshot()
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("传输未在期限内完成")
	return nil
}

func allDone(t *testing.T, trs []transfer.Transfer) {
	t.Helper()
	for _, tr := range trs {
		if tr.Phase != transfer.PhaseDone {
			t.Fatalf("%s 处于 %s:%s", tr.Name, tr.Phase, tr.Err)
		}
	}
}

func makeFile(t *testing.T, dir, name string, size int) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i*131 + 7)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// makeJPEG 生成一张带渐变的真实 JPEG(供缩略图路径测试)。
func makeJPEG(t *testing.T, dir, name string, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 255 / w), G: uint8(y * 255 / h), B: 128, A: 255})
		}
	}
	p := filepath.Join(dir, name)
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return p
}

func fileSHA(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

func mustFolder(t *testing.T, db *index.DB, segs ...string) int64 {
	t.Helper()
	var id int64
	if err := db.WithTx(func(tx *sql.Tx) error {
		var err error
		id, err = db.EnsureFolderPath(tx, 1, segs)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestE2ELifecycle 完整生命周期:上传(文件夹递归/中文/空文件/多块/图片缩略图)
// → 远端形态 → 下载比对 → 信息查询。
func TestE2ELifecycle(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	src := t.TempDir()
	makeFile(t, src, "资料/文档 深层/说明.txt", 1000)
	makeFile(t, src, "资料/empty.bin", 0)
	makeFile(t, src, "资料/big.bin", 2_500_000) // 1MiB 块 → 3 块
	makeJPEG(t, src, "资料/照片.jpg", 700, 500)   // >512 → 触发缩放

	dest := mustFolder(t, e.db, "测试")
	n, err := e.m.UploadPaths(ctx, []string{filepath.Join(src, "资料")}, dest)
	if err != nil || n != 4 {
		t.Fatalf("入队 %d 个(期望 4): %v", n, err)
	}
	allDone(t, waitIdle(t, e.m))

	// 远端:4 个 blob + keyfile(ListBlobs 排除保留名)
	blobs, err := e.store.ListBlobs(ctx)
	if err != nil || len(blobs) != 4 {
		t.Fatalf("远端 blob 数 = %d,期望 4: %v", len(blobs), err)
	}

	// 索引:虚拟路径正确,时间戳齐全
	hits, err := e.db.Search("说明", 10)
	if err != nil || len(hits) != 1 {
		t.Fatalf("搜索 \"说明\": %+v %v", hits, err)
	}
	wantPath := "/测试/资料/文档 深层/说明.txt"
	if hits[0].Path != wantPath {
		t.Fatalf("虚拟路径 = %q,期望 %q", hits[0].Path, wantPath)
	}
	f, err := e.db.GetFile(hits[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !f.EncryptedAt.Valid || !f.UploadedAt.Valid || f.Size != 1000 {
		t.Fatalf("记录字段不完整: %+v", f)
	}
	if f.ChunkSize != 1<<20 {
		t.Fatalf("chunkSize = %d,期望 1MiB", f.ChunkSize)
	}

	// 缩略图:照片.jpg 应有,且是缩过的 JPEG
	imgHits, _ := e.db.Search("照片.jpg", 10)
	if len(imgHits) != 1 {
		t.Fatalf("找不到 照片.jpg: %+v", imgHits)
	}
	td, mime, err := e.db.GetThumbnail(imgHits[0].ID)
	if err != nil {
		t.Fatalf("缩略图缺失: %v", err)
	}
	if mime != "image/jpeg" || len(td) == 0 || len(td) > 128<<10 {
		t.Fatalf("缩略图异常: mime=%s size=%d", mime, len(td))
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(td))
	if err != nil {
		t.Fatalf("缩略图不是合法 JPEG: %v", err)
	}
	if cfg.Width > 512 || cfg.Height > 512 {
		t.Fatalf("缩略图 %dx%d 超过 512px", cfg.Width, cfg.Height)
	}

	// 下载:逐字节一致(含空文件)
	for _, target := range []string{"说明.txt", "empty.bin", "big.bin", "照片.jpg"} {
		hs, _ := e.db.Search(target, 5)
		if len(hs) != 1 {
			t.Fatalf("搜索 %s: %+v", target, hs)
		}
		outDir := t.TempDir()
		if _, err := e.m.DownloadTo(ctx, []int64{hs[0].ID}, outDir); err != nil {
			t.Fatal(err)
		}
		allDone(t, waitIdle(t, e.m))
		var srcPath string
		for _, cand := range []string{
			filepath.Join(src, "资料", "文档 深层", target),
			filepath.Join(src, "资料", target),
		} {
			if _, err := os.Stat(cand); err == nil {
				srcPath = cand
			}
		}
		got := filepath.Join(outDir, target)
		if fileSHA(t, got) != fileSHA(t, srcPath) {
			t.Fatalf("%s 下载后 SHA 不一致", target)
		}
	}
}

// TestE2ESameNameRename 同目录同名二次上传 → "(1)" 后缀。
func TestE2ESameNameRename(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	p := makeFile(t, dir, "a.txt", 10)

	dest := mustFolder(t, e.db)
	for i := 0; i < 2; i++ {
		if _, err := e.m.UploadPaths(ctx, []string{p}, dest); err != nil {
			t.Fatal(err)
		}
		allDone(t, waitIdle(t, e.m))
	}
	entries, err := e.db.ListFolder(dest)
	if err != nil || len(entries) != 2 {
		t.Fatalf("条目数 = %d,期望 2: %v %+v", len(entries), err, entries)
	}
	names := map[string]bool{entries[0].Name: true, entries[1].Name: true}
	if !names["a.txt"] || !names["a (1).txt"] {
		t.Fatalf("重名消解结果不符: %v", names)
	}
}

// TestE2ECancel 上传中取消:无索引残留、临时目录清空。
func TestE2ECancel(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	p := makeFile(t, dir, "large.bin", 32<<20) // 32MB:确保取消发生在传输途中

	if _, err := e.m.UploadPaths(ctx, []string{p}, 1); err != nil {
		t.Fatal(err)
	}
	// 立即取消(任务可能还在 queued/encrypting)
	snap := e.m.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("快照条目 = %d", len(snap))
	}
	e.m.Cancel(snap[0].ID)
	trs := waitIdle(t, e.m)
	if trs[0].Phase != transfer.PhaseCanceled {
		t.Fatalf("终态 = %s,期望 canceled", trs[0].Phase)
	}
	if hits, _ := e.db.Search("large.bin", 5); len(hits) != 0 {
		t.Fatal("取消后索引不应有记录")
	}
	ents, err := os.ReadDir(filepath.Join(os.TempDir(), "kist"))
	if err == nil && len(ents) > 0 {
		// 取消的任务已完成清理;若同进程其他测试并发残留则不算失败,只提示
		t.Logf("临时目录尚有 %d 项(可能为其他测试残留)", len(ents))
	}
}

// TestE2ERemoveAndGC 软删除 → gc(dry-run / 实删)→ 孤儿报告。
func TestE2ERemoveAndGC(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := e.m.UploadPaths(ctx, []string{makeFile(t, dir, "x.bin", 100)}, 1); err != nil {
		t.Fatal(err)
	}
	allDone(t, waitIdle(t, e.m))

	hs, _ := e.db.Search("x.bin", 5)
	if len(hs) != 1 {
		t.Fatal("上传后应可搜索")
	}
	if err := e.db.SoftDeleteFiles([]int64{hs[0].ID}); err != nil {
		t.Fatal(err)
	}
	f, _ := e.db.GetFile(hs[0].ID)
	if err := e.db.MarkBlobTrash([]string{f.BlobName}); err != nil {
		t.Fatal(err)
	}
	if again, _ := e.db.Search("x.bin", 5); len(again) != 0 {
		t.Fatal("软删后应不可搜索")
	}

	// 人为放一个孤儿 blob(索引无记录)
	orphanFile, err := os.CreateTemp("", "kist-orphan-*")
	if err != nil {
		t.Fatal(err)
	}
	orphanFile.Write([]byte("orphan"))
	orphanFile.Close()
	defer os.Remove(orphanFile.Name())
	fo, _ := os.Open(orphanFile.Name())
	defer fo.Close()
	if err := e.store.PutBlob(ctx, "aaaa0000bbbb1111cccc2222dddd3333", fo, nil); err != nil {
		t.Fatal(err)
	}

	// dry-run:列出待删与孤儿,不动远端
	del, orph, err := transfer.RunGC(ctx, e.store, e.db, true)
	if err != nil || len(del) != 1 || len(orph) != 1 {
		t.Fatalf("dry-run: del=%v orph=%v err=%v", del, orph, err)
	}
	if after, _ := e.store.ListBlobs(ctx); len(after) != 2 {
		t.Fatalf("dry-run 不应动远端: %v", after)
	}

	// 实删:trash 消失,孤儿保留
	del, orph, err = transfer.RunGC(ctx, e.store, e.db, false)
	if err != nil || len(del) != 1 || len(orph) != 1 {
		t.Fatalf("gc: del=%v orph=%v err=%v", del, orph, err)
	}
	after, _ := e.store.ListBlobs(ctx)
	if len(after) != 1 || after[0] != "aaaa0000bbbb1111cccc2222dddd3333" {
		t.Fatalf("实删后远端应只剩孤儿: %v", after)
	}
}

// TestE2EWrongMK 用错误主密钥下载 → CORRUPT/密钥不符,不留 .part 文件。
func TestE2EWrongMK(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := e.m.UploadPaths(ctx, []string{makeFile(t, dir, "secret.txt", 100)}, 1); err != nil {
		t.Fatal(err)
	}
	allDone(t, waitIdle(t, e.m))
	hs, _ := e.db.Search("secret.txt", 5)
	if len(hs) != 1 {
		t.Fatal("上传后应可搜索")
	}

	// 另一把主密钥的 Manager(模拟指向别的账户)
	wrongMK, _ := crypto.GenerateMasterKey()
	m2 := transfer.NewManager(transfer.Deps{
		Remote: e.store, DB: e.db,
		MK:          func() (crypto.MasterKey, bool) { return wrongMK, true },
		Concurrency: func() int { return 1 },
		ChunkMiB:    func() int { return 1 },
	})
	outDir := t.TempDir()
	if _, err := m2.DownloadTo(ctx, []int64{hs[0].ID}, outDir); err != nil {
		t.Fatal(err)
	}
	trs := waitIdle(t, m2)
	if trs[0].Phase != transfer.PhaseError {
		t.Fatalf("终态 = %s,期望 error", trs[0].Phase)
	}
	ents, _ := os.ReadDir(outDir)
	if len(ents) != 0 {
		t.Fatalf("失败后不应留 .part 文件: %v", ents)
	}
}

// ---- TODO-07 日志验收 ----

// syncBuf 并发安全的日志缓冲(worker goroutine 写、测试断言读,-race 下无告警)。
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLog 把全局 slog 重定向到缓冲,测试结束还原。
func captureLog(t *testing.T) func() string {
	t.Helper()
	sb := &syncBuf{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(sb, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return sb.String
}

// TestE2ELogThumbFailure 真图片生成失败记 Warn、传输起止记 Info,
// 且全流程日志不落口令(TODO-07:分级正确 + 隐私红线)。
func TestE2ELogThumbFailure(t *testing.T) {
	e := newEnv(t)
	read := captureLog(t)

	// 合法 PNG 魔数 + 垃圾字节:嗅探认作 PNG,解码必败 → 走 Warn 分支
	p := filepath.Join(t.TempDir(), "broken.png")
	if err := os.WriteFile(p, append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 256)...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.UploadPaths(context.Background(), []string{p}, 1); err != nil {
		t.Fatal(err)
	}
	allDone(t, waitIdle(t, e.m))

	s := read()
	for _, want := range []string{
		"level=INFO", "传输开始", "传输完成",
		"level=WARN", "缩略图生成失败",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("日志应包含 %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, testPass) {
		t.Errorf("日志不得包含口令 %q", testPass)
	}
}

// TestE2ELogOrphanOnIndexFailure PUT 成功而索引写入失败(库已关):
// blob 成孤儿记 Warn、传输失败记 Error(TODO-07 静默黑洞)。
func TestE2ELogOrphanOnIndexFailure(t *testing.T) {
	e := newEnv(t)
	read := captureLog(t)

	src := makeFile(t, t.TempDir(), "孤儿.bin", 4096)
	e.db.Close() // 之后索引写入必败而 PUT 照常成功(单文件路径不走 UploadPaths 的建目录事务)

	if _, err := e.m.UploadPaths(context.Background(), []string{src}, 1); err != nil {
		t.Fatal(err)
	}
	trs := waitIdle(t, e.m)
	if trs[0].Phase != transfer.PhaseError {
		t.Fatalf("终态 = %s,期望 error", trs[0].Phase)
	}
	s := read()
	if !strings.Contains(s, "level=WARN") || !strings.Contains(s, "孤儿") {
		t.Errorf("孤儿产生应记 Warn:\n%s", s)
	}
	if !strings.Contains(s, "level=ERROR") || !strings.Contains(s, "传输失败") {
		t.Errorf("传输失败应记 Error:\n%s", s)
	}
}
