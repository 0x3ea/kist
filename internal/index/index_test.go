package index

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func newTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// mustFolder 在根下建目录路径并返回末端 id(独立事务,便于测试)。
func mustFolder(t *testing.T, db *DB, segs ...string) int64 {
	t.Helper()
	var id int64
	err := db.WithTx(func(tx *sql.Tx) error {
		var err error
		id, err = db.EnsureFolderPath(tx, 1, segs)
		return err
	})
	if err != nil {
		t.Fatalf("EnsureFolderPath(%v): %v", segs, err)
	}
	return id
}

// mustFile 插入一条文件记录并返回 id。
func mustFile(t *testing.T, db *DB, folderID int64, name, note string) int64 {
	t.Helper()
	var id int64
	err := db.WithTx(func(tx *sql.Tx) error {
		var err error
		id, err = db.InsertFile(tx, FileRow{
			UUID: name + "-uuid", FolderID: folderID, Name: name,
			Size: 10, CipherSize: 200, SHA256: "abc", ChunkSize: 4096,
			BlobName: name + "-blob", ModifiedAt: 123,
			Note: sql.NullString{String: note, Valid: note != ""},
		})
		return err
	})
	if err != nil {
		t.Fatalf("InsertFile(%s): %v", name, err)
	}
	return id
}

func mustEntries(t *testing.T, db *DB, folderID int64) []Entry {
	t.Helper()
	es, err := db.ListFolder(folderID)
	if err != nil {
		t.Fatalf("ListFolder: %v", err)
	}
	return es
}

func TestOpenIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	dev1, err := db.DeviceID()
	if err != nil {
		t.Fatal(err)
	}
	if dev1 == "" {
		t.Fatal("device_id 为空")
	}
	db.Close()

	// 重开同一文件:迁移幂等、根目录仍在、device_id 不变
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("重开: %v", err)
	}
	defer db2.Close()
	var name string
	if err := db2.QueryRow(`SELECT name FROM folders WHERE id = 1`).Scan(&name); err != nil {
		t.Fatalf("根目录缺失: %v", err)
	}
	dev2, _ := db2.DeviceID()
	if dev1 != dev2 {
		t.Fatalf("device_id 应持久: %q vs %q", dev1, dev2)
	}
}

func TestWALMode(t *testing.T) {
	db := newTestDB(t)
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q,期望 wal", mode)
	}
}

func TestEnsureFolderPath(t *testing.T) {
	db := newTestDB(t)
	a := mustFolder(t, db, "a")
	ab := mustFolder(t, db, "a", "b")
	if a == ab {
		t.Fatal("嵌套目录不应与父级同 id")
	}
	// 已存在路径复用同一 id
	if mustFolder(t, db, "a") != a || mustFolder(t, db, "a", "b") != ab {
		t.Fatal("已存在路径应复用 id")
	}
	// 同名不同层级不冲突
	ax := mustFolder(t, db, "a", "x")
	bx := mustFolder(t, db, "b", "x")
	if ax == bx {
		t.Fatal("不同父目录下的同名目录应是两条记录")
	}
	// 非法目录段
	if err := db.WithTx(func(tx *sql.Tx) error {
		_, err := db.EnsureFolderPath(tx, 1, []string{".."})
		return err
	}); err == nil {
		t.Fatal("目录段 \"..\" 应被拒绝")
	}
}

