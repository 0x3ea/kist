package backup

// conflict_test.go — TODO-09 同步分叉检测(SVN 式基线校验)验收:
//   - 双设备分叉:push 拒绝静默覆盖,给出 *Conflict;两种裁决(保留本机/
//     保留云端)行为正确
//   - 平局盲区:两侧各写一次(revision 计数相等)仍被识别为分叉——旧 LWW
//     在此误判 noop/覆盖,是本次修复的核心场景
//   - 快进路径行为与现状一致(不引入多余确认);同步成功后基线推进正确
//   - 远端头部损坏(截断类)时 push 自愈,不与跨账户保护(ErrWrongKey)混淆

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"kist/internal/config"
	"kist/internal/crypto"
	"kist/internal/index"
	"kist/internal/remote"
)

// asConflict 断言 err 是 *Conflict 并返回它。
func asConflict(t *testing.T, err error) *Conflict {
	t.Helper()
	var c *Conflict
	if !errors.As(err, &c) {
		t.Fatalf("期望 *Conflict,得到 %v", err)
	}
	return c
}

// newPullDevice 模拟新设备:全新家目录,拉 keyfile 后快进恢复远端索引。
func newPullDevice(t *testing.T, ctx context.Context, srvURL string, mk crypto.MasterKey) (*remote.Store, *index.DB) {
	t.Helper()
	_, store, db := setupDevice(t, srvURL)
	kfb, err := store.GetKeyFile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.KeyFilePath(), kfb, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := PullRemote(ctx, mk, store, db, false)
	if err != nil || res.Action != "replaced" {
		t.Fatalf("快进恢复: %+v %v", res, err)
	}
	return store, db
}

// TestPushDetectsDivergence 验收:平局分叉(两侧各写一次、计数相等)后,
// push/pull 双双拒绝,远端不被静默覆盖——旧 LWW 在此误判 noop/覆盖,
// 是 TODO-09 的核心修复场景。
func TestPushDetectsDivergence(t *testing.T) {
	srv, _ := newSrv(t)
	ctx := context.Background()

	_, storeA, dbA := setupDevice(t, srv.URL)
	mk := createAccount(t, storeA)
	insertFile(t, dbA, "A1.txt", "", false) // rev 1
	if _, err := BackupNow(ctx, mk, dbA, storeA, false); err != nil {
		t.Fatal(err)
	}
	storeB, dbB := newPullDevice(t, ctx, srv.URL, mk) // 双方基线均为 1

	// 平局:A、B 各写恰好一条,双方 rev 2
	insertFile(t, dbA, "A2.txt", "", false)
	if _, err := BackupNow(ctx, mk, dbA, storeA, false); err != nil {
		t.Fatal(err)
	}
	insertFile(t, dbB, "B1.txt", "", false)

	// B push:本机 2 ≠ 基线 1,远端 2 ≠ 基线 1 → 分叉拒绝
	_, err := BackupNow(ctx, mk, dbB, storeB, false)
	c := asConflict(t, err)
	if c.Kind != KindDiverged || c.LocalRev != 2 || c.RemoteRev != 2 || c.BaselineRev != 1 {
		t.Fatalf("分叉字段不符: %+v", c)
	}

	// B pull 同样拒绝(旧逻辑在计数相等时误判 noop)
	if _, err := PullRemote(ctx, mk, storeB, dbB, false); !errors.As(err, &c) || c.Kind != KindDiverged {
		t.Fatalf("B pull 应报分叉: %v", err)
	}

	// 远端未被覆盖:A(干净方)pull 决策 noop,看不到 B 的数据
	res, err := PullRemote(ctx, mk, storeA, dbA, false)
	if err != nil || res.Action != "noop" {
		t.Fatalf("A pull 应 noop(远端原样): %+v %v", res, err)
	}
	if hits, _ := dbA.Search("B1", 5); len(hits) != 0 {
		t.Fatalf("拒绝推送后远端不得出现 B 的数据: %+v", hits)
	}
}

