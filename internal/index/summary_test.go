package index

// TODO-16 的测试:目录元数据、目录检索、子树聚合与封面三级回退链、纯索引移动。

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
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

// mustThumb 种一行 legacy 缩略图(TODO-10 出库后 PutThumbnail 已删,legacy
// 表只读,测试直插 raw SQL 是唯一合法写入方——迁移回退语义的测试载体)。
func mustThumb(t *testing.T, db *DB, fileID int64) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT OR REPLACE INTO thumbnails (file_id, data, width, height, mime) VALUES (?,?,?,?,?)`,
		fileID, []byte{1}, 8, 8, "image/jpeg"); err != nil {
		t.Fatalf("seed thumbnails(%d): %v", fileID, err)
	}
}

// mustCover 种一行 ready 封面引用(TODO-10 出库后的封面常规形态)。
func mustCover(t *testing.T, db *DB, fileID int64) {
	t.Helper()
	if err := db.WithTx(func(tx *sql.Tx) error {
		_, err := db.PutCover(tx, CoverRow{
			FileID: fileID, BlobName: fmt.Sprintf("cover-%d", fileID), Size: 128,
			Width: 8, Height: 8, Mime: "image/jpeg", Source: CoverCustom,
			State: CoverReady, CreatedAt: now(),
		})
		return err
	}); err != nil {
		t.Fatalf("PutCover(%d): %v", fileID, err)
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

// mustFolderCover 种一行目录自有封面引用(v6)。
func mustFolderCover(t *testing.T, db *DB, folderID int64, state string) string {
	t.Helper()
	blob := fmt.Sprintf("fcover-%d", folderID)
	err := db.WithTx(func(tx *sql.Tx) error {
		_, err := db.PutFolderCover(tx, FolderCoverRow{
			FolderID: folderID, BlobName: blob, Size: 128,
			Width: 8, Height: 8, Mime: "image/jpeg", Source: CoverCustom,
			State: state, CreatedAt: now(),
		})
		return err
	})
	if err != nil {
		t.Fatalf("PutFolderCover(%d): %v", folderID, err)
	}
	return blob
}

func TestCoverFallbackChain(t *testing.T) {
	db := newTestDB(t)
	idA, idSub, ch1, ch2, cover, notes := buildSeries(t, db)
	mustThumb(t, db, ch1)
	mustThumb(t, db, cover)
	mustThumb(t, db, notes)

	// 派生拼贴(无自有封面):子条目混合名序 = cover.jpg, 第01话.zip, 第02话.zip, 设定集/
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
	if s.CustomCover {
		t.Fatal("未设自有封面不应满铺")
	}

	// 子目录的格子:representative 下钻取子树内首个有缩略图的文件。
	// 设定集的直接子条目只有 notes.txt(有缩略图):单槽派生不构成宫格,
	// 回落空切片(渲染端显示默认文件夹图标,满铺专属自有封面)
	sSub, err := db.FolderSummary(idSub)
	if err != nil {
		t.Fatal(err)
	}
	if len(sSub.CoverFileIDs) != 0 {
		t.Fatalf("子目录单槽应回落: %v", sSub.CoverFileIDs)
	}

	// 自有封面(ready)→ CustomCover 满铺标志;拼贴链保留(渲染端满铺时不看)
	mustFolderCover(t, db, idA, CoverReady)
	s, _ = db.FolderSummary(idA)
	if !s.CustomCover {
		t.Fatal("自有封面应满铺")
	}
	if !slices.Equal(s.CoverFileIDs, want) {
		t.Fatalf("满铺时拼贴链应保留原值: got %v want %v", s.CoverFileIDs, want)
	}

	// 挂账中(uploading)不可见:CustomCover=false,继续走派生拼贴
	if err := db.WithTx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE folder_covers SET state = ? WHERE folder_id = ?`, CoverUploading, idA)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	s, _ = db.FolderSummary(idA)
	if s.CustomCover {
		t.Fatal("uploading 挂账不应可见")
	}

	// 清除自有封面 → 回落派生拼贴
	if err := db.WithTx(func(tx *sql.Tx) error {
		prev, err := db.ClearFolderCover(tx, idA)
		if err != nil {
			return err
		}
		return db.MarkBlobTrashTx(tx, []string{prev})
	}); err != nil {
		t.Fatal(err)
	}
	s, _ = db.FolderSummary(idA)
	if s.CustomCover || !slices.Equal(s.CoverFileIDs, want) {
		t.Fatalf("清除后应回落拼贴: %+v", s)
	}

	// 空作品:拼贴空切片 + 无自有封面,渲染端自决
	idEmpty := mustFolder(t, db, "空作品")
	s, err = db.FolderSummary(idEmpty)
	if err != nil || len(s.CoverFileIDs) != 0 || s.CustomCover {
		t.Fatalf("空作品封面: %+v %v", s, err)
	}

	_ = ch2
}

