package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"kist/internal/crypto"
	"kist/internal/index"
)

// TestReconcileMatrix 启动对账四局面(TODO-22 验收):
// 双方未动 noop / 只有本地动 补推 / 只有远端动 快进拉 / 双方动 分叉。
// 另含新设备空库(远端有备份 → 自动恢复,原 SuggestPullIndex 的替代)。
func TestReconcileMatrix(t *testing.T) {
	srv, _ := newSrv(t)
	ctx := context.Background()

	// ---- 设备 A:建账户、写两条、推送(基线 = 当前 rev)----
	_, storeA, dbA := setupDevice(t, srv.URL)
	mk := createAccount(t, storeA)
	insertFile(t, dbA, "a.txt", "", false)
	insertFile(t, dbA, "b.txt", "", false)
	if _, err := BackupNow(ctx, mk, dbA, storeA, false); err != nil {
		t.Fatal(err)
	}

	// ---- 双方未动 → noop ----
	res, err := Reconcile(ctx, mk, dbA, storeA)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != ReconcileNoop {
		t.Fatalf("双方未动应 noop,得到 %s", res.Action)
	}

	// ---- 只有本地动过 → 补推,基线推进 ----
	insertFile(t, dbA, "c.txt", "", false)
	res, err = Reconcile(ctx, mk, dbA, storeA)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != ReconcilePushed || res.Backup == nil {
		t.Fatalf("只有本地动应 pushed,得到 %s", res.Action)
	}
	if bl, err := dbA.LastSyncedRev(); err != nil || bl != res.Backup.Revision {
		t.Fatalf("补推后基线应等于推送 revision: bl=%d rev=%d err=%v", bl, res.Backup.Revision, err)
	}

	// ---- 只有远端动过 → 快进拉:B 快进到 A 的状态,写一条并推送 ----
	_, storeB, dbB := setupDevice(t, srv.URL)
	if _, err := PullRemote(ctx, mk, storeB, dbB, false); err != nil {
		t.Fatal(err)
	}
	insertFile(t, dbB, "d.txt", "", false)
	if _, err := BackupNow(ctx, mk, dbB, storeB, false); err != nil {
		t.Fatal(err)
	}
	revBefore, err := dbA.Revision()
	if err != nil {
		t.Fatal(err)
	}
	res, err = Reconcile(ctx, mk, dbA, storeA)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != ReconcilePulled || res.Pull == nil {
		t.Fatalf("只有远端动应 pulled,得到 %s", res.Action)
	}
	if res.Pull.Action != "replaced" {
		t.Fatalf("快进拉应 replaced,得到 %s", res.Pull.Action)
	}
	revAfter, err := dbA.Revision()
	if err != nil {
		t.Fatal(err)
	}
	if revAfter != res.Pull.RemoteRev || revAfter == revBefore {
		t.Fatalf("快进后本地 revision 应等于远端: before=%d after=%d remote=%d",
			revBefore, revAfter, res.Pull.RemoteRev)
	}
	if n, err := dbA.FolderSummary(1); err != nil || n.FileCount != 4 {
		t.Fatalf("快进后 A 应有 4 个文件: %+v err=%v", n, err)
	}

	// ---- 双方都动过 → 分叉,不执行任何动作 ----
	insertFile(t, dbA, "e-local.txt", "", false) // A 本地推进,不推
	insertFile(t, dbB, "e-remote.txt", "", false)
	if _, err := BackupNow(ctx, mk, dbB, storeB, false); err != nil {
		t.Fatal(err)
	}
	res, err = Reconcile(ctx, mk, dbA, storeA)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != ReconcileConflict || res.Conflict == nil {
		t.Fatalf("双方动过应 conflict,得到 %s", res.Action)
	}
	if res.Conflict.Kind != KindDiverged {
		t.Fatalf("应判 diverged,得到 %s", res.Conflict.Kind)
	}

	// ---- 新设备空库(远端有备份)→ 判远端领先,自动恢复 ----
	// 空库 rev=0、基线 0;远端是 B 推的 5 文件(a,b,c,d,e-remote)
	_, storeC, dbC := setupDevice(t, srv.URL)
	res, err = Reconcile(ctx, mk, dbC, storeC)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != ReconcilePulled || res.Pull == nil || res.Pull.Action != "replaced" {
		t.Fatalf("空库应自动恢复(pulled/replaced),得到 %s", res.Action)
	}
	if n, err := dbC.FolderSummary(1); err != nil || n.FileCount != 5 {
		t.Fatalf("恢复后应有 5 个文件: %+v err=%v", n, err)
	}
}

