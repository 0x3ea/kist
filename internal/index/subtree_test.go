package index

// FolderSubtree(文件夹下载前置)的测试:嵌套投影、空目录、软删剪枝、
// uploading 透传与根归一。

import (
	"errors"
	"testing"
)

func TestFolderSubtreeNested(t *testing.T) {
	db := newTestDB(t)
	root := mustFolder(t, db, "漫画")
	a := mustFolder(t, db, "漫画", "作品A")
	b := mustFolder(t, db, "漫画", "作品A", "第02话")
	c := mustFolder(t, db, "漫画", "作品A", "第01话") // 插入序与名称序相反,验证排序
	other := mustFolder(t, db, "漫画", "作品B")

	mustFileRow(t, db, row("a-top.jpg", a, false, "ready", 10, 1))
	mustFileRow(t, db, row("ep02.zip", b, true, "ready", 20, 2))
	mustFileRow(t, db, row("01.jpg", c, false, "ready", 30, 3))
	mustFileRow(t, db, row("outside.bin", other, false, "ready", 40, 4))
	mustFileRow(t, db, row("loose.bin", root, false, "ready", 50, 5))

	st, err := db.FolderSubtree(a)
	if err != nil {
		t.Fatalf("FolderSubtree: %v", err)
	}
	if st.RootID != a || st.RootName != "作品A" {
		t.Fatalf("root = %d/%q, want %d/作品A", st.RootID, st.RootName, a)
	}
	// 目录:父前序、同级名称序;子树外(作品B)与根本身不出现
	got := make([]string, 0, len(st.Folders))
	for _, f := range st.Folders {
		got = append(got, f.RelPath)
	}
	want := []string{"第01话", "第02话"}
	if len(got) != len(want) {
		t.Fatalf("Folders = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Folders = %v, want %v", got, want)
		}
	}
	if st.Folders[0].ID != c || st.Folders[1].ID != b {
		t.Fatalf("Folders id = %d,%d, want %d,%d", st.Folders[0].ID, st.Folders[1].ID, c, b)
	}
	// 文件:RelDir 与所属目录对应;子树外文件落网外
	type wantFile struct{ rel, name string }
	wants := []wantFile{{"", "a-top.jpg"}, {"第01话", "01.jpg"}, {"第02话", "ep02.zip"}}
	if len(st.Files) != len(wants) {
		t.Fatalf("Files 数 = %d, want %d(%v)", len(st.Files), len(wants), st.Files)
	}
	for i, w := range wants {
		if st.Files[i].RelDir != w.rel || st.Files[i].File.Name != w.name {
			t.Fatalf("Files[%d] = %s/%s, want %s/%s", i, st.Files[i].RelDir, st.Files[i].File.Name, w.rel, w.name)
		}
	}
}

func TestFolderSubtreeEmptyDir(t *testing.T) {
	db := newTestDB(t)
	root := mustFolder(t, db, "合集")
	mustFolder(t, db, "合集", "空目录") // 无任何文件的叶子目录仍要出现在投影里
	st, err := db.FolderSubtree(root)
	if err != nil {
		t.Fatalf("FolderSubtree: %v", err)
	}
	if len(st.Folders) != 1 || st.Folders[0].RelPath != "空目录" {
		t.Fatalf("Folders = %+v, want [空目录]", st.Folders)
	}
	if len(st.Files) != 0 {
		t.Fatalf("Files = %+v, want 空", st.Files)
	}
}

func TestFolderSubtreeSkipsSoftDeleted(t *testing.T) {
	db := newTestDB(t)
	root := mustFolder(t, db, "漫画")
	gone := mustFolder(t, db, "漫画", "已删作品")
	goneFile := mustFileRow(t, db, row("gone.zip", gone, true, "ready", 10, 1))
	_ = goneFile // 只需它存在于软删支下,断言为"整支缺席"
	if err := db.SoftDeleteFolders([]int64{gone}); err != nil {
		t.Fatalf("SoftDeleteFolders: %v", err)
	}

	// 同名"软删目录+活跃目录"共存(ux_folders_live 允许):只取活跃者
	live := mustFolder(t, db, "漫画", "已删作品") // EnsureFolderPath 只查活跃行,新建同名义目录
	liveFile := mustFileRow(t, db, row("live.zip", live, true, "ready", 20, 2))

	st, err := db.FolderSubtree(root)
	if err != nil {
		t.Fatalf("FolderSubtree: %v", err)
	}
	if len(st.Folders) != 1 || st.Folders[0].ID != live {
		t.Fatalf("Folders = %+v, want 仅活跃目录 %d", st.Folders, live)
	}
	if len(st.Files) != 1 || st.Files[0].File.ID != liveFile {
		t.Fatalf("Files = %+v, want 仅活跃文件", st.Files)
	}

	// 子树根本身已软删 → 不可见
	if _, err := db.FolderSubtree(gone); !errors.Is(err, errFolderInvisible) {
		t.Fatalf("FolderSubtree(已软删) err = %v, want errFolderInvisible", err)
	}
	// 不存在的 id 同样不可见
	if _, err := db.FolderSubtree(99999); !errors.Is(err, errFolderInvisible) {
		t.Fatalf("FolderSubtree(不存在) err = %v, want errFolderInvisible", err)
	}
}

func TestFolderSubtreeIncludesUploading(t *testing.T) {
	db := newTestDB(t)
	root := mustFolder(t, db, "漫画")
	mustFileRow(t, db, row("ready.zip", root, true, "ready", 10, 1))
	mustFileRow(t, db, row("pending.zip", root, true, "uploading", 20, 2))

	st, err := db.FolderSubtree(root)
	if err != nil {
		t.Fatalf("FolderSubtree: %v", err)
	}
	// uploading 照常返回:跳不跳过是下载侧的业务取舍
	if len(st.Files) != 2 {
		t.Fatalf("Files = %+v, want 2 条(含 uploading)", st.Files)
	}
}

func TestFolderSubtreeRootNormalization(t *testing.T) {
	db := newTestDB(t)
	a := mustFolder(t, db, "a")
	mustFileRow(t, db, row("x.bin", a, false, "ready", 10, 1))

	st, err := db.FolderSubtree(0) // 0 视为根
	if err != nil {
		t.Fatalf("FolderSubtree(0): %v", err)
	}
	if st.RootID != rootFolderID || st.RootName != "" {
		t.Fatalf("root = %d/%q, want %d/空串", st.RootID, st.RootName, rootFolderID)
	}
	if len(st.Folders) != 1 || st.Folders[0].RelPath != "a" {
		t.Fatalf("Folders = %+v, want [a]", st.Folders)
	}
	if len(st.Files) != 1 || st.Files[0].RelDir != "a" {
		t.Fatalf("Files = %+v, want a/x.bin", st.Files)
	}
}