// TestResolveKeepLocal 裁决"保留本机":force 推送成功,远端换成本机内容,
// 基线推进;全新设备拉到的就是这份内容。
func TestResolveKeepLocal(t *testing.T) {
	srv, _ := newSrv(t)
	ctx := context.Background()

	_, storeA, dbA := setupDevice(t, srv.URL)
	mk := createAccount(t, storeA)
	insertFile(t, dbA, "A1.txt", "", false) // rev 1
	if _, err := BackupNow(ctx, mk, dbA, storeA, false); err != nil {
		t.Fatal(err)
	}
	storeB, dbB := newPullDevice(t, ctx, srv.URL, mk)

	// 分叉:A 推到 rev 2;B 写到 rev 2(平局)
	insertFile(t, dbA, "A2.txt", "", false)
	if _, err := BackupNow(ctx, mk, dbA, storeA, false); err != nil {
		t.Fatal(err)
	}
	insertFile(t, dbB, "B1.txt", "", false)
	if _, err := BackupNow(ctx, mk, dbB, storeB, false); err == nil {
		t.Fatal("分叉 push 应被拒")
	}

	// 裁决:保留本机
	if _, err := BackupNow(ctx, mk, dbB, storeB, true); err != nil {
		t.Fatalf("force 推送应成功: %v", err)
	}
	if rev, _ := dbB.LastSyncedRev(); rev != 2 {
		t.Fatalf("force 推送后基线应=2: %d", rev)
	}

	// 全新设备 C 拉到的就是 B 的内容(A 的第 2 条已随裁决被放弃)
	_, dbC := newPullDevice(t, ctx, srv.URL, mk)
	if hits, _ := dbC.Search("B1", 5); len(hits) != 1 {
		t.Fatalf("C 应看到保留本机的内容: %+v", hits)
	}
	if hits, _ := dbC.Search("A2", 5); len(hits) != 0 {
		t.Fatalf("被放弃的远端旧内容不应出现: %+v", hits)
	}
}

// TestResolveKeepRemote 裁决"保留云端":pull force 采纳远端,本机改动
// 归档 backups/,基线推进到远端 revision。
func TestResolveKeepRemote(t *testing.T) {
	srv, _ := newSrv(t)
	ctx := context.Background()

	_, storeA, dbA := setupDevice(t, srv.URL)
	mk := createAccount(t, storeA)
	insertFile(t, dbA, "A1.txt", "", false) // rev 1
	if _, err := BackupNow(ctx, mk, dbA, storeA, false); err != nil {
		t.Fatal(err)
	}
	homeB, storeB, dbB := setupDevice(t, srv.URL)
	kfb, err := storeB.GetKeyFile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.KeyFilePath(), kfb, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PullRemote(ctx, mk, storeB, dbB, false); err != nil {
		t.Fatal(err)
	}

	// 再造分叉:A 推到 rev 3;B 本机也写到 rev 3
	insertFile(t, dbA, "A2.txt", "", false)
	if _, err := BackupNow(ctx, mk, dbA, storeA, false); err != nil {
		t.Fatal(err)
	}
	insertFile(t, dbA, "A3.txt", "", false)
	if _, err := BackupNow(ctx, mk, dbA, storeA, false); err != nil {
		t.Fatal(err)
	}
	insertFile(t, dbB, "B1.txt", "", false)
	if _, err := BackupNow(ctx, mk, dbB, storeB, false); err == nil {
		t.Fatal("分叉 push 应被拒")
	}

	// 裁决:保留云端(force pull)
	res, err := PullRemote(ctx, mk, storeB, dbB, true)
	if err != nil || res.Action != "replaced" || !res.Forked {
		t.Fatalf("force pull 应 replaced+Forked: %+v %v", res, err)
	}
	if rev, _ := dbB.LastSyncedRev(); rev != 3 {
		t.Fatalf("采纳远端后基线应=3: %d", rev)
	}
	if hits, _ := dbB.Search("A3", 5); len(hits) != 1 {
		t.Fatalf("B 应看到远端内容: %+v", hits)
	}
	if hits, _ := dbB.Search("B1", 5); len(hits) != 0 {
		t.Fatalf("被放弃的本机改动不应在册(已归档): %+v", hits)
	}
	if ents, err := os.ReadDir(filepath.Join(homeB, "backups")); err != nil || len(ents) == 0 {
		t.Fatalf("本机改动应归档 backups/: %v", err)
	}
}

