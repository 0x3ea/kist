package main

// app_drive_test.go — TODO-21 多网盘生命周期的 headless 绑定层测试:
// 档案增删/查重/切换(库文件随盘走)/开新库分支(共用 keyfile)/改口令多盘扇出。
// 远端用 x/net/webdav 内存服务器,与 internal/e2e 同款手法。

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/net/webdav"

	"kist/internal/config"
	"kist/internal/crypto"
)

// newDriveEnv 干净 App + 本地 WebDAV 服务器;不预置任何档案与库。
// KIST_TMPDIR 指到私有目录:这些测试会构造 Manager(构造即清场 /tmp/kist),
// 与并行跑的 e2e/transfer 包互不干扰。
func newDriveEnv(t *testing.T) (*App, *httptest.Server) {
	t.Helper()
	t.Setenv("KIST_HOME", t.TempDir())
	t.Setenv("KIST_TMPDIR", filepath.Join(t.TempDir(), "tmp"))
	srv := httptest.NewServer(&webdav.Handler{
		FileSystem: webdav.Dir(t.TempDir()),
		LockSystem: webdav.NewMemLS(),
	})
	t.Cleanup(srv.Close)
	return NewApp(), srv
}

// testKeyfile 用快 Argon2 参数种一把本地 keyfile,返回字节。
func testKeyfile(t *testing.T, pass string) []byte {
	t.Helper()
	kf, _, err := crypto.CreateKeyFile(pass, crypto.Argon2Params{Time: 1, MemoryKiB: 1024, Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.KeyFilePath(), kf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return kf.Bytes()
}

func driveIDs(a *App) []string {
	out := []string{}
	for _, d := range a.ListDrives() {
		out = append(out, d.ID)
	}
	return out
}

func TestDriveLifecycleAndSwitch(t *testing.T) {
	a, srv := newDriveEnv(t)

	// 第一盘走兼容入口(向导同款):建档 + 开库 + 建管线
	if err := a.SaveWebDAVConfig(WebDAVConfig{URL: srv.URL, Username: "u", Password: "p", RootPath: "/kist", RememberPassword: true}); err != nil {
		t.Fatalf("SaveWebDAVConfig: %v", err)
	}
	if a.db == nil || a.mgr == nil || a.store == nil {
		t.Fatal("首配后应开库并建管线")
	}
	if st := a.GetAppState(); !st.Configured || st.DriveCount != 1 || st.DriveName == "" {
		t.Fatalf("AppState 应含当前盘信息: %+v", st)
	}

	// 第二盘:同账号不同根目录(允许的两个库)
	if err := a.SaveDrive(DriveInput{Name: "备盘", URL: srv.URL, Username: "u", Password: "p", RootPath: "/kist-b", RememberPassword: true}); err != nil {
		t.Fatalf("SaveDrive(第二盘): %v", err)
	}
	if n := len(a.ListDrives()); n != 2 {
		t.Fatalf("应有 2 个档案,得 %d", n)
	}

	// 查重:同 URL+用户名+RootPath 拒绝
	err := a.SaveDrive(DriveInput{Name: "重复", URL: srv.URL, Username: "u", RootPath: "/kist-b"})
	if err == nil || !strings.Contains(err.Error(), "同地址") {
		t.Fatalf("重复档案应被拒绝,得 %v", err)
	}

	id1, id2 := driveIDs(a)[0], driveIDs(a)[1]

	// 删除活动盘拒绝;删除非活动盘可以,最后一个盘拒绝
	if err := a.DeleteDrive(id1); err == nil || !strings.Contains(err.Error(), "当前网盘") {
		t.Fatalf("删活动盘应拒绝,得 %v", err)
	}
	if err := a.DeleteDrive(id2); err != nil {
		t.Fatalf("删非活动盘应成功: %v", err)
	}
	if err := a.SaveDrive(DriveInput{Name: "备盘", URL: srv.URL, Username: "u", Password: "p", RootPath: "/kist-b", RememberPassword: true}); err != nil {
		t.Fatalf("重建第二盘: %v", err)
	}
	id2 = driveIDs(a)[1]
	if err := a.DeleteDrive(id1); err == nil {
		t.Fatal("只剩一个档案时删除应拒绝")
	}

	// 切换:库文件随盘走,管线重建,状态带新盘名
	if err := a.SetActiveDrive("不存在的ID"); err == nil {
		t.Fatal("切换到不存在档案应报错")
	}
	if err := a.SetActiveDrive(id2); err != nil {
		t.Fatalf("SetActiveDrive: %v", err)
	}
	if a.cfg.Active != id2 || a.db == nil || a.db.Path != config.DriveIndexPath(id2) || a.mgr == nil {
		t.Fatalf("切换后库应锚定新盘: id2=%s active=%s drives=%+v", id2, a.cfg.Active, a.ListDrives())
	}
	if st := a.GetAppState(); st.DriveName != "备盘" {
		t.Fatalf("切换后 DriveName 应更新: %+v", st)
	}
	// 幂等:重复切换 no-op
	if err := a.SetActiveDrive(id2); err != nil {
		t.Fatalf("重复切换应 no-op: %v", err)
	}
	// 新盘的库是独立空库
	if n, ferr := a.db.FolderSummary(1); ferr != nil || n.FileCount != 0 {
		t.Fatalf("新盘应为空库: %+v err=%v", n, ferr)
	}
}

func TestCreateAccountOnNewVault(t *testing.T) {
	a, srv := newDriveEnv(t)
	ctx := context.Background()

	if err := a.SaveWebDAVConfig(WebDAVConfig{URL: srv.URL, Username: "u", Password: "p", RootPath: "/kist", RememberPassword: true}); err != nil {
		t.Fatalf("首配: %v", err)
	}
	localKF := testKeyfile(t, "共用口令")

	// 第一盘远端已有 keyfile:CreateAccount 拒绝(走解锁/恢复)
	store1, err := buildStore(a.cfg.ActiveDrive())
	if err != nil {
		t.Fatal(err)
	}
	if err := store1.EnsureReady(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store1.PutKeyFile(ctx, localKF); err != nil {
		t.Fatal(err)
	}
	if err := a.CreateAccount("共用口令"); err == nil || !strings.Contains(err.Error(), "已有 keyfile") {
		t.Fatalf("远端已有 keyfile 应拒绝,得 %v", err)
	}

	// 第二盘:远端无 keyfile,本地有 → 开新库 = 推现有 keyfile
	if err := a.SaveDrive(DriveInput{Name: "新库盘", URL: srv.URL, Username: "u", Password: "p", RootPath: "/kist-b", RememberPassword: true}); err != nil {
		t.Fatal(err)
	}
	id2 := driveIDs(a)[1]
	if err := a.SetActiveDrive(id2); err != nil {
		t.Fatal(err)
	}
	// 口令不符 → AUTH_FAILED,绝不静默换密钥
	if err := a.CreateAccount("错口令"); err == nil || !strings.Contains(err.Error(), "AUTH_FAILED") {
		t.Fatalf("口令不符应 AUTH_FAILED,得 %v", err)
	}
	if err := a.CreateAccount("共用口令"); err != nil {
		t.Fatalf("开新库: %v", err)
	}
	// 远端 keyfile 与本地逐字节一致;库为空;处于解锁态
	store2, err := buildStore(a.cfg.ActiveDrive())
	if err != nil {
		t.Fatal(err)
	}
	rkf, err := store2.GetKeyFile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(rkf) != string(localKF) {
		t.Fatal("开新库必须推现有 keyfile(共用密钥),不得生成新密钥")
	}
	if n, ferr := a.db.FolderSummary(1); ferr != nil || n.FileCount != 0 {
		t.Fatalf("开新库后本地应为空库: %+v", n)
	}
	if _, ok := a.mkSnapshot(); !ok {
		t.Fatal("开新库后应处于解锁态")
	}
}

func TestCreateAccountArchivesStaleLocalIndex(t *testing.T) {
	a, srv := newDriveEnv(t)

	if err := a.SaveWebDAVConfig(WebDAVConfig{URL: srv.URL, Username: "u", Password: "p", RootPath: "/kist", RememberPassword: true}); err != nil {
		t.Fatal(err)
	}
	testKeyfile(t, "共用口令")

	// 第二盘先正常使用,本地库留下数据行
	if err := a.SaveDrive(DriveInput{Name: "二号", URL: srv.URL, Username: "u", Password: "p", RootPath: "/kist-b", RememberPassword: true}); err != nil {
		t.Fatal(err)
	}
	id2 := driveIDs(a)[1]
	if err := a.SetActiveDrive(id2); err != nil {
		t.Fatal(err)
	}
	seedFile(t, a.db, seedFolder(t, a.db, "旧数据"), "残留.zip", true, 123)

	// 远端被清空的等价模拟:档案改指向全新根目录(编辑凭据不换库文件)
	if err := a.SaveDrive(DriveInput{ID: id2, Name: "二号", URL: srv.URL, Username: "u", Password: "p", RootPath: "/kist-c", RememberPassword: true}); err != nil {
		t.Fatal(err)
	}
	if a.db.Path != config.DriveIndexPath(id2) {
		t.Fatalf("编辑凭据不应换库文件: %s", a.db.Path)
	}

	// 开新库:本地库非空 → 走归档分支,旧索引文件进 backups/,新库为空
	if err := a.CreateAccount("共用口令"); err != nil {
		t.Fatalf("开新库(归档分支): %v", err)
	}
	if n, ferr := a.db.FolderSummary(1); ferr != nil || n.FileCount != 0 {
		t.Fatalf("归档后新库应为空: %+v", n)
	}
	matches, err := filepath.Glob(filepath.Join(config.BackupDir(), "index-"+id2+"-*.db"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("旧索引应归档到 backups/,得 %v err=%v", matches, err)
	}
}

func TestChangePassphraseFansOut(t *testing.T) {
	a, srv := newDriveEnv(t)
	ctx := context.Background()

	drives := []struct {
		root     string
		remember bool
	}{
		{"/kist", true},
		{"/kist-b", true},
		{"/kist-c", false}, // 未记密码:扇出跳过
	}
	for i, d := range drives {
		if err := a.SaveDrive(DriveInput{Name: string(rune('A' + i)), URL: srv.URL, Username: "u", Password: "p", RootPath: d.root, RememberPassword: d.remember}); err != nil {
			t.Fatal(err)
		}
	}
	localKF := testKeyfile(t, "旧口令")
	// 三块盘远端都放一份旧 keyfile(模拟多库已建立)
	for _, d := range drives {
		st, err := buildStore(&config.Drive{URL: srv.URL, Username: "u", Password: "p", RootPath: d.root})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.EnsureReady(ctx); err != nil {
			t.Fatal(err)
		}
		if err := st.PutKeyFile(ctx, localKF); err != nil {
			t.Fatal(err)
		}
	}

	if err := a.ChangePassphrase("旧口令", "新口令"); err != nil {
		t.Fatalf("ChangePassphrase: %v", err)
	}
	newKF, err := os.ReadFile(config.KeyFilePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(newKF) == string(localKF) {
		t.Fatal("本地 keyfile 应已 Rewrap")
	}
	// 记了密码的两盘远端同步为新 keyfile;未记密码的盘停旧口令
	for _, d := range drives {
		st, err := buildStore(&config.Drive{URL: srv.URL, Username: "u", Password: "p", RootPath: d.root})
		if err != nil {
			t.Fatal(err)
		}
		got, err := st.GetKeyFile(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if d.remember && string(got) != string(newKF) {
			t.Fatalf("盘 %s 远端 keyfile 应同步为新包装", d.root)
		}
		if !d.remember && string(got) != string(localKF) {
			t.Fatalf("未记密码的盘 %s 远端应保持旧包装", d.root)
		}
	}
	// 旧口令即刻失效、新口令可用(共用 MK)
	kf, err := crypto.ParseKeyFile(newKF)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kf.Unlock("旧口令"); err == nil {
		t.Fatal("旧口令应已失效")
	}
	if _, err := kf.Unlock("新口令"); err != nil {
		t.Fatalf("新口令应可用: %v", err)
	}
}
