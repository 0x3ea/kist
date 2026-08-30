package main

// TODO-10 封面出库的 GUI 绑定测试:GetCover 三级来源(缓存 → 远端 → legacy)、
// SetFileCover 导入/清除/断网回退出站箱、LRU 淘汰。
// 需要网络的用例走 newNetworkedApp(真 httptest WebDAV + 已解锁)。

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kist/internal/config"
	"kist/internal/crypto"
	"kist/internal/index"
	"kist/internal/transfer"
)

// newNetworkedApp newDriveEnv + 首配开库建管线 + 解锁:封面导入(远端
// PUT)与 GetCover 远端拉取的测试载体。
func newNetworkedApp(t *testing.T) (*App, *httptest.Server) {
	t.Helper()
	a, srv := newDriveEnv(t)
	if err := a.SaveWebDAVConfig(WebDAVConfig{URL: srv.URL, Username: "u", Password: "p", RootPath: "/kist", RememberPassword: true}); err != nil {
		t.Fatalf("SaveWebDAVConfig: %v", err)
	}
	testKeyfile(t, "test-pass")
	if _, err := a.Unlock("test-pass"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if a.db == nil || a.mgr == nil || a.store == nil {
		t.Fatal("首配后应开库并建管线")
	}
	return a, srv
}

// TestFileCoverBinding 导入 → 可读 → 覆盖(旧 blob trash)→ 清除(占位);
// 非图片 BAD_CONFIG、已删文件 NOT_FOUND 语义保持。
func TestFileCoverBinding(t *testing.T) {
	a, _ := newNetworkedApp(t)
	folder := seedFolder(t, a.db, "小说合集")
	f := seedFile(t, a.db, folder, "第一卷.epub", false, 100)
	cover := makeCoverJPEG(t, t.TempDir())

	// 导入:同步返回非 deferred,GetCover 可读
	deferred, err := a.SetFileCover(f, cover)
	if err != nil || deferred {
		t.Fatalf("导入:deferred=%v err=%v", deferred, err)
	}
	td, err := a.GetCover(f)
	if err != nil || td.Mime != "image/jpeg" || len(td.Data) == 0 {
		t.Fatalf("导入后封面应可读:%+v %v", td, err)
	}
	cov, err := a.db.GetReadyCover(f)
	if err != nil || cov.Source != index.CoverCustom {
		t.Fatalf("应为 custom 引用:%+v %v", cov, err)
	}
	if ents, _ := os.ReadDir(config.CoversCacheDir()); len(ents) != 1 {
		t.Fatalf("应恰一个缓存条目:%v", ents)
	}

	// 覆盖:旧 blob 进 trash(引用换手闭环)
	first := cov.BlobName
	if _, err := a.SetFileCover(f, cover); err != nil {
		t.Fatal(err)
	}
	states, _ := a.db.ListBlobStates()
	if states[first] != "trash" {
		t.Fatalf("旧封面 blob 应 trash:%v", states)
	}

	// 非图片:显式用户动作,BAD_CONFIG 不静默
	notImg := filepath.Join(t.TempDir(), "x.txt")
	if err := os.WriteFile(notImg, []byte("不是图片"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetFileCover(f, notImg); err == nil || !strings.HasPrefix(err.Error(), "[BAD_CONFIG]") {
		t.Fatalf("非图片应 BAD_CONFIG:%v", err)
	}

	// 清除:纯索引零网络;缓存条目同时删除
	deferred, err = a.SetFileCover(f, "")
	if err != nil || deferred {
		t.Fatalf("清除:deferred=%v err=%v", deferred, err)
	}
	if _, err := a.GetCover(f); err == nil || !strings.HasPrefix(err.Error(), "[NOT_FOUND]") {
		t.Fatalf("清除后应 NOT_FOUND:%v", err)
	}
	if ents, _ := os.ReadDir(config.CoversCacheDir()); len(ents) != 0 {
		t.Fatalf("清除后缓存应删除:%v", ents)
	}

	// 已删除文件:导入拒绝
	if err := a.DeleteEntries([]int64{f}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetFileCover(f, cover); err == nil || !strings.HasPrefix(err.Error(), "[NOT_FOUND]") {
		t.Fatalf("已删除文件的封面操作应 NOT_FOUND:%v", err)
	}
}

// TestGetCoverCacheAndFallback 缓存命中(断网仍可读)、断网导入回退 defer、
// 锁定 [LOCKED]、legacy 回退不依赖解锁。
func TestGetCoverCacheAndFallback(t *testing.T) {
	a, srv := newNetworkedApp(t)
	folder := seedFolder(t, a.db, "作品")
	f := seedFile(t, a.db, folder, "图.png", false, 10)
	cover := makeCoverJPEG(t, t.TempDir())
	if _, err := a.SetFileCover(f, cover); err != nil {
		t.Fatal(err)
	}
	if _, err := a.GetCover(f); err != nil {
		t.Fatal(err)
	}

	// 断网:缓存命中,零网络可读
	srv.Close()
	if td, err := a.GetCover(f); err != nil || len(td.Data) == 0 {
		t.Fatalf("断网后缓存命中应可读:%+v %v", td, err)
	}

	// 锁定:清缓存后 GetCover 需要远端,未解锁 → [LOCKED]
	a.mu.Lock()
	a.unlocked = false
	a.mk = crypto.MasterKey{}
	a.mu.Unlock()
	row, err := a.db.GetFile(f)
	if err != nil {
		t.Fatal(err)
	}
	removeCoverCache(row.UUID)
	if _, err := a.GetCover(f); err == nil || !strings.HasPrefix(err.Error(), "[LOCKED]") {
		t.Fatalf("未解锁且未缓存应 [LOCKED]:%v", err)
	}
	// legacy 不需要解锁:无 covers 引用、只有 legacy 行的文件,锁定态仍可读
	f2 := seedFile(t, a.db, folder, "legacy.png", false, 10)
	seedThumb(t, a.db, f2)
	if td, err := a.GetCover(f2); err != nil || len(td.Data) != 1 {
		t.Fatalf("锁定态 legacy 回退应可读:%+v %v", td, err)
	}
}

// TestSetFileCoverDeferredOffline 断网导入:回退出站箱(deferred=true),
// 引用 uploading 不可见、产物在箱、收账后转 ready。
func TestSetFileCoverDeferredOffline(t *testing.T) {
	a, srv := newNetworkedApp(t)
	folder := seedFolder(t, a.db, "作品")
	f := seedFile(t, a.db, folder, "离线.epub", false, 10)
	cover := makeCoverJPEG(t, t.TempDir())

	srv.Close() // 模拟断网
	deferred, err := a.SetFileCover(f, cover)
	if err != nil {
		t.Fatalf("断网导入应回退 defer 而非报错:%v", err)
	}
	if !deferred {
		t.Fatal("应上报 deferred")
	}
	if has, _ := a.db.HasCover(f); has {
		t.Fatal("uploading 引用应对外不可见")
	}
	covers, err := a.db.ListUploadingCovers()
	if err != nil || len(covers) != 1 {
		t.Fatalf("应有一笔封面挂账:%+v %v", covers, err)
	}
	if _, err := os.Stat(transfer.OutboxArtifactPath(covers[0].BlobName)); err != nil {
		t.Fatal("封面产物应在出站箱")
	}

	// 收账(push 语义):引用转 ready
	if err := a.db.MarkUploaded(covers[0].BlobName, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.GetReadyCover(f); err != nil {
		t.Fatalf("收账后应 ready:%v", err)
	}
}

// TestCoverCacheLRUEviction 预算超限按 mtime 淘汰最旧。
func TestCoverCacheLRUEviction(t *testing.T) {
	a := newTestApp(t)
	a.mu.Lock()
	a.cfg.Settings.CoverCacheMB = 1 // 1MiB(直接注入,绕过 normalize 夹取)
	a.mu.Unlock()

	keys := []string{"a.jpg", "b.jpg", "c.jpg", "d.jpg", "e.jpg"}
	for i, k := range keys {
		writeCoverCache(k, make([]byte, 300<<10)) // 5 × 300KB = 1.5MB > 1MiB
		// 错开 mtime:k 最旧,依次变新
		ts := time.Now().Add(-time.Duration(len(keys)-i) * time.Minute)
		if err := os.Chtimes(filepath.Join(config.CoversCacheDir(), k), ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	a.enforceCoverCacheLRU()

	for _, k := range keys {
		_, err := os.Stat(filepath.Join(config.CoversCacheDir(), k))
		shouldBeGone := k == "a.jpg" || k == "b.jpg"
		if shouldBeGone && err == nil {
			t.Fatalf("%s 应被淘汰", k)
		}
		if !shouldBeGone && err != nil {
			t.Fatalf("%s 应保留:%v", k, err)
		}
	}
}
