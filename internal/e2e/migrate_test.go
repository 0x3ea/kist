package e2e

// TODO-12 迁移 e2e:双端 httptest 全链路(迁移 → 新设备视角恢复/浏览/下载)、
// 断点跳过与半截重传、无 keyfile 拒绝、无 index.enc 警告。

import (
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"kist/internal/backup"
	"kist/internal/config"
	"kist/internal/crypto"
	"kist/internal/dav"
	"kist/internal/index"
	"kist/internal/migrate"
	"kist/internal/remote"
	"kist/internal/transfer"

	"golang.org/x/net/webdav"
)

// newBareSrv 起一个空 WebDAV 服务(不带 kist 账户语义)。
func newBareSrv(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(&webdav.Handler{
		FileSystem: webdav.Dir(t.TempDir()),
		LockSystem: webdav.NewMemLS(),
	})
	t.Cleanup(srv.Close)
	return srv
}

func dstClient(t *testing.T, url string) dav.Client {
	t.Helper()
	c, err := dav.New(dav.Config{URL: url, Username: "u2", Password: "p2", RootPath: "/kist"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestMigrateEndToEnd 完整迁移 + 新设备在新端解锁、浏览、下载(TODO-12 验收)。
func TestMigrateEndToEnd(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	srcA := makeFile(t, dir, "迁移甲.bin", 3000)
	srcB := makeFile(t, dir, "迁移乙.bin", 100)
	if _, err := e.m.UploadPaths(ctx, []string{srcA, srcB}, 1); err != nil {
		t.Fatal(err)
	}
	allDone(t, waitIdle(t, e.m))
	if _, err := backup.BackupNow(ctx, e.mk, e.db, e.store); err != nil {
		t.Fatal(err)
	}

	dstSrv := newBareSrv(t)
	res, err := migrate.Run(ctx, migrate.Options{
		Src: e.store, Dst: dstClient(t, dstSrv.URL), Concurrency: 2,
	})
	if err != nil {
		t.Fatalf("migrate.Run: %v", err)
	}
	// 2 blob + keyfile + index.enc
	if res.Copied != 4 || res.Skipped != 0 || res.Failed != 0 {
		t.Fatalf("迁移计数异常: %+v", res)
	}

	// ---- 新设备视角:全新 KIST_HOME 指向新端,拉 keyfile、恢复索引、下载 ----
	t.Setenv("KIST_HOME", t.TempDir())
	if err := config.Save(&config.StoredConfig{URL: dstSrv.URL, Username: "u2", Password: "p2"}); err != nil {
		t.Fatal(err)
	}
	dcb, err := dav.New(dav.Config{URL: dstSrv.URL, Username: "u2", Password: "p2"})
	if err != nil {
		t.Fatal(err)
	}
	storeB := remote.NewStore(dcb, "/kist")
	kfb, err := storeB.GetKeyFile(ctx)
	if err != nil {
		t.Fatalf("新端拉取 keyfile: %v", err)
	}
	if err := os.WriteFile(config.KeyFilePath(), kfb, 0o600); err != nil {
		t.Fatal(err)
	}
	dbB, err := index.Open(config.IndexPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbB.Close() })
	pres, err := backup.PullRemote(ctx, e.mk, storeB, dbB)
	if err != nil || pres.Action != "replaced" {
		t.Fatalf("新端 pull: %+v %v", pres, err)
	}
	for _, name := range []string{"迁移甲.bin", "迁移乙.bin"} {
		if hits, _ := dbB.Search(name, 5); len(hits) != 1 {
			t.Fatalf("新端应能搜到 %s", name)
		}
	}
	m2 := transfer.NewManager(transfer.Deps{
		Remote: storeB, DB: dbB,
		MK:          func() (crypto.MasterKey, bool) { return e.mk, true },
		Concurrency: func() int { return 2 },
		ChunkMiB:    func() int { return 1 },
	})
	outDir := t.TempDir()
	hs, _ := dbB.Search("迁移甲.bin", 5)
	if _, err := m2.DownloadTo(ctx, []int64{hs[0].ID}, outDir); err != nil {
		t.Fatal(err)
	}
	if last := lastTr(t, waitIdle(t, m2)); last.Phase != transfer.PhaseDone {
		t.Fatalf("新端下载终态 %s(%s)", last.Phase, last.Err)
	}
	if fileSHA(t, outDir+"/迁移甲.bin") != fileSHA(t, srcA) {
		t.Fatal("迁移后下载内容与源不一致")
	}
}

// TestMigrateResume 断点语义:目标同大小对象跳过;半截残留(大小不符)重传覆盖。
func TestMigrateResume(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.m.UploadPaths(ctx, []string{makeFile(t, t.TempDir(), "断点.bin", 500)}, 1); err != nil {
		t.Fatal(err)
	}
	allDone(t, waitIdle(t, e.m))
	if _, err := backup.BackupNow(ctx, e.mk, e.db, e.store); err != nil {
		t.Fatal(err)
	}

	dstSrv := newBareSrv(t)
	dst := dstClient(t, dstSrv.URL)
	if err := dst.EnsureRoot(ctx); err != nil {
		t.Fatal(err)
	}

	// 模拟上次迁移:blob 已完整搬运(将被跳过);keyfile 名下留了半截垃圾(必须重传)
	objs, err := e.store.ListAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, ob := range objs {
		if ob.Name == "keyfile" || ob.Name == "index.enc" {
			continue
		}
		body, err := e.store.GetBlobBody(ctx, ob.Name)
		if err != nil {
			t.Fatal(err)
		}
		err = dst.PutStream(ctx, "/kist/"+ob.Name, ob.Size, body)
		body.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := dst.PutStream(ctx, "/kist/keyfile", 3, strings.NewReader("xxx")); err != nil {
		t.Fatal(err)
	}

	res, err := migrate.Run(ctx, migrate.Options{Src: e.store, Dst: dst, Concurrency: 2})
	if err != nil {
		t.Fatalf("断点重跑应成功: %v", err)
	}
	if res.Skipped != 1 || res.Copied != 2 || res.Failed != 0 {
		t.Fatalf("断点计数异常(期望跳过1/复制2): %+v", res)
	}
}

// TestMigrateRequiresKeyfile 源端没有 keyfile:显式报错,绝不静默"成功"。
func TestMigrateRequiresKeyfile(t *testing.T) {
	srcSrv := newBareSrv(t)
	srcC, err := dav.New(dav.Config{URL: srcSrv.URL, Username: "u", Password: "p", RootPath: "/kist"})
	if err != nil {
		t.Fatal(err)
	}
	store := remote.NewStore(srcC, "/kist")
	if err := store.EnsureReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	dstSrv := newBareSrv(t)
	_, err = migrate.Run(context.Background(), migrate.Options{
		Src: store, Dst: dstClient(t, dstSrv.URL),
	})
	if err == nil || !strings.Contains(err.Error(), "keyfile") {
		t.Fatalf("无 keyfile 必须显式报错,得到: %v", err)
	}
}

// TestMigrateNoIndexBackup 源端从未 backup:迁移成功但带显式警告标志。
func TestMigrateNoIndexBackup(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.m.UploadPaths(ctx, []string{makeFile(t, t.TempDir(), "无备份.bin", 100)}, 1); err != nil {
		t.Fatal(err)
	}
	allDone(t, waitIdle(t, e.m))
	// 刻意不 BackupNow:源端无 index.enc

	dstSrv := newBareSrv(t)
	res, err := migrate.Run(ctx, migrate.Options{Src: e.store, Dst: dstClient(t, dstSrv.URL)})
	if err != nil {
		t.Fatalf("无备份不是错误: %v", err)
	}
	if !res.NoIndexBackup || res.Copied != 2 { // blob + keyfile
		t.Fatalf("无备份迁移结果异常: %+v", res)
	}
}
