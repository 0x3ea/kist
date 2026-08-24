package index

// TODO-16 的测试:目录元数据、目录检索、子树聚合与封面三级回退链、纯索引移动。

import (
	"database/sql"
	"fmt"
	"slices"
	"testing"
)

// mustFileRow 按显式 FileRow 插入(既有 mustFile 不带 pack/state,这里要全字段)。
func mustFileRow(t *testing.T, db *DB, f FileRow) int64 {
	t.Helper()
	var id int64
	if err := db.WithTx(func(tx *sql.Tx) error {
		var err error
		id, err = db.InsertFile(tx, f)
		return err
	}); err != nil {
		t.Fatalf("InsertFile(%s): %v", f.Name, err)
	}
	return id
}

var rowSeq int

// row 简写:folderID 必须在插入时给对——files.folder_id 有外键,事后 UPDATE 挂点
// 对测试没有额外价值,反而绕路。UUID/BlobName 带自增序号:重名消解测试会
// 插入同名文件,而这两列有 UNIQUE 约束。
func row(name string, folderID int64, pack bool, state string, size, modified int64) FileRow {
	rowSeq++
	return FileRow{
		UUID: fmt.Sprintf("%s-uuid-%d", name, rowSeq), FolderID: folderID, Name: name,
		Size: size, CipherSize: size + 16, SHA256: "sha-" + name, ChunkSize: 4096,
		BlobName: fmt.Sprintf("%s-blob-%d", name, rowSeq),
		State:    state, ModifiedAt: modified, Pack: pack,
	}
}

func mustThumb(t *testing.T, db *DB, fileID int64) {
	t.Helper()
	if err := db.WithTx(func(tx *sql.Tx) error {
		return db.PutThumbnail(tx, fileID, []byte{1}, 8, 8, "image/jpeg")
	}); err != nil {
		t.Fatalf("PutThumbnail(%d): %v", fileID, err)
	}
}

func TestFolderMetaRoundTrip(t *testing.T) {
	db := newTestDB(t)
	idA := mustFolder(t, db, "作品A")
	idB := mustFolder(t, db, "作品B")

	note := "作者:某人"
	if err := db.UpdateFolderMeta(idA, FolderMetaUpdate{
		Note: &note,
		Tags: []string{"科幻", " 已完结 ", "科幻", ""},
	}); err != nil {
		t.Fatal(err)
	}
	m, err := db.GetFolderMeta(idA)
	if err != nil {
		t.Fatal(err)
	}
	if m.Note != note {
		t.Fatalf("note 往返: %q", m.Note)
	}
	// 空白 tag 去除、重复去重
	if !slices.Equal(m.Tags, []string{"已完结", "科幻"}) {
		t.Fatalf("tags 应去重去空白并按名称序: %v", m.Tags)
	}

	// 全量覆盖换一组:旧关联替换,死 tag 行被清出 tags 表
	if err := db.UpdateFolderMeta(idA, FolderMetaUpdate{Tags: []string{"恋爱"}}); err != nil {
		t.Fatal(err)
	}
	m, _ = db.GetFolderMeta(idA)
	if !slices.Equal(m.Tags, []string{"恋爱"}) || m.Note != note {
		t.Fatalf("覆盖 tags 不应动 note: %+v", m)
	}
	var tagRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM tags`).Scan(&tagRows); err != nil {
		t.Fatal(err)
	}
	if tagRows != 1 {
		t.Fatalf("死 tag 应被清理,tags 表剩 %d 行(期望 1)", tagRows)
	}

	// 两个目录持有同名 tag:词只存一行,关联各一份
	if err := db.UpdateFolderMeta(idB, FolderMetaUpdate{Tags: []string{"恋爱", "悬疑"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM tags`).Scan(&tagRows); err != nil {
		t.Fatal(err)
	}
	if tagRows != 2 {
		t.Fatalf("同名 tag 共享一行: %d(期望 2)", tagRows)
	}

	// 清空 tags:空切片 = 清除
	if err := db.UpdateFolderMeta(idA, FolderMetaUpdate{Tags: []string{}}); err != nil {
		t.Fatal(err)
	}
	m, _ = db.GetFolderMeta(idA)
	if len(m.Tags) != 0 {
		t.Fatalf("空切片应清空 tags: %v", m.Tags)
	}

	// 不存在的目录报错,而不是静默建行
	if err := db.UpdateFolderMeta(9999, FolderMetaUpdate{Tags: []string{"x"}}); err == nil {
		t.Fatal("不存在的目录应报错")
	}
	if _, err := db.GetFolderMeta(9999); err == nil {
		t.Fatal("GetFolderMeta 对不存在的目录应报错")
	}
}