// TestReconcileNoRemoteBackup 从未备份:本地干净 noop;本地有改动补推。
func TestReconcileNoRemoteBackup(t *testing.T) {
	srv, _ := newSrv(t)
	ctx := context.Background()

	_, store, db := setupDevice(t, srv.URL)
	mk := createAccount(t, store)
	res, err := Reconcile(ctx, mk, db, store)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != ReconcileNoop {
		t.Fatalf("空库+空远端应 noop,得到 %s", res.Action)
	}
	insertFile(t, db, "x.txt", "", false)
	res, err = Reconcile(ctx, mk, db, store)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != ReconcilePushed {
		t.Fatalf("空远端+本地有改动应补推,得到 %s", res.Action)
	}
}

func TestReconcileWrongKey(t *testing.T) {
	srv, _ := newSrv(t)
	ctx := context.Background()

	// A 建账户并备份;拿无关钥匙对同一网盘对账 → 拒绝(守住跨账户)
	_, storeA, dbA := setupDevice(t, srv.URL)
	mk := createAccount(t, storeA)
	insertFile(t, dbA, "机密.txt", "", false)
	if _, err := BackupNow(ctx, mk, dbA, storeA, false); err != nil {
		t.Fatal(err)
	}
	_, storeB, dbB := setupDevice(t, srv.URL)
	wrongMK, _ := crypto.GenerateMasterKey()
	if _, err := Reconcile(ctx, wrongMK, dbB, storeB); !errors.Is(err, crypto.ErrWrongKey) {
		t.Fatalf("钥匙不符应拒绝并返回 ErrWrongKey,得到 %v", err)
	}
}

// TestReconcileCorruptRemote 远端头部截断:对账不做自愈式推送,原样上抛
// 交调用方降级提示(修复走手动/防抖备份的既有自愈路径)。
func TestReconcileCorruptRemote(t *testing.T) {
	srv, root := newSrv(t)
	ctx := context.Background()

	_, store, db := setupDevice(t, srv.URL)
	mk := createAccount(t, store)
	insertFile(t, db, "a.txt", "", false)
	if _, err := BackupNow(ctx, mk, db, store, false); err != nil {
		t.Fatal(err)
	}
	encPath := filepath.Join(root, "kist", "index.enc")
	if err := os.Truncate(encPath, 50); err != nil { // 头部 156B,截断即读不出
		t.Fatal(err)
	}
	if _, err := Reconcile(ctx, mk, db, store); !errors.Is(err, crypto.ErrCorruptBlob) {
		t.Fatalf("远端截断应上抛 ErrCorruptBlob,得到 %v", err)
	}
}

