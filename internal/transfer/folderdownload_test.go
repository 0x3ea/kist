package transfer

// 文件夹下载规划(planFolderTree)与本地名消解(uniqueLocalNameTaken)的
// 单元测试:落盘行为本身由 internal/e2e 担保(与 pack/upload 同分工)。

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kist/internal/index"
)

// newDLDB 建索引库;本包首次引 index.DB,夹具自足(summary_test 的 mustFileRow
// 是 index 包内测试的,跨不过来)。
func newDLDB(t *testing.T) *index.DB {
	t.Helper()
	db, err := index.Open(filepath.Join(t.TempDir(), "idx.db"))
	if err != nil {
		t.Fatalf("index.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func dlFolder(t *testing.T, db *index.DB, segs ...string) int64 {
	t.Helper()
	var id int64
	if err := db.WithTx(func(tx *sql.Tx) error {
		var err error
		id, err = db.EnsureFolderPath(tx, 1, segs)
		return err
	}); err != nil {
		t.Fatalf("EnsureFolderPath(%v): %v", segs, err)
	}
	return id
}

var dlSeq int

// dlRow 简写:UUID/BlobName 带序号,同名文件不撞 UNIQUE。
func dlRow(name string, folderID int64, pack bool, state string) index.FileRow {
	dlSeq++
	return index.FileRow{
		UUID: fmt.Sprintf("%s-uuid-%d", name, dlSeq), FolderID: folderID, Name: name,
		Size: 10, CipherSize: 26, SHA256: "sha-" + name, ChunkSize: 4096,
		BlobName: fmt.Sprintf("%s-blob-%d", name, dlSeq), State: state, ModifiedAt: 1, Pack: pack,
	}
}

func dlFile(t *testing.T, db *index.DB, f index.FileRow) {
	t.Helper()
	if err := db.WithTx(func(tx *sql.Tx) error {
		_, err := db.InsertFile(tx, f)
		return err
	}); err != nil {
		t.Fatalf("InsertFile(%s): %v", f.Name, err)
	}
}

// newPlanManager 只服务 planFolderTree(纯查询);KIST_TMPDIR 隔离,
// 避免构造即清场与并行包/真机上的 /tmp/kist 互相误伤。
func newPlanManager(t *testing.T, db *index.DB) *Manager {
	t.Helper()
	t.Setenv("KIST_TMPDIR", filepath.Join(t.TempDir(), "tmp"))
	return NewManager(Deps{DB: db, Concurrency: func() int { return 1 }})
}

func TestPlanFolderTreeMapping(t *testing.T) {
	db := newDLDB(t)
	root := dlFolder(t, db, "作品A")
	ep1 := dlFolder(t, db, "作品A", "第01话")
	deep := dlFolder(t, db, "作品A", "extras", "深目录")
	dlFolder(t, db, "作品A", "空目录") // 无文件的叶子目录也要出现在 dirs
	dlFile(t, db, dlRow("第01话.zip", ep1, true, "ready"))
	dlFile(t, db, dlRow("封面.jpg", root, false, "ready"))
	dlFile(t, db, dlRow("deep.bin", deep, false, "ready"))

	dest := t.TempDir()
	m := newPlanManager(t, db)
	taken := map[string]bool{}
	ft, err := m.planFolderTree(root, dest, taken)
	if err != nil {
		t.Fatalf("planFolderTree: %v", err)
	}
	if ft.rootName != "作品A" || ft.rootLocal != filepath.Join(dest, "作品A") {
		t.Fatalf("root = %q/%q, want 作品A", ft.rootName, ft.rootLocal)
	}
	// 目录投影:父前序、同级名称序;空目录在内
	wantDirs := []string{"extras", filepath.Join("extras", "深目录"), "空目录", "第01话"}
	if len(ft.dirs) != len(wantDirs) {
		t.Fatalf("dirs = %v, want %v", ft.dirs, wantDirs)
	}
	for i := range wantDirs {
		if ft.dirs[i] != wantDirs[i] {
			t.Fatalf("dirs = %v, want %v", ft.dirs, wantDirs)
		}
	}
	// 文件:落盘目录 = rootLocal+RelDir,显示名带根名前缀
	wantSpecs := []struct{ name, dir string }{
		{"作品A/封面.jpg", ft.rootLocal},
		{"作品A/extras/深目录/deep.bin", filepath.Join(ft.rootLocal, "extras", "深目录")},
		{"作品A/第01话/第01话.zip", filepath.Join(ft.rootLocal, "第01话")},
	}
	if len(ft.specs) != len(wantSpecs) {
		t.Fatalf("specs 数 = %d, want %d", len(ft.specs), len(wantSpecs))
	}
	for i, w := range wantSpecs {
		if ft.specs[i].name != w.name || ft.specs[i].destDir != w.dir {
			t.Fatalf("specs[%d] = %q@%q, want %q@%q", i, ft.specs[i].name, ft.specs[i].destDir, w.name, w.dir)
		}
	}
	if ft.skipped != 0 {
		t.Fatalf("skipped = %d, want 0", ft.skipped)
	}
	// 纯规划:零 FS 写入,根目录尚未创建
	if _, err := os.Stat(ft.rootLocal); !os.IsNotExist(err) {
		t.Fatalf("规划期不应落盘:Stat(%s) = %v", ft.rootLocal, err)
	}
	if !taken["作品A"] {
		t.Fatalf("taken 未登记根名: %v", taken)
	}
}

func TestPlanFolderTreeSkipsUploading(t *testing.T) {
	db := newDLDB(t)
	root := dlFolder(t, db, "作品A")
	only := dlFolder(t, db, "作品A", "空目录")
	dlFile(t, db, dlRow("ok.zip", root, true, "ready"))
	dlFile(t, db, dlRow("pending.zip", only, true, "uploading"))

	m := newPlanManager(t, db)
	ft, err := m.planFolderTree(root, t.TempDir(), map[string]bool{})
	if err != nil {
		t.Fatalf("planFolderTree: %v", err)
	}
	if ft.skipped != 1 || len(ft.specs) != 1 || ft.specs[0].file.Name != "ok.zip" {
		t.Fatalf("skipped = %d, specs = %+v, want 跳过 1 且仅剩 ok.zip", ft.skipped, ft.specs)
	}
	// 只剩 uploading 的目录仍要建(否则整目录从落盘结果里消失)
	found := false
	for _, d := range ft.dirs {
		if d == "空目录" {
			found = true
		}
	}
	if !found {
		t.Fatalf("dirs = %v, want 含 空目录", ft.dirs)
	}
}

func TestPlanFolderTreeRootNameClash(t *testing.T) {
	db := newDLDB(t)
	root := dlFolder(t, db, "作品A")
	dlFile(t, db, dlRow("ep.zip", root, true, "ready"))

	dest := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dest, "作品A"), 0o755); err != nil { // FS 已占用
		t.Fatal(err)
	}
	m := newPlanManager(t, db)
	taken := map[string]bool{}
	ft1, err := m.planFolderTree(root, dest, taken)
	if err != nil {
		t.Fatalf("plan1: %v", err)
	}
	if ft1.rootName != "作品A (1)" { // 文件系统来源的消解
		t.Fatalf("root1 = %q, want 作品A (1)", ft1.rootName)
	}
	ft2, err := m.planFolderTree(root, dest, taken) // 同批再来一个同名根
	if err != nil {
		t.Fatalf("plan2: %v", err)
	}
	// FS 看得见 作品A、taken 占着 作品A (1) → 双来源叠加落到 (2)
	if ft2.rootName != "作品A (2)" {
		t.Fatalf("root2 = %q, want 作品A (2)", ft2.rootName)
	}
	if len(ft2.specs) == 0 || ft2.specs[0].name != "作品A (2)/ep.zip" {
		t.Fatalf("specs 显示名前缀应跟随消解后的根名: %+v", ft2.specs)
	}
}