func TestSearchFolders(t *testing.T) {
	db := newTestDB(t)
	idA := mustFolder(t, db, "作品A")
	idSub := mustFolder(t, db, "作品A", "设定集")
	idB := mustFolder(t, db, "作品B")
	_ = idSub

	note := "作者:某人"
	if err := db.UpdateFolderMeta(idA, FolderMetaUpdate{Note: &note, Tags: []string{"科幻"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateFolderMeta(idB, FolderMetaUpdate{Tags: []string{"悬疑"}}); err != nil {
		t.Fatal(err)
	}

	// 三路命中:目录名 / note / tag 名
	for _, q := range []string{"作品A", "某人", "科幻"} {
		hits, err := db.SearchFolders(q, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 1 || hits[0].ID != idA || hits[0].Path != "/作品A" {
			t.Fatalf("搜索 %q: %+v", q, hits)
		}
		if q == "科幻" && !slices.Equal(hits[0].Tags, []string{"科幻"}) {
			t.Fatalf("tag 命中应带 tags: %+v", hits[0])
		}
	}

	// 子串命中子目录
	hits, err := db.SearchFolders("设定", 10)
	if err != nil || len(hits) != 1 || hits[0].ID != idSub || hits[0].Path != "/作品A/设定集" {
		t.Fatalf("子目录命中: %+v %v", hits, err)
	}

	// 祖先软删后整支不可见
	if err := db.SoftDeleteFolders([]int64{idA}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"作品A", "设定"} {
		hits, err := db.SearchFolders(q, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 0 {
			t.Fatalf("祖先软删后 %q 仍命中: %+v", q, hits)
		}
	}
}

// buildSeries 建标准测试树:
//
//	/作品A
//	  ├─ 第01话.zip  pack ready     size=100 modified=200
//	  ├─ 第02话.zip  pack uploading size=150 modified=300
//	  ├─ cover.jpg   ready          size=10  modified=50
//	  └─ 设定集/
//	      └─ notes.txt ready        size=5   modified=400
func buildSeries(t *testing.T, db *DB) (idA, idSub, ch1, ch2, cover, notes int64) {
	t.Helper()
	idA = mustFolder(t, db, "作品A")
	idSub = mustFolder(t, db, "作品A", "设定集")
	ch1 = mustFileRow(t, db, row("第01话.zip", idA, true, "ready", 100, 200))
	ch2 = mustFileRow(t, db, row("第02话.zip", idA, true, "uploading", 150, 300))
	cover = mustFileRow(t, db, row("cover.jpg", idA, false, "ready", 10, 50))
	notes = mustFileRow(t, db, row("notes.txt", idSub, false, "ready", 5, 400))
	return
}

func TestFolderSummary(t *testing.T) {
	db := newTestDB(t)
	idA, _, ch1, _, _, _ := buildSeries(t, db)

	s, err := db.FolderSummary(idA)
	if err != nil {
		t.Fatal(err)
	}
	if s.PackCount != 2 || s.FileCount != 4 || s.TotalSize != 265 || s.LatestAt != 400 || s.PendingCount != 1 {
		t.Fatalf("作品A 摘要: %+v", s)
	}
	// 根的子树 = 全库
	sRoot, err := db.FolderSummary(0)
	if err != nil || sRoot.FileCount != 4 {
		t.Fatalf("根摘要: %+v %v", sRoot, err)
	}

	// 软删一话(pack)后聚合即时收缩
	if err := db.SoftDeleteFiles([]int64{ch1}); err != nil {
		t.Fatal(err)
	}
	s, _ = db.FolderSummary(idA)
	if s.PackCount != 1 || s.FileCount != 3 || s.TotalSize != 165 {
		t.Fatalf("软删后摘要: %+v", s)
	}
}

func TestCoverFallbackChain(t *testing.T) {
	db := newTestDB(t)
	idA, idSub, ch1, ch2, cover, notes := buildSeries(t, db)
	mustThumb(t, db, ch1)
	mustThumb(t, db, cover)
	mustThumb(t, db, notes)

	// 第 2 级(无自定义封面):子条目混合名序 = cover.jpg, 第01话.zip, 第02话.zip, 设定集/
	// cover.jpg 与第01话.zip 有缩略图;第02话.zip 无缩略图记 0;
	// 设定集是目录,下钻解析到子树内首个有缩略图的 notes.txt
	s, err := db.FolderSummary(idA)
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{cover, ch1, 0, notes}
	if !slices.Equal(s.CoverFileIDs, want) {
		t.Fatalf("派生四宫格: got %v want %v", s.CoverFileIDs, want)
	}

	// 子目录的格子:representative 下钻取子树内首个有缩略图的文件
	coverA := cover
	if err := db.UpdateFolderMeta(idA, FolderMetaUpdate{Cover: nil}); err != nil {
		t.Fatal(err)
	}
	sSub, err := db.FolderSummary(idSub)
	if err != nil {
		t.Fatal(err)
	}
	// 设定集的直接子条目只有 notes.txt(有缩略图)
	if !slices.Equal(sSub.CoverFileIDs, []int64{notes}) {
		t.Fatalf("子目录四宫格: %v", sSub.CoverFileIDs)
	}
	_ = coverA

	// 第 1 级:设置自定义封面 → 单图满铺
	if err := db.UpdateFolderMeta(idA, FolderMetaUpdate{Cover: &cover}); err != nil {
		t.Fatal(err)
	}
	s, _ = db.FolderSummary(idA)
	if !slices.Equal(s.CoverFileIDs, []int64{cover}) {
		t.Fatalf("自定义封面: %v", s.CoverFileIDs)
	}

	// 引用悬空(封面文件被软删)→ 回退第 2 级,且封面文件不再是子条目:
	// 剩 3 个子条目(第01话有缩略图、第02话无、设定集下钻到 notes),尾部留白
	if err := db.SoftDeleteFiles([]int64{cover}); err != nil {
		t.Fatal(err)
	}
	s, _ = db.FolderSummary(idA)
	want = []int64{ch1, 0, notes}
	if !slices.Equal(s.CoverFileIDs, want) {
		t.Fatalf("悬空回退: got %v want %v", s.CoverFileIDs, want)
	}

	// 第 3 级:空作品返回空切片,渲染端自决
	idEmpty := mustFolder(t, db, "空作品")
	s, err = db.FolderSummary(idEmpty)
	if err != nil || len(s.CoverFileIDs) != 0 {
		t.Fatalf("空作品封面: %+v %v", s, err)
	}

	_ = ch2
}

func TestCoverMixedOrderAndLimit(t *testing.T) {
	db := newTestDB(t)
	id := mustFolder(t, db, "排序")
	// 6 个子条目验证:混合名序(目录与文件同台)与 >4 截断
	mustFolder(t, db, "排序", "b目录")
	_ = mustFileRow(t, db, row("a文件", id, false, "ready", 1, 1)) // 无缩略图 → 该格 0
	fC := mustFileRow(t, db, row("c文件", id, false, "ready", 1, 1))
	fD := mustFileRow(t, db, row("d文件", id, false, "ready", 1, 1))
	_ = mustFileRow(t, db, row("e文件", id, false, "ready", 1, 1)) // 名序第 5,被截断
	_ = mustFileRow(t, db, row("f文件", id, false, "ready", 1, 1)) // 名序第 6,被截断
	mustThumb(t, db, fC)                                         // a文件无缩略图 → 0;b目录无代表 → 0;c/d 有,e/f 被截断
	mustThumb(t, db, fD)
	s, err := db.FolderSummary(id)
	if err != nil {
		t.Fatal(err)
	}
	// 名序前四:a文件(0) b目录(0) c文件(id) d文件(id);e/f 截断
	if !slices.Equal(s.CoverFileIDs, []int64{0, 0, fC, fD}) {
		t.Fatalf("混合序+截断: %v", s.CoverFileIDs)
	}
}

func TestMoveFiles(t *testing.T) {
	db := newTestDB(t)
	idA, _, ch1, ch2, _, _ := buildSeries(t, db)
	idB := mustFolder(t, db, "作品B")

	rev0, err := db.Revision()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MoveFiles([]int64{ch1, ch2}, idB); err != nil {
		t.Fatal(err)
	}
	rev1, _ := db.Revision()
	if rev1 != rev0+1 {
		t.Fatalf("移动应计一次 revision: %d → %d", rev0, rev1)
	}
	sa, _ := db.FolderSummary(idA)
	sb, _ := db.FolderSummary(idB)
	// A 剩 cover.jpg + 设定集/notes.txt;B 收下两话(其一 uploading)
	if sa.FileCount != 2 || sa.PackCount != 0 || sb.PackCount != 2 || sb.PendingCount != 1 {
		t.Fatalf("移动后聚合: A=%+v B=%+v", sa, sb)
	}

	// 移回已有同名文件的目标:重名消解
	ch3 := mustFileRow(t, db, row("第01话.zip", idA, true, "ready", 99, 99))
	if err := db.MoveFiles([]int64{ch3}, idB); err != nil {
		t.Fatal(err)
	}
	f, err := db.GetFile(ch3)
	if err != nil || f.FolderID != idB || f.Name != "第01话 (1).zip" {
		t.Fatalf("重名消解: %+v %v", f, err)
	}

	// 不可见目录作为目标:目录在树上取不到,聚合缺席(FolderSummaries 以缺失判定)
	m, err := db.FolderSummaries([]int64{idB, 9999})
	if err != nil || len(m) != 1 {
		t.Fatalf("不可见 id 应缺席: %v %v", m, err)
	}

	// 不存在的文件报错且整个事务回滚(ch3 的挂点不应被改动)
	if err := db.MoveFiles([]int64{9999}, idA); err == nil {
		t.Fatal("不存在的文件应报错")
	}
	f, _ = db.GetFile(ch3)
	if f.FolderID != idB {
		t.Fatal("失败事务应回滚")
	}
}

// TestMetaSurvivesSnapshot 验收项:元数据经快照(备份同款 VACUUM INTO)替换后仍在。
func TestMetaSurvivesSnapshot(t *testing.T) {
	db := newTestDB(t)
	idA := mustFolder(t, db, "作品A")
	ch1 := mustFileRow(t, db, row("第01话.zip", idA, true, "ready", 100, 200))
	mustThumb(t, db, ch1)
	note := "作者:某人"
	if err := db.UpdateFolderMeta(idA, FolderMetaUpdate{Note: &note, Tags: []string{"科幻"}, Cover: &ch1}); err != nil {
		t.Fatal(err)
	}

	snap := fmt.Sprintf("%s-snap.db", db.Path)
	if err := db.SnapshotTo(snap); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceWith(snap); err != nil {
		t.Fatal(err)
	}
	m, err := db.GetFolderMeta(idA)
	if err != nil || m.Note != note || !slices.Equal(m.Tags, []string{"科幻"}) || m.CoverFileID != ch1 {
		t.Fatalf("快照替换后元数据丢失: %+v %v", m, err)
	}
	s, err := db.FolderSummary(idA)
	if err != nil || !slices.Equal(s.CoverFileIDs, []int64{ch1}) {
		t.Fatalf("快照替换后封面链失效: %+v %v", s, err)
	}
}

var _ = sql.NullString{} // 保留 import:row() 的调用方将来可能需要显式 NULL 字段