func TestListFolderAndBreadcrumb(t *testing.T) {
	db := newTestDB(t)
	a := mustFolder(t, db, "a")
	b := mustFolder(t, db, "a", "b")
	mustFile(t, db, 1, "根文件.txt", "")
	mustFile(t, db, a, "a文件.txt", "")
	mustFile(t, db, b, "b文件.txt", "")

	root := mustEntries(t, db, 0)
	if len(root) != 2 { // 目录 a + 根文件.txt
		t.Fatalf("根目录条目数 = %d,期望 2: %+v", len(root), root)
	}
	if !root[0].IsFolder || root[0].Name != "a" {
		t.Fatalf("目录应排在文件前: %+v", root[0])
	}
	inA := mustEntries(t, db, a)
	if len(inA) != 2 { // 子目录 b + a文件.txt
		t.Fatalf("a 目录条目数 = %d,期望 2", len(inA))
	}

	crumb, err := db.FolderPath(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(crumb) != 3 || crumb[0].ID != 1 || crumb[1].Name != "a" || crumb[2].Name != "b" {
		t.Fatalf("面包屑不符: %+v", crumb)
	}
}

func TestSoftDeleteVisibility(t *testing.T) {
	db := newTestDB(t)
	a := mustFolder(t, db, "a")
	fa := mustFile(t, db, a, "文件一.txt", "")
	fr := mustFile(t, db, 1, "文件二.txt", "")

	if err := db.SoftDeleteFiles([]int64{fr}); err != nil {
		t.Fatal(err)
	}
	if len(mustEntries(t, db, 0)) != 1 { // 只剩目录 a
		t.Fatal("软删文件应从列表消失")
	}
	if hits, _ := db.Search("文件二", 10); len(hits) != 0 {
		t.Fatal("软删文件不应出现在搜索里")
	}

	// 软删目录:其下文件也应从搜索消失(祖先过滤)
	if err := db.SoftDeleteFolders([]int64{a}); err != nil {
		t.Fatal(err)
	}
	if hits, _ := db.Search("文件一", 10); len(hits) != 0 {
		t.Fatal("祖先目录软删后,其中文件不应出现在搜索里")
	}
	if f, err := db.GetFile(fa); err != nil || f.DeletedAt.Valid {
		t.Fatalf("目录软删不应连带软删文件记录: %+v %v", f, err)
	}
}

func TestSearch(t *testing.T) {
	db := newTestDB(t)
	a := mustFolder(t, db, "a")
	mustFile(t, db, 1, "Report.PDF", "")
	mustFile(t, db, a, "会议纪要.txt", "季度汇报材料")
	mustFile(t, db, 1, "100%_done.txt", "")

	// ASCII 大小写不敏感
	hits, err := db.Search("pdf", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Name != "Report.PDF" {
		t.Fatalf("搜索 \"pdf\": %+v", hits)
	}
	// 中文关键词 + 完整虚拟路径
	hits, _ = db.Search("会议", 10)
	if len(hits) != 1 || hits[0].Path != "/a/会议纪要.txt" {
		t.Fatalf("搜索 \"会议\": %+v", hits)
	}
	// 备注命中
	hits, _ = db.Search("汇报", 10)
	if len(hits) != 1 || hits[0].Name != "会议纪要.txt" || hits[0].Note != "季度汇报材料" {
		t.Fatalf("搜索 \"汇报\": %+v", hits)
	}
	// LIKE 通配符按字面量匹配(未转义的 % 会匹配一切)
	hits, _ = db.Search("100%_", 10)
	if len(hits) != 1 || hits[0].Name != "100%_done.txt" {
		t.Fatalf("搜索 \"100%%_\": %+v", hits)
	}
}

func TestRevisionConcurrent(t *testing.T) {
	db := newTestDB(t)
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- db.WithTx(func(tx *sql.Tx) error {
				_, err := tx.Exec(`INSERT INTO files
					(uuid, folder_id, name, size, cipher_size, sha256, chunk_size, blob_name, created_at, modified_at)
					VALUES (?,?,?,?,?,?,?,?,?,?)`,
					fmt.Sprintf("u%d", i), 1, fmt.Sprintf("f%d", i), 1, 1, "x", 1024,
					fmt.Sprintf("b%d", i), 1, 1)
				return err
			})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("并发事务: %v", err)
		}
	}
	rev, err := db.Revision()
	if err != nil {
		t.Fatal(err)
	}
	if rev != n {
		t.Fatalf("revision = %d,期望恰为 %d(单调不丢)", rev, n)
	}
}

func TestSnapshotAndReplaceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "index.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	a := mustFolder(t, db, "a", "b")
	mustFile(t, db, a, "快照验证.txt", "备注A")
	origDev, _ := db.DeviceID()
	revBefore, _ := db.Revision()

	snap := filepath.Join(dir, "snap.db")
	if err := db.SnapshotTo(snap); err != nil {
		t.Fatalf("SnapshotTo: %v", err)
	}

	// 模拟本地库丢失:删掉重开为全新库
	db.Close()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		os.Remove(dbPath + suffix)
	}
	db2, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if rev, _ := db2.Revision(); rev != 0 {
		t.Fatalf("全新库 revision 应为 0,得到 %d", rev)
	}
	if dev, _ := db2.DeviceID(); dev == origDev {
		t.Fatal("全新库 device_id 应不同")
	}

	// 从快照替换恢复
	if err := db2.ReplaceWith(snap); err != nil {
		t.Fatalf("ReplaceWith: %v", err)
	}
	if rev, _ := db2.Revision(); rev != revBefore {
		t.Fatalf("恢复后 revision = %d,期望 %d", rev, revBefore)
	}
	if dev, _ := db2.DeviceID(); dev != origDev {
		t.Fatal("恢复后 device_id 应来自快照")
	}
	hits, err := db2.Search("快照验证", 10)
	if err != nil || len(hits) != 1 || hits[0].Path != "/a/b/快照验证.txt" {
		t.Fatalf("恢复后搜索: %+v %v", hits, err)
	}
	// 被替换的旧库应归档存在
	bdir := filepath.Join(dir, "backups")
	es, err := os.ReadDir(bdir)
	if err != nil || len(es) == 0 {
		t.Fatalf("归档目录缺失或为空: %v", err)
	}
	db2.Close()
}

func TestThumbnailNoteUserMeta(t *testing.T) {
	db := newTestDB(t)
	id := mustFile(t, db, 1, "照片.jpg", "")

	// 缩略图往返
	data := []byte{0xFF, 0xD8, 0xFF, 0xE0, 1, 2, 3}
	err := db.WithTx(func(tx *sql.Tx) error {
		return db.PutThumbnail(tx, id, data, 512, 384, "image/jpeg")
	})
	if err != nil {
		t.Fatal(err)
	}
	got, mime, err := db.GetThumbnail(id)
	if err != nil || string(got) != string(data) || mime != "image/jpeg" {
		t.Fatalf("缩略图往返: %v %s %v", got, mime, err)
	}

	// 备注:写入→可搜索→revision 增加
	rev0, _ := db.Revision()
	if err := db.SetNote(id, "旅行照片 2026"); err != nil {
		t.Fatal(err)
	}
	rev1, _ := db.Revision()
	if rev1 != rev0+1 {
		t.Fatalf("SetNote 应使 revision +1: %d → %d", rev0, rev1)
	}
	hits, _ := db.Search("旅行", 10)
	if len(hits) != 1 {
		t.Fatalf("按备注搜索应命中: %+v", hits)
	}

	// user_meta JSON 往返
	if err := db.SetUserMeta(id, `{"star":true}`); err != nil {
		t.Fatal(err)
	}
	f, err := db.GetFile(id)
	if err != nil || !f.UserMeta.Valid || f.UserMeta.String != `{"star":true}` {
		t.Fatalf("user_meta 往返: %+v %v", f, err)
	}
}

func TestGetFileByUUID(t *testing.T) {
	db := newTestDB(t)
	mustFile(t, db, 1, "x.txt", "")
	f, err := db.GetFileByUUID("x.txt-uuid")
	if err != nil || f.Name != "x.txt" || f.State != "ready" {
		t.Fatalf("GetFileByUUID: %+v %v", f, err)
	}
	if _, err := db.GetFileByUUID("不存在"); err != sql.ErrNoRows {
		t.Fatalf("不存在的 uuid 应返回 ErrNoRows,得到 %v", err)
	}
}

func TestBlobs(t *testing.T) {
	db := newTestDB(t)
	id := mustFile(t, db, 1, "b.txt", "")
	err := db.WithTx(func(tx *sql.Tx) error {
		if err := db.RegisterBlob(tx, "b.txt-blob", "file", 200); err != nil {
			return err
		}
		return db.RegisterBlob(tx, "index.enc", "index", 500)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkBlobTrash([]string{"b.txt-blob"}); err != nil {
		t.Fatal(err)
	}
	states, err := db.ListBlobStates()
	if err != nil {
		t.Fatal(err)
	}
	if states["b.txt-blob"] != "trash" || states["index.enc"] != "active" {
		t.Fatalf("blob 状态不符: %+v", states)
	}
	_ = id
}
