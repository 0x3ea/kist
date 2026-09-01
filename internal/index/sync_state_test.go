package index

import (
	"database/sql"
	"testing"
)

// TestLastSyncedRevBaseline TODO-09 验收:同步基线的首读初始化与重写。
// 升级库(无 last_synced_rev 键)首读必须以"当时的当前 revision"为基线——
// 若误从 0 起步,既存用户下次同步会被误判分叉。
func TestLastSyncedRevBaseline(t *testing.T) {
	// 升级库形态:先有历史(rev 2),基线键不存在,首次读取在此时发生
	db := newTestDB(t)
	for i := 0; i < 2; i++ {
		if err := db.WithTx(func(tx *sql.Tx) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	rev, err := db.LastSyncedRev()
	if err != nil || rev != 2 {
		t.Fatalf("升级库首读基线应=当前 revision 2: %d %v", rev, err)
	}

	// 显式重写(同步成功后的基线推进)
	if err := db.SetLastSyncedRev(7); err != nil {
		t.Fatal(err)
	}
	if rev, _ = db.LastSyncedRev(); rev != 7 {
		t.Fatalf("重写后基线应为 7: %d", rev)
	}

	// 簿记写入不推高 revision(与 SetLastBackupAt 同规)
	if cur, _ := db.Revision(); cur != 2 {
		t.Fatalf("基线簿记不得推高 revision: %d", cur)
	}

	// 全新库形态:首读发生在 rev 0,基线即 0
	fresh := newTestDB(t)
	if rev, err := fresh.LastSyncedRev(); err != nil || rev != 0 {
		t.Fatalf("空库基线应为 0: %d %v", rev, err)
	}
}
