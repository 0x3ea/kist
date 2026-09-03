package index

import (
	"path/filepath"
	"testing"
)

// TestListLivePaths 活路径清单:目录+文件全收、路径拼接正确、软删子树不可见。
func TestListLivePaths(t *testing.T) {
	db := newTestDB(t)
	ctx := t.TempDir()
	_ = ctx
	vid := mustFolder(t, db, "话术")
	fid := mustFolder(t, db, "话术", "s2")
	mustFile(t, db, vid, "a.mp4", "")
	mustFile(t, db, fid, "b.mp4", "")
	mustFile(t, db, 1, "root.txt", "")

	paths, err := db.ListLivePaths()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{} // path → kind
	for _, p := range paths {
		got[p.Path] = p.Kind
	}
	want := map[string]string{
		"/话术":          "folder",
		"/话术/s2":       "folder",
		"/话术/a.mp4":    "file",
		"/话术/s2/b.mp4": "file",
		"/root.txt":    "file",
	}
	for w, wk := range want {
		gk, ok := got[w]
		if !ok {
			t.Fatalf("缺路径 %q(得到 %v)", w, paths)
		}
		if gk != wk {
			t.Fatalf("%q kind 应为 %s,得到 %s", w, wk, gk)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("应恰有 %d 条,得到 %d:%v", len(want), len(got), paths)
	}

	// 软删目录:其自身与后代文件全部不可见
	if err := db.SoftDeleteFolders([]int64{vid}); err != nil {
		t.Fatal(err)
	}
	paths, err = db.ListLivePaths()
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0].Path != "/root.txt" {
		t.Fatalf("软删子树应不可见,得到 %v", paths)
	}
}

// TestSnapshotPaths 快照同构查询:SnapshotTo 产物与活库清单一致,且只读打开
// 不要求迁移(backup.FetchConflictDetail 的比对数据源)。
func TestSnapshotPaths(t *testing.T) {
	db := newTestDB(t)
	vid := mustFolder(t, db, "dir")
	mustFile(t, db, vid, "a.txt", "")
	mustFile(t, db, 1, "b.txt", "")

	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := db.SnapshotTo(snap); err != nil {
		t.Fatal(err)
	}
	live, err := db.ListLivePaths()
	if err != nil {
		t.Fatal(err)
	}
	snapPaths, err := SnapshotPaths(snap)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != len(snapPaths) {
		t.Fatalf("快照应与活库同构: live=%v snap=%v", live, snapPaths)
	}
	for i := range live {
		if live[i] != snapPaths[i] {
			t.Fatalf("第 %d 条不一致: live=%+v snap=%+v", i, live[i], snapPaths[i])
		}
	}
}