// TestEnsureRemoteRev 裁决前重检:一致放行;不一致返回 StaleError(带当前
// revision);远端索引消失按 revision 0 比较,同样判 stale。
func TestEnsureRemoteRev(t *testing.T) {
	srv, root := newSrv(t)
	ctx := context.Background()

	_, store, db := setupDevice(t, srv.URL)
	mk := createAccount(t, store)
	insertFile(t, db, "a.txt", "", false)
	info, err := BackupNow(ctx, mk, db, store, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureRemoteRev(ctx, mk, store, info.Revision); err != nil {
		t.Fatalf("revision 一致应放行,得到 %v", err)
	}
	var stale *StaleError
	err = EnsureRemoteRev(ctx, mk, store, info.Revision+100)
	if !errors.As(err, &stale) || stale.Current != info.Revision {
		t.Fatalf("应返回 StaleError 且带当前 revision,得到 %v", err)
	}

	// 远端索引被外部删除:当前 = 0,对任何非零期望判 stale
	if err := os.Remove(filepath.Join(root, "kist", "index.enc")); err != nil {
		t.Fatal(err)
	}
	err = EnsureRemoteRev(ctx, mk, store, info.Revision)
	if !errors.As(err, &stale) || stale.Current != 0 {
		t.Fatalf("远端消失应对非零期望判 stale(Current=0),得到 %v", err)
	}
	if err := EnsureRemoteRev(ctx, mk, store, 0); err != nil {
		t.Fatalf("远端消失且期望为 0 应放行,得到 %v", err)
	}
}

// rewriteFile 模拟"同路径重上传":换 blob 名(内容同一性由此判定),
// 不改路径——DiffIndex 的 Changed 栏依据。
func rewriteFile(t *testing.T, db *index.DB, name, newBlob string) {
	t.Helper()
	if _, err := db.Exec(`UPDATE files SET blob_name = ? WHERE name = ?`, newBlob, name); err != nil {
		t.Fatal(err)
	}
}

// TestFetchConflictDetail 详情:分叉时三栏 diff 正确;本地领先无 diff;
// 远端从未备份报错。
func TestFetchConflictDetail(t *testing.T) {
	srv, _ := newSrv(t)
	ctx := context.Background()

	_, storeA, dbA := setupDevice(t, srv.URL)
	mk := createAccount(t, storeA)
	insertFile(t, dbA, "same.txt", "", false)
	insertFile(t, dbA, "local-only.txt", "", false)
	if _, err := BackupNow(ctx, mk, dbA, storeA, false); err != nil {
		t.Fatal(err)
	}

	// B 快进到同一状态,然后两侧各写:分叉
	_, storeB, dbB := setupDevice(t, srv.URL)
	if _, err := PullRemote(ctx, mk, storeB, dbB, false); err != nil {
		t.Fatal(err)
	}
	insertFile(t, dbB, "remote-only.txt", "", false)
	rewriteFile(t, dbB, "same.txt", "same-blob-v2") // 同路径不同内容 → Changed
	if _, err := BackupNow(ctx, mk, dbB, storeB, false); err != nil {
		t.Fatal(err)
	}
	insertFile(t, dbA, "another-local.txt", "", false)

	d, err := FetchConflictDetail(ctx, mk, dbA, storeA)
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != KindDiverged {
		t.Fatalf("应判 diverged,得到 %s", d.Kind)
	}
	// A 本地:same(v1)/local-only/another-local;远端:same(v2)/local-only/remote-only
	if n := d.Diff.LocalOnly.Total; n != 1 || d.Diff.LocalOnly.Items[0] != "/another-local.txt" {
		t.Fatalf("仅本地应 1 条 /another-local.txt,得到 %d %v", n, d.Diff.LocalOnly.Items)
	}
	if n := d.Diff.RemoteOnly.Total; n != 1 {
		t.Fatalf("仅远端应 1 条,得到 %d(%v)", n, d.Diff.RemoteOnly.Items)
	}
	if n := d.Diff.Changed.Total; n != 1 || d.Diff.Changed.Items[0] != "/same.txt" {
		t.Fatalf("内容不同应 1 条 /same.txt,得到 %d %v", n, d.Diff.Changed.Items)
	}

	// 本地领先(远端 == 基线):kind=local-ahead,无 diff;远端从未备份则报错
	srv2, _ := newSrv(t)
	_, storeC, dbC := setupDevice(t, srv2.URL)
	mkC := createAccount(t, storeC)
	if _, err := FetchConflictDetail(ctx, mkC, dbC, storeC); err == nil {
		t.Fatal("远端无索引应报错")
	}
	insertFile(t, dbC, "z.txt", "", false)
	if _, err := BackupNow(ctx, mkC, dbC, storeC, false); err != nil {
		t.Fatal(err)
	}
	insertFile(t, dbC, "z2.txt", "", false)
	d2, err := FetchConflictDetail(ctx, mkC, dbC, storeC)
	if err != nil {
		t.Fatal(err)
	}
	if d2.Kind != KindLocalAhead || d2.Diff.LocalOnly.Total != 0 {
		t.Fatalf("本地领先应 local-ahead 且无 diff,得到 kind=%s localOnly=%d",
			d2.Kind, d2.Diff.LocalOnly.Total)
	}
}

// TestDiffIndexCap 单栏超过 diffCap 时 Items 截断、Total 保真。
func TestDiffIndexCap(t *testing.T) {
	var local, remote []index.PathEntry
	for i := 0; i < diffCap+5; i++ {
		p := "/f" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + ".txt"
		local = append(local, index.PathEntry{Path: p, Kind: "file", Blob: "l"})
		remote = append(remote, index.PathEntry{Path: p, Kind: "file", Blob: "r"})
	}
	res := DiffIndex(local, remote)
	if res.Changed.Total != diffCap+5 || len(res.Changed.Items) != diffCap {
		t.Fatalf("应 Total=%d Items=%d,得到 Total=%d Items=%d",
			diffCap+5, diffCap, res.Changed.Total, len(res.Changed.Items))
	}
}
