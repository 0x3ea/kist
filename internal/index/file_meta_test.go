package index

// TODO-17 的测试:文件元数据(tag 挂文件)、双形态共享 tag 词表的死词清理、
// 文件 tag 进搜索面、v3→v4 迁移升级。

import (
	"database/sql"
	"fmt"
	"slices"
	"testing"
)

func TestFileMetaRoundTrip(t *testing.T) {
	db := newTestDB(t)
	folderID := mustFolder(t, db, "小说合集")
	fileID := mustFileRow(t, db, row("1.epub", folderID, false, "ready", 100, 1000))

	note := "第一卷"
	if err := db.UpdateFileMeta(fileID, FileMetaUpdate{
		Note: &note,
		Tags: []string{"作者:某人", " 科幻 ", "科幻", ""},
	}); err != nil {
		t.Fatal(err)
	}
	m, err := db.GetFileMeta(fileID)
	if err != nil {
		t.Fatal(err)
	}
	if m.Note != note {
		t.Fatalf("note 往返: %q", m.Note)
	}
	// 空白 tag 去除、重复去重,名称序返回(与目录侧同款)
	if !slices.Equal(m.Tags, []string{"作者:某人", "科幻"}) {
		t.Fatalf("tags 应去重去空白并按名称序: %v", m.Tags)
	}

	// 全量覆盖 tags 不动 note;死词被清出词表
	if err := db.UpdateFileMeta(fileID, FileMetaUpdate{Tags: []string{"恋爱"}}); err != nil {
		t.Fatal(err)
	}
	m, _ = db.GetFileMeta(fileID)
	if !slices.Equal(m.Tags, []string{"恋爱"}) || m.Note != note {
		t.Fatalf("覆盖 tags 不应动 note: %+v", m)
	}
	var tagRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM tags`).Scan(&tagRows); err != nil {
		t.Fatal(err)
	}
	if tagRows != 1 {
		t.Fatalf("死词应被清理,tags 表剩 %d 行(期望 1)", tagRows)
	}

	// nil Note = 不动;空切片 = 清空
	if err := db.UpdateFileMeta(fileID, FileMetaUpdate{Tags: []string{}}); err != nil {
		t.Fatal(err)
	}
	m, _ = db.GetFileMeta(fileID)
	if len(m.Tags) != 0 || m.Note != note {
		t.Fatalf("空切片清 tags、nil note 不动: %+v", m)
	}

	// 不存在的文件:读与写都报错,不静默建行
	if _, err := db.GetFileMeta(9999); err == nil {
		t.Fatal("GetFileMeta 对不存在的文件应报错")
	}
	if err := db.UpdateFileMeta(9999, FileMetaUpdate{Tags: []string{"x"}}); err == nil {
		t.Fatal("UpdateFileMeta 对不存在的文件应报错")
	}

	// 软删后的文件不可见,元数据读写随之拒绝
	if err := db.SoftDeleteFiles([]int64{fileID}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetFileMeta(fileID); err == nil {
		t.Fatal("软删后的文件 GetFileMeta 应报错")
	}
	if err := db.UpdateFileMeta(fileID, FileMetaUpdate{Tags: []string{"x"}}); err == nil {
		t.Fatal("软删后的文件 UpdateFileMeta 应报错")
	}
}

// TestTagVocabularySharedAcrossForms 是 TODO-17 的坑记录测试:tags 词表被
// folder_tags 与 file_tags 共享,任一侧清死词都必须 UNION 两张挂点表——
// 只查自己会把另一形态仍在用的同名词误删。两个方向各验一次。
func TestTagVocabularySharedAcrossForms(t *testing.T) {
	db := newTestDB(t)
	folderID := mustFolder(t, db, "小说合集")
	fileID := mustFileRow(t, db, row("1.epub", folderID, false, "ready", 100, 1000))

	tagCount := func() int {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM tags`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// 同词挂两形态:词表只存一行
	if err := db.UpdateFolderMeta(folderID, FolderMetaUpdate{Tags: []string{"科幻"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateFileMeta(fileID, FileMetaUpdate{Tags: []string{"科幻"}}); err != nil {
		t.Fatal(err)
	}
	if n := tagCount(); n != 1 {
		t.Fatalf("同词两形态共享一行: %d(期望 1)", n)
	}

	// 方向一:清目录侧,文件仍在用——词必须活下来
	if err := db.UpdateFolderMeta(folderID, FolderMetaUpdate{Tags: []string{}}); err != nil {
		t.Fatal(err)
	}
	if n := tagCount(); n != 1 {
		t.Fatalf("清目录 tag 误删了文件仍在用的词: 剩 %d(期望 1)", n)
	}
	if m, err := db.GetFileMeta(fileID); err != nil || !slices.Equal(m.Tags, []string{"科幻"}) {
		t.Fatalf("文件的 tag 不应受目录清理影响: %+v err=%v", m, err)
	}

	// 方向二:清文件侧,目录仍在用——词同样必须活下来
	if err := db.UpdateFolderMeta(folderID, FolderMetaUpdate{Tags: []string{"科幻"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateFileMeta(fileID, FileMetaUpdate{Tags: []string{}}); err != nil {
		t.Fatal(err)
	}
	if n := tagCount(); n != 1 {
		t.Fatalf("清文件 tag 误删了目录仍在用的词: 剩 %d(期望 1)", n)
	}

	// 两边都清:无人再用,词被清出词表
	if err := db.UpdateFolderMeta(folderID, FolderMetaUpdate{Tags: []string{}}); err != nil {
		t.Fatal(err)
	}
	if n := tagCount(); n != 0 {
		t.Fatalf("两形态都清后词应被回收: 剩 %d(期望 0)", n)
	}
}

func TestFileTagSearch(t *testing.T) {
	db := newTestDB(t)
	folderID := mustFolder(t, db, "小说合集")
	tagged := mustFileRow(t, db, row("第一卷.epub", folderID, false, "ready", 100, 1000))
	mustFileRow(t, db, row("第二卷.epub", folderID, false, "ready", 100, 1001))

	if err := db.UpdateFileMeta(tagged, FileMetaUpdate{Tags: []string{"作者:某人"}}); err != nil {
		t.Fatal(err)
	}

	// 按 tag 子串命中,与文件名/备注同入口
	hits, err := db.Search("某人", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != tagged {
		t.Fatalf("tag 命中应只有第一卷: %+v", hits)
	}
	if !slices.Equal(hits[0].Tags, []string{"作者:某人"}) {
		t.Fatalf("FileHit.Tags 应回填: %v", hits[0].Tags)
	}

	// 无 tag 的文件 Tags 归一为空切片(而非 nil),前端按数组处理
	hits, err = db.Search("第二卷", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || len(hits[0].Tags) != 0 {
		t.Fatalf("按名命中且 Tags 为空切片: %+v", hits)
	}
}

// TestV4UpgradeFromV3 模拟 v3 老库(版本号回退 + 拆掉 v4 的表、补回 v3 独有
// 而现版 schema 已不再建的列,数据保留):重新打开时 migrate 应补建
// file_tags 并一路升到最新,既有数据无损。
func TestV4UpgradeFromV3(t *testing.T) {
	db := newTestDB(t)
	folderID := mustFolder(t, db, "漫画库")
	fileID := mustFileRow(t, db, row("话1.pack", folderID, true, "ready", 1000, 1000))
	path := db.Path
	db.Close()

	d, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DROP TABLE file_tags`,
		// v3 的 cover_file_id 列(v6 起在现版 schema 中已删除):补回才能
		// 如实模拟 v3 老库,migrate 重放 v6 的 DROP COLUMN 才有列可删
		`ALTER TABLE folders ADD COLUMN cover_file_id INTEGER REFERENCES files(id) ON DELETE SET NULL`,
		`PRAGMA user_version = 3`,
	} {
		if _, err := d.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	d.Close()

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("老库升级打开: %v", err)
	}
	t.Cleanup(func() { db2.Close() })

	var v int
	if err := db2.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != len(migrations) {
		t.Fatalf("user_version 应为 %d: %d err=%v", len(migrations), v, err)
	}
	// 既有数据无损,v4 表即刻可用
	if m, err := db2.GetFileMeta(fileID); err != nil || len(m.Tags) != 0 {
		t.Fatalf("升级后旧文件可读: %+v err=%v", m, err)
	}
	if err := db2.UpdateFileMeta(fileID, FileMetaUpdate{Tags: []string{"科幻"}}); err != nil {
		t.Fatalf("升级后 v4 表可写: %v", err)
	}
}

// TestFileMetaSurvivesSnapshot 文件 tag 与手动封面随快照替换存活
// (TestMetaSurvivesSnapshot 的文件侧孪生;index.enc LWW 同步的地基)。
func TestFileMetaSurvivesSnapshot(t *testing.T) {
	db := newTestDB(t)
	folderID := mustFolder(t, db, "小说合集")
	fileID := mustFileRow(t, db, row("第一卷.epub", folderID, false, "ready", 100, 1000))
	note := "作者:某人"
	if err := db.UpdateFileMeta(fileID, FileMetaUpdate{Note: &note, Tags: []string{"科幻"}}); err != nil {
		t.Fatal(err)
	}
	// 手动封面:直写 covers 引用行(GUI SetFileCover 的索引侧效果)
	mustCover(t, db, fileID)

	snap := fmt.Sprintf("%s-snap.db", db.Path)
	if err := db.SnapshotTo(snap); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceWith(snap); err != nil {
		t.Fatal(err)
	}
	m, err := db.GetFileMeta(fileID)
	if err != nil || m.Note != note || !slices.Equal(m.Tags, []string{"科幻"}) {
		t.Fatalf("快照替换后文件元数据丢失: %+v %v", m, err)
	}
	c, err := db.GetReadyCover(fileID)
	if err != nil || c.Mime != "image/jpeg" || c.BlobName == "" {
		t.Fatalf("快照替换后手动封面丢失: %+v %v", c, err)
	}
}