// TestCoverSingleDerivedSlotFallsBack 回归:索引里只有 ./A/B/{1,2,3}.epub 时,
// B 正常三格宫格,A 唯一子条目是目录 B——若不回落,A 会满铺 B 的代表文件
// (1.epub)的封面,读起来像"该目录就是这个文件"。
func TestCoverSingleDerivedSlotFallsBack(t *testing.T) {
	db := newTestDB(t)
	idA := mustFolder(t, db, "A")
	idB := mustFolder(t, db, "A", "B")
	f1 := mustFileRow(t, db, row("1.epub", idB, false, "ready", 10, 1))
	f2 := mustFileRow(t, db, row("2.epub", idB, false, "ready", 11, 2))
	f3 := mustFileRow(t, db, row("3.epub", idB, false, "ready", 12, 3))
	for _, id := range []int64{f1, f2, f3} {
		mustThumb(t, db, id)
	}

	sums, err := db.FolderSummaries([]int64{idA, idB})
	if err != nil {
		t.Fatal(err)
	}
	if got := sums[idB].CoverFileIDs; !slices.Equal(got, []int64{f1, f2, f3}) {
		t.Fatalf("B 三格宫格: %v", got)
	}
	if got := sums[idA].CoverFileIDs; len(got) != 0 {
		t.Fatalf("A 单槽派生应回落空切片: %v", got)
	}
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

// TestMoveEntries 目录移动(文件+目录混合入口)的语义验收:改挂点一行,
// 子孙路径派生跟随;环/祖先-后代拒绝;空转 no-op 不计 revision;
// 根/软删/缺目标拒绝。
func TestMoveEntries(t *testing.T) {
	db := newTestDB(t)
	idA, idSub, _, _, _, notes := buildSeries(t, db)
	idB := mustFolder(t, db, "作品B")
	rev := func() uint64 {
		t.Helper()
		r, err := db.Revision()
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	// 目录移动:改挂点,子孙路径派生跟随(设定集及其 notes.txt 仍在其下)
	rev0 := rev()
	if err := db.MoveEntries(nil, []int64{idA}, idB); err != nil {
		t.Fatal(err)
	}
	if got := rev(); got != rev0+1 {
		t.Fatalf("移动应计一次 revision: %d → %d", rev0, got)
	}
	crumb, err := db.FolderPath(idSub)
	if err != nil {
		t.Fatal(err)
	}
	var segs []string
	for _, c := range crumb {
		if c.Name != "" {
			segs = append(segs, c.Name)
		}
	}
	if got, want := strings.Join(segs, "/"), "作品B/作品A/设定集"; got != want {
		t.Fatalf("子孙路径应跟随: got %q want %q", got, want)
	}
	if f, err := db.GetFile(notes); err != nil || f.FolderID != idSub {
		t.Fatalf("孙级文件挂点不应变: %+v %v", f, err)
	}

	// 环:dest 即自身 / dest 在子树内
	if err := db.MoveEntries(nil, []int64{idA}, idA); err == nil {
		t.Fatal("移进自身应报错")
	}
	if err := db.MoveEntries(nil, []int64{idA}, idSub); err == nil {
		t.Fatal("移进自身子树应报错")
	}

	// 选区内祖先-后代同移
	if err := db.MoveEntries(nil, []int64{idA, idSub}, idB); err == nil {
		t.Fatal("祖先与后代同移应报错")
	}

	// 空转 no-op:移回所在父,不计 revision
	rev1 := rev()
	if err := db.MoveEntries(nil, []int64{idA}, idB); err != nil {
		t.Fatal(err)
	}
	if got := rev(); got != rev1 {
		t.Fatalf("空转 no-op 不应计 revision: %d → %d", rev1, got)
	}

	// 根不可移;目标不存在拒绝;软删目录不可移
	if err := db.MoveEntries(nil, []int64{1}, idB); err == nil {
		t.Fatal("根目录不可移动")
	}
	if err := db.MoveEntries(nil, []int64{idA}, 9999); err == nil {
		t.Fatal("目标不存在应报错")
	}
	idC := mustFolder(t, db, "作品C")
	if err := db.SoftDeleteFolders([]int64{idC}); err != nil {
		t.Fatal(err)
	}
	if err := db.MoveEntries(nil, []int64{idC}, idB); err == nil {
		t.Fatal("软删目录不可移动")
	}
}

// TestMoveEntriesNameClash 撞名消解与文件/目录同名共存:目录撞目录 " (n)"
// 递增;目录与文件同名互不干扰(两套消解只查各的表);混合移动一次事务
// 恰计一次 revision。
func TestMoveEntriesNameClash(t *testing.T) {
	db := newTestDB(t)
	idA := mustFolder(t, db, "作品A")
	idB := mustFolder(t, db, "作品B")
	// B 下已有同名目录与同名文件(文件/目录同名可共存)
	_ = mustFolder(t, db, "作品B", "作品A")
	_ = mustFileRow(t, db, row("作品A", idB, false, "ready", 1, 1))
	// A 下备一个文件一起移
	f := mustFileRow(t, db, row("第01话.zip", idA, true, "ready", 9, 9))

	rev0, err := db.Revision()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MoveEntries([]int64{f}, []int64{idA}, idB); err != nil {
		t.Fatal(err)
	}
	rev1, _ := db.Revision()
	if rev1 != rev0+1 {
		t.Fatalf("混合移动应恰计一次 revision: %d → %d", rev0, rev1)
	}

	// 落点:目录消解为 "作品A (1)",原名目录与同名文件原样不动
	entries, err := db.ListFolder(idB)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = true
	}
	for _, want := range []string{"作品A", "作品A (1)"} {
		if !names[want] {
			t.Fatalf("缺条目 %q: %v", want, names)
		}
	}
	// 被移文件消解:B 下无同名文件,保持原名
	got, err := db.GetFile(f)
	if err != nil || got.FolderID != idB || got.Name != "第01话.zip" {
		t.Fatalf("文件落点: %+v %v", got, err)
	}
}

// TestMetaSurvivesSnapshot 验收项:元数据与目录封面经快照(备份同款
// VACUUM INTO)替换后仍在。
func TestMetaSurvivesSnapshot(t *testing.T) {
	db := newTestDB(t)
	idA := mustFolder(t, db, "作品A")
	note := "作者:某人"
	if err := db.UpdateFolderMeta(idA, FolderMetaUpdate{Note: &note, Tags: []string{"科幻"}}); err != nil {
		t.Fatal(err)
	}
	mustFolderCover(t, db, idA, CoverReady)

	snap := fmt.Sprintf("%s-snap.db", db.Path)
	if err := db.SnapshotTo(snap); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceWith(snap); err != nil {
		t.Fatal(err)
	}
	m, err := db.GetFolderMeta(idA)
	if err != nil || m.Note != note || !slices.Equal(m.Tags, []string{"科幻"}) {
		t.Fatalf("快照替换后元数据丢失: %+v %v", m, err)
	}
	if _, err := db.GetReadyFolderCover(idA); err != nil {
		t.Fatalf("快照替换后目录封面引用丢失: %v", err)
	}
	s, err := db.FolderSummary(idA)
	if err != nil || !s.CustomCover {
		t.Fatalf("快照替换后封面链失效: %+v %v", s, err)
	}
}

// TestRenameFolder 目录重命名(TODO-19):往返、同名 no-op 不计 revision、
// 撞名报错不消解、跨级同名放行、根/软删/非法段拒绝。
// TestRenameFile 文件重命名:纯索引零流量,语义对齐 RenameFolder
// (同名 no-op 不计 revision;真改名计一次;撞名报错;非法名拒绝;
// 软删文件不可改名;跨级同名放行)。
func TestRenameFile(t *testing.T) {
	db := newTestDB(t)
	idA := mustFolder(t, db, "作品A")
	idB := mustFolder(t, db, "作品B")
	f1 := mustFileRow(t, db, row("第01话.epub", idA, false, "ready", 10, 1))
	f2 := mustFileRow(t, db, row("第02话.epub", idA, false, "ready", 11, 2))
	other := mustFileRow(t, db, row("同名.epub", idB, false, "ready", 12, 3))

	rev0, _ := db.Revision()
	if err := db.RenameFile(f1, "第01话.epub"); err != nil {
		t.Fatal(err)
	}
	if rev, _ := db.Revision(); rev != rev0 {
		t.Fatalf("同名 no-op 不应计 revision: %d → %d", rev0, rev)
	}

	if err := db.RenameFile(f1, "改名话.epub"); err != nil {
		t.Fatal(err)
	}
	if rev, _ := db.Revision(); rev != rev0+1 {
		t.Fatalf("真改名应计一次 revision: %d → %d", rev0, rev)
	}
	got, err := db.GetFile(f1)
	if err != nil || got.Name != "改名话.epub" {
		t.Fatalf("改名后文件名不符: %+v %v", got, err)
	}

	// 撞名:报错不消解
	if err := db.RenameFile(f2, "改名话.epub"); err == nil {
		t.Fatal("撞名应报错而非自动消解")
	}
	// 跨级同名放行(唯一性只约束同目录,与上传消解同口径)
	if err := db.RenameFile(other, "改名话.epub"); err != nil {
		t.Fatalf("跨级同名应放行: %v", err)
	}

	// 非法段拒绝
	for _, bad := range []string{"", "a/b", "..", "."} {
		if err := db.RenameFile(f1, bad); err == nil {
			t.Fatalf("非法名 %q 应报错", bad)
		}
	}
	// 软删文件不可改名
	if err := db.SoftDeleteFiles([]int64{f2}); err != nil {
		t.Fatal(err)
	}
	if err := db.RenameFile(f2, "删后再改.epub"); err == nil {
		t.Fatal("软删文件应不可改名")
	}
}

func TestRenameFolder(t *testing.T) {
	db := newTestDB(t)
	idA := mustFolder(t, db, "作品A")
	idB := mustFolder(t, db, "作品B")

	names := func() []string {
		entries, err := db.ListFolder(1)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range entries {
			if e.IsFolder {
				out = append(out, e.Name)
			}
		}
		return out
	}

	rev0, _ := db.Revision()
	if err := db.RenameFolder(idA, "作品A"); err != nil {
		t.Fatal(err)
	}
	if rev, _ := db.Revision(); rev != rev0 {
		t.Fatalf("同名 no-op 不应计 revision: %d → %d", rev0, rev)
	}

	if err := db.RenameFolder(idA, "改名后的A"); err != nil {
		t.Fatal(err)
	}
	if rev, _ := db.Revision(); rev != rev0+1 {
		t.Fatalf("真改名应计一次 revision: %d → %d", rev0, rev)
	}
	got := names()
	if !slices.Contains(got, "改名后的A") || slices.Contains(got, "作品A") {
		t.Fatalf("改名后目录名不符: %v", got)
	}

	// 撞名:报错不消解(重命名是显式单发动作)
	if err := db.RenameFolder(idB, "改名后的A"); err == nil {
		t.Fatal("撞名应报错而非自动消解")
	}

	// 跨级同名放行:唯一性只约束同父
	sub := mustFolder(t, db, "作品B", "子层")
	if err := db.RenameFolder(sub, "改名后的A"); err != nil {
		t.Fatalf("跨级同名应放行: %v", err)
	}

	// 根/软删/非法段拒绝
	if err := db.RenameFolder(rootFolderID, "新根"); err == nil {
		t.Fatal("根目录不可重命名")
	}
	for _, bad := range []string{"", "a/b", "..", "."} {
		if err := db.RenameFolder(idA, bad); err == nil {
			t.Fatalf("非法目录名 %q 应拒绝", bad)
		}
	}
	if err := db.SoftDeleteFolders([]int64{idA}); err != nil {
		t.Fatal(err)
	}
	if err := db.RenameFolder(idA, "又改名"); err == nil {
		t.Fatal("软删后的目录不可重命名")
	}
	// 不存在的目录
	if err := db.RenameFolder(9999, "x"); err == nil {
		t.Fatal("不存在的目录应报错")
	}
}

var _ = sql.NullString{} // 保留 import:row() 的调用方将来可能需要显式 NULL 字段