func TestPlanFolderTreeRejects(t *testing.T) {
	db := newDLDB(t)
	root := dlFolder(t, db, "作品A")
	if err := db.SoftDeleteFolders([]int64{root}); err != nil {
		t.Fatal(err)
	}
	m := newPlanManager(t, db)
	if _, err := m.planFolderTree(root, t.TempDir(), map[string]bool{}); err == nil {
		t.Fatal("已软删的子树根应报错")
	}
	// 根目录(0 归一到 id=1)不可整树下载
	if _, err := m.planFolderTree(0, t.TempDir(), map[string]bool{}); err == nil ||
		!strings.Contains(err.Error(), "根目录不可整树下载") {
		t.Fatalf("根目录应报不可整树下载, got %v", err)
	}
}

func TestUniqueLocalNameTaken(t *testing.T) {
	dir := t.TempDir()
	if got := uniqueLocalNameTaken(dir, "a.txt", nil); got != "a.txt" {
		t.Fatalf("空目录应原样返回, got %q", got)
	}
	for _, name := range []string{"a.txt", "a (1).txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := uniqueLocalNameTaken(dir, "a.txt", nil); got != "a (2).txt" {
		t.Fatalf("FS 来源消解, got %q", got)
	}
	// taken 来源:FS 看不见的占位同样参与消解
	empty := t.TempDir()
	if got := uniqueLocalNameTaken(empty, "a.txt", map[string]bool{"a.txt": true}); got != "a (1).txt" {
		t.Fatalf("taken 来源消解, got %q", got)
	}
	if got := uniqueLocalNameTaken(empty, "a.txt", map[string]bool{"a.txt": true, "a (1).txt": true}); got != "a (2).txt" {
		t.Fatalf("双来源叠加消解, got %q", got)
	}
}