// TestPushRemoteAhead 只有一方动过的良性形态:远端单独走了 → push 报
// remote-ahead 指引先拉(而非分叉);快进后恢复正常推送,全程无需确认。
func TestPushRemoteAhead(t *testing.T) {
	srv, _ := newSrv(t)
	ctx := context.Background()

	_, storeA, dbA := setupDevice(t, srv.URL)
	mk := createAccount(t, storeA)
	insertFile(t, dbA, "A1.txt", "", false) // rev 1
	if _, err := BackupNow(ctx, mk, dbA, storeA, false); err != nil {
		t.Fatal(err)
	}
	storeB, dbB := newPullDevice(t, ctx, srv.URL, mk)

	// B 单独写并推(远端 rev 2);A 保持干净
	insertFile(t, dbB, "B1.txt", "", false)
	if _, err := BackupNow(ctx, mk, dbB, storeB, false); err != nil {
		t.Fatal(err)
	}

	// A push:本机未动(== 基线),远端动了 → remote-ahead,不是分叉
	_, err := BackupNow(ctx, mk, dbA, storeA, false)
	c := asConflict(t, err)
	if c.Kind != KindRemoteAhead {
		t.Fatalf("应判 remote-ahead: %+v", c)
	}

	// A 快进拉取(与现状一致,无多余确认),基线到位后推送恢复畅通
	res, err := PullRemote(ctx, mk, storeA, dbA, false)
	if err != nil || res.Action != "replaced" || res.Forked {
		t.Fatalf("A 快进: %+v %v", res, err)
	}
	if rev, _ := dbA.LastSyncedRev(); rev != 2 {
		t.Fatalf("快进后基线应=2: %d", rev)
	}
	if _, err := BackupNow(ctx, mk, dbA, storeA, false); err != nil {
		t.Fatalf("同步后应可正常推送: %v", err)
	}
}

// TestPushSelfHealsCorruptRemote 远端 index.enc 截断(如上传中断)时:
// 检测读不出远端 revision,推是唯一出路——直接放行覆盖(旧系统语义)。
// 注意头部认证失败与钥匙不符在 crypto 层同形(ErrWrongKey),后者仍拒绝,
// 跨账户保护不受影响。
func TestPushSelfHealsCorruptRemote(t *testing.T) {
	srv, root := newSrv(t)
	ctx := context.Background()
	_, storeA, dbA := setupDevice(t, srv.URL)
	mk := createAccount(t, storeA)
	insertFile(t, dbA, "A1.txt", "", false)
	if _, err := BackupNow(ctx, mk, dbA, storeA, false); err != nil {
		t.Fatal(err)
	}

	// 截断远端 index.enc 到 <156B(头部不完整 → ErrCorruptBlob 形态)
	encPath := filepath.Join(root, "kist", "index.enc")
	raw, err := os.ReadFile(encPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(encPath, raw[:100], 0o600); err != nil {
		t.Fatal(err)
	}

	insertFile(t, dbA, "A2.txt", "", false)
	if _, err := BackupNow(ctx, mk, dbA, storeA, false); err != nil {
		t.Fatalf("截断远端应放行推送自愈: %v", err)
	}

	// 自愈后远端可用:全新设备正常拉取
	_, dbB := newPullDevice(t, ctx, srv.URL, mk)
	if hits, _ := dbB.Search("A2", 5); len(hits) != 1 {
		t.Fatalf("自愈后拉取应见最新内容: %+v", hits)
	}
}
