package backup

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kist/internal/config"
	"kist/internal/crypto"
	"kist/internal/dav"
	"kist/internal/index"
	"kist/internal/remote"

	"golang.org/x/net/webdav"
)

const testPass = "backup-测试口令"

func weakParams() crypto.Argon2Params {
	return crypto.Argon2Params{Time: 1, MemoryKiB: 1024, Threads: 1}
}

// newSrv 起本地 WebDAV 服务,返回服务与底层目录(便于直接篡改文件做损坏测试)。
func newSrv(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	root := t.TempDir()
	srv := httptest.NewServer(&webdav.Handler{
		FileSystem: webdav.Dir(root),
		LockSystem: webdav.NewMemLS(),
	})
	t.Cleanup(srv.Close)
	return srv, root
}

// setupDevice 模拟一台新设备:独立 KIST_HOME + 指向同一网盘的配置。
// 测试内多处调用以切换"当前设备",注意 t.Setenv 是进程级全局,
// 依赖前一台设备的 store/db 句柄已构建完毕。
func setupDevice(t *testing.T, srvURL string) (home string, store *remote.Store, db *index.DB) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("KIST_HOME", home)
	if err := config.Save(&config.StoredConfig{URL: srvURL, Username: "u", Password: "p"}); err != nil {
		t.Fatal(err)
	}
	dc, err := dav.New(dav.Config{URL: srvURL, Username: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	store = remote.NewStore(dc, "/kist")
	db, err = index.Open(config.IndexPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return home, store, db
}

func createAccount(t *testing.T, store *remote.Store) crypto.MasterKey {
	t.Helper()
	ctx := context.Background()
	if err := store.EnsureReady(ctx); err != nil {
		t.Fatal(err)
	}
	kf, mk, err := crypto.CreateKeyFile(testPass, weakParams())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutKeyFile(ctx, kf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.KeyFilePath(), kf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return mk
}

func insertFile(t *testing.T, db *index.DB, name, note string, withThumb bool) {
	t.Helper()
	err := db.WithTx(func(tx *sql.Tx) error {
		id, err := db.InsertFile(tx, index.FileRow{
			UUID: name + "-uuid", FolderID: 1, Name: name,
			Size: 10, CipherSize: 100, SHA256: "abc", ChunkSize: 1 << 20,
			BlobName: name + "-blob", ModifiedAt: 1,
			Note: sql.NullString{String: note, Valid: note != ""},
		})
		if err != nil {
			return err
		}
		if withThumb {
			return db.PutThumbnail(tx, id, []byte{0xFF, 0xD8, 1, 2}, 512, 384, "image/jpeg")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestBackupPullDualDevice 双设备互推:LWW 替换、noop、local-newer、归档、数据完整。
func TestBackupPullDualDevice(t *testing.T) {
	srv, _ := newSrv(t)
	ctx := context.Background()

	// ---- 设备 A:建账户、写两条记录(其一含备注+缩略图)、备份 ----
	homeA, storeA, dbA := setupDevice(t, srv.URL)
	mk := createAccount(t, storeA)
	insertFile(t, dbA, "报表.xlsx", "季度汇报材料", false)
	insertFile(t, dbA, "照片.jpg", "", true)
	revA, _ := dbA.Revision()
	devA, _ := dbA.DeviceID()

	info, err := BackupNow(ctx, mk, dbA, storeA)
	if err != nil {
		t.Fatalf("BackupNow: %v", err)
	}
	if info.Revision != revA || info.Size == 0 {
		t.Fatalf("BackupInfo 异常: %+v(revA=%d)", info, revA)
	}

	// ---- 设备 B:全新家目录,拉 keyfile + pull 恢复 ----
	_, storeB, dbB := setupDevice(t, srv.URL)
	kfb, err := storeB.GetKeyFile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.KeyFilePath(), kfb, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := PullRemote(ctx, mk, storeB, dbB)
	if err != nil {
		t.Fatalf("PullRemote(B): %v", err)
	}
	if res.Action != "replaced" || res.RemoteRev != revA || res.RemoteDevice != devA {
		t.Fatalf("B 的恢复决策异常: %+v", res)
	}
	if rev, _ := dbB.Revision(); rev != revA {
		t.Fatalf("B 恢复后 revision = %d,期望 %d", rev, revA)
	}
	if dev, _ := dbB.DeviceID(); dev != devA {
		t.Fatal("B 恢复后 device_id 应来自快照")
	}
	// 备注与缩略图随库完整迁移
	hits, _ := dbB.Search("汇报", 10)
	if len(hits) != 1 || hits[0].Note != "季度汇报材料" {
		t.Fatalf("B 搜索备注: %+v", hits)
	}
	img, _ := dbB.Search("照片.jpg", 5)
	td, _, err := dbB.GetThumbnail(img[0].ID)
	if err != nil || len(td) != 4 {
		t.Fatalf("B 缩略图: %v %d", err, len(td))
	}

	// ---- B 新增记录并备份 → A pull → replaced(双向同步)----
	insertFile(t, dbB, "B 的新文件.txt", "", false)
	if _, err := BackupNow(ctx, mk, dbB, storeB); err != nil {
		t.Fatal(err)
	}
	res, err = PullRemote(ctx, mk, storeA, dbA)
	if err != nil || res.Action != "replaced" {
		t.Fatalf("A pull: %+v %v", res, err)
	}
	if hits, _ := dbA.Search("B 的新文件", 5); len(hits) != 1 {
		t.Fatalf("A pull 后应看到 B 的新文件: %+v", hits)
	}

	// ---- A 本地改动未备份 → local-newer,本地不动 ----
	insertFile(t, dbA, "A 未推送.txt", "", false)
	res, err = PullRemote(ctx, mk, storeA, dbA)
	if err != nil || res.Action != "local-newer" {
		t.Fatalf("A 二次 pull: %+v %v", res, err)
	}
	if hits, _ := dbA.Search("A 未推送", 5); len(hits) != 1 {
		t.Fatal("local-newer 不应改动本地")
	}

	// ---- noop:已同步到同一 revision ----
	if _, err := BackupNow(ctx, mk, dbA, storeA); err != nil {
		t.Fatal(err)
	}
	// 注:BackupNow 只更新 last_backup_at(不走 WithTx),revision 不变
	res, err = PullRemote(ctx, mk, storeB, dbB)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != "noop" && res.Action != "replaced" {
		t.Fatalf("期望 noop 或 replaced,得到 %s", res.Action)
	}

	// ---- A 侧被替换的旧库已归档 ----
	ents, err := os.ReadDir(filepath.Join(homeA, "backups"))
	if err != nil || len(ents) == 0 {
		t.Fatalf("A 的归档目录缺失或为空: %v", err)
	}
}

func TestPullWrongKey(t *testing.T) {
	srv, _ := newSrv(t)
	ctx := context.Background()
	_, storeA, dbA := setupDevice(t, srv.URL)
	mk := createAccount(t, storeA)
	insertFile(t, dbA, "机密.txt", "", false)
	if _, err := BackupNow(ctx, mk, dbA, storeA); err != nil {
		t.Fatal(err)
	}

	_, storeB, dbB := setupDevice(t, srv.URL)
	wrongMK, _ := crypto.GenerateMasterKey()
	if _, err := PullRemote(ctx, wrongMK, storeB, dbB); !errors.Is(err, crypto.ErrWrongKey) {
		t.Fatalf("期望 ErrWrongKey,得到 %v", err)
	}
}

func TestPullNoBackup(t *testing.T) {
	srv, _ := newSrv(t)
	ctx := context.Background()
	_, storeA, dbA := setupDevice(t, srv.URL)
	mk := createAccount(t, storeA)
	if _, err := PullRemote(ctx, mk, storeA, dbA); err == nil {
		t.Fatal("从未备份过应报错")
	}
}

// TestPullCorruptBackup 篡改远端 index.enc 的数据块 → 解密校验失败,拒绝落地。
func TestPullCorruptBackup(t *testing.T) {
	srv, root := newSrv(t)
	ctx := context.Background()
	_, storeA, dbA := setupDevice(t, srv.URL)
	mk := createAccount(t, storeA)
	insertFile(t, dbA, "x.txt", "", false)
	if _, err := BackupNow(ctx, mk, dbA, storeA); err != nil {
		t.Fatal(err)
	}

	// 直接在 WebDAV 底层目录翻转 index.enc 的一个块字节(头部之后)
	encPath := filepath.Join(root, "kist", "index.enc")
	raw, err := os.ReadFile(encPath)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-8] ^= 0xFF
	if err := os.WriteFile(encPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	_, storeB, dbB := setupDevice(t, srv.URL)
	if _, err := PullRemote(ctx, mk, storeB, dbB); err == nil {
		t.Fatal("损坏的备份必须被拒绝")
	}
}

// TestBackupPullLogs 备份完成与 pull 三种决策都记 Info 日志,且不落口令
// (TODO-07:分级正确 + 隐私红线)。
func TestBackupPullLogs(t *testing.T) {
	srv, _ := newSrv(t)
	ctx := context.Background()

	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	_, storeA, dbA := setupDevice(t, srv.URL)
	mk := createAccount(t, storeA)
	insertFile(t, dbA, "a.txt", "", false)
	if _, err := BackupNow(ctx, mk, dbA, storeA); err != nil {
		t.Fatal(err)
	}

	_, storeB, dbB := setupDevice(t, srv.URL)
	if _, err := PullRemote(ctx, mk, storeB, dbB); err != nil { // replaced
		t.Fatal(err)
	}
	if _, err := PullRemote(ctx, mk, storeB, dbB); err != nil { // noop:同 revision
		t.Fatal(err)
	}

	insertFile(t, dbA, "本地改动.txt", "", false)
	if _, err := PullRemote(ctx, mk, storeA, dbA); err != nil { // local-newer
		t.Fatal(err)
	}

	s := buf.String()
	for _, want := range []string{
		"level=INFO", "索引备份完成",
		"本地已恢复", "版本一致", "保留本地",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("日志应包含 %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, testPass) {
		t.Errorf("日志不得包含口令 %q", testPass)
	}
}
