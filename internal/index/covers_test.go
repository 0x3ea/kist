package index

// TODO-10 封面出库的索引侧测试:轻引用往返、上传状态不可见性、
// prevBlob 闭环(替换/清除同事务 trash 旧 blob)、出站箱级联、v5 升级。

import (
	"database/sql"
	"fmt"
	"testing"
)

func coverRow(fileID int64, blob string, state string) CoverRow {
	return CoverRow{
		FileID: fileID, BlobName: blob, Size: 128,
		Width: 8, Height: 8, Mime: "image/jpeg",
		Source: CoverCustom, State: state, CreatedAt: 1,
	}
}

func TestCoverRoundTrip(t *testing.T) {
	db := newTestDB(t)
	id := mustFile(t, db, 1, "照片.jpg", "")

	// uploading 引用不可见:产物还在出站箱,此时可取等于逼 GUI 去远端拉空
	if err := db.WithTx(func(tx *sql.Tx) error {
		_, err := db.PutCover(tx, coverRow(id, "cover-a", CoverUploading))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetReadyCover(id); err != sql.ErrNoRows {
		t.Fatalf("uploading 引用应不可见: %v", err)
	}
	if has, err := db.HasCover(id); err != nil || has {
		t.Fatalf("uploading 时 HasCover 应为假: %v %v", has, err)
	}

	// 翻转为 ready 后可见,字段全等
	if err := db.MarkUploaded("cover-a", 1); err != nil {
		t.Fatal(err)
	}
	c, err := db.GetReadyCover(id)
	if err != nil {
		t.Fatal(err)
	}
	if c.FileID != id || c.BlobName != "cover-a" || c.Size != 128 || c.Width != 8 ||
		c.Mime != "image/jpeg" || c.Source != CoverCustom || c.State != CoverReady {
		t.Fatalf("字段全等: %+v", c)
	}
	if has, err := db.HasCover(id); err != nil || !has {
		t.Fatalf("ready 后 HasCover 应为真: %v %v", has, err)
	}
}

func TestCoverReplaceReturnsPrev(t *testing.T) {
	db := newTestDB(t)
	id := mustFile(t, db, 1, "照片.jpg", "")

	var prev1, prev2, prev3, prev4 string
	err := db.WithTx(func(tx *sql.Tx) error {
		var e1, e2, e3, e4 error
		prev1, e1 = db.PutCover(tx, coverRow(id, "cover-a", CoverReady))
		prev2, e2 = db.PutCover(tx, coverRow(id, "cover-b", CoverReady))
		prev3, e3 = db.ClearCover(tx, id)
		prev4, e4 = db.ClearCover(tx, id)
		if e1 != nil {
			return e1
		}
		if e2 != nil {
			return e2
		}
		if e3 != nil {
			return e3
		}
		return e4
	})
	if err != nil {
		t.Fatal(err)
	}
	if prev1 != "" {
		t.Fatalf("首写应无旧引用: %q", prev1)
	}
	if prev2 != "cover-a" {
		t.Fatalf("替换应返回旧 blob 名: %q", prev2)
	}
	if prev3 != "cover-b" {
		t.Fatalf("清除应返回现 blob 名: %q", prev3)
	}
	if prev4 != "" {
		t.Fatalf("再清应返回空: %q", prev4)
	}
}

// TestReplaceCoverTrashesOldBlob 引用换手与旧字节 trash 必须同一事务:
// 中途出错时整个事务(引用+trash)一并回滚,不得留半套。
func TestReplaceCoverTrashesOldBlob(t *testing.T) {
	db := newTestDB(t)
	id := mustFile(t, db, 1, "照片.jpg", "")

	if err := db.WithTx(func(tx *sql.Tx) error {
		db.RegisterBlob(tx, "cover-a", "cover", 128)
		db.RegisterBlob(tx, "cover-b", "cover", 128)
		_, err := db.PutCover(tx, coverRow(id, "cover-a", CoverReady))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// 同事务替换 + trash:正常路径
	if err := db.WithTx(func(tx *sql.Tx) error {
		prev, err := db.PutCover(tx, coverRow(id, "cover-b", CoverReady))
		if err != nil {
			return err
		}
		return db.MarkBlobTrashTx(tx, []string{prev})
	}); err != nil {
		t.Fatal(err)
	}
	states, err := db.ListBlobStates()
	if err != nil {
		t.Fatal(err)
	}
	if states["cover-a"] != "trash" || states["cover-b"] != "active" {
		t.Fatalf("旧 blob 应 trash、新 blob 应 active: %v", states)
	}

	// 回滚路径:替换+trash 后事务失败,一切撤销,引用保持 cover-b
	if err := db.WithTx(func(tx *sql.Tx) error {
		prev, err := db.PutCover(tx, coverRow(id, "cover-a", CoverReady))
		if err != nil {
			return err
		}
		if err := db.MarkBlobTrashTx(tx, []string{prev}); err != nil {
			return err
		}
		return errForced
	}); err == nil {
		t.Fatal("应返回注入的错误")
	}
	c, err := db.GetReadyCover(id)
	if err != nil || c.BlobName != "cover-b" {
		t.Fatalf("回滚后引用应保持 cover-b: %+v %v", c, err)
	}
	states, _ = db.ListBlobStates()
	if states["cover-a"] != "trash" || states["cover-b"] != "active" {
		t.Fatalf("回滚不应改变 trash/active 态: %v", states)
	}
}

var errForced = fmt.Errorf("forced rollback")

func TestCoverBlobNamesOf(t *testing.T) {
	db := newTestDB(t)
	idA := mustFile(t, db, 1, "a.jpg", "")
	idB := mustFile(t, db, 1, "b.jpg", "")
	idC := mustFile(t, db, 1, "c.txt", "")
	for _, id := range []int64{idA, idB} {
		if err := db.WithTx(func(tx *sql.Tx) error {
			_, err := db.PutCover(tx, coverRow(id, fmt.Sprintf("cover-%d", id), CoverReady))
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	names, err := db.CoverBlobNamesOf([]int64{idA, idB, idC})
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 {
		t.Fatalf("应取到 2 个封面 blob 名: %v", names)
	}
	if empty, err := db.CoverBlobNamesOf(nil); err != nil || len(empty) != 0 {
		t.Fatalf("空入参: %v %v", empty, err)
	}
}

// TestOutboxCoverAccounting 出站箱挂账的三条封面路径:
// defer(文件+封面同账)→ push 收账双翻转 / discard 级联 / 仅封面挂账。
func TestOutboxCoverAccounting(t *testing.T) {
	db := newTestDB(t)
	folderID := int64(1)

	// —— defer:文件 uploading + 封面 uploading + 两笔 pending
	var fileID int64
	err := db.WithTx(func(tx *sql.Tx) error {
		var err error
		fileID, err = db.InsertFile(tx, FileRow{
			UUID: "u1", FolderID: folderID, Name: "图.jpg", Size: 10, CipherSize: 200,
			SHA256: "abc", ChunkSize: 4096, BlobName: "file-blob",
			State: "uploading", ModifiedAt: 1,
		})
		if err != nil {
			return err
		}
		if _, err := db.PutCover(tx, coverRow(fileID, "cover-blob", CoverUploading)); err != nil {
			return err
		}
		if err := db.RegisterBlobPending(tx, "file-blob", "file", 200); err != nil {
			return err
		}
		return db.RegisterBlobPending(tx, "cover-blob", "cover", 128)
	})
	if err != nil {
		t.Fatal(err)
	}

	covers, err := db.ListUploadingCovers()
	if err != nil || len(covers) != 1 || covers[0].BlobName != "cover-blob" {
		t.Fatalf("ListUploadingCovers: %+v %v", covers, err)
	}

	// push 收账:按 blob 名逐对象 MarkUploaded(文件推完收文件账、封面推完
	// 收封面账),各自翻转 files/covers/blobs 三表
	if err := db.MarkUploaded("file-blob", 7); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkUploaded("cover-blob", 7); err != nil {
		t.Fatal(err)
	}
	f, err := db.GetFile(fileID)
	if err != nil || f.State != "ready" {
		t.Fatalf("文件应转 ready: %+v %v", f, err)
	}
	if _, err := db.GetReadyCover(fileID); err != nil {
		t.Fatalf("封面应转 ready: %v", err)
	}
	states, _ := db.ListBlobStates()
	if states["file-blob"] != "active" {
		t.Fatalf("file-blob 应 active: %v", states)
	}

	// —— 仅封面挂账(GUI 导入回退:文件已 ready):导入顶掉旧封面
	// (prevBlob 同事务 trash),再入一笔新挂账;discard 只动封面
	var prev string
	if err := db.WithTx(func(tx *sql.Tx) error {
		var err error
		prev, err = db.PutCover(tx, coverRow(fileID, "cover-2", CoverUploading))
		if err != nil {
			return err
		}
		if err := db.MarkBlobTrashTx(tx, []string{prev}); err != nil {
			return err
		}
		return db.RegisterBlobPending(tx, "cover-2", "cover", 128)
	}); err != nil {
		t.Fatal(err)
	}
	if prev != "cover-blob" {
		t.Fatalf("替换封面应返回旧 blob 名: %q", prev)
	}
	if _, err := db.DiscardPending("cover-2"); err != nil {
		t.Fatalf("仅封面挂账的 discard: %v", err)
	}
	if has, _ := db.HasCover(fileID); has {
		t.Fatal("discard 后封面引用应消失")
	}
	if f.State != "ready" {
		t.Fatal("仅封面 discard 不得波及文件")
	}
	states, _ = db.ListBlobStates()
	if states["cover-blob"] != "trash" || states["file-blob"] != "active" || len(states) != 2 {
		t.Fatalf("被顶掉且被弃的旧封面 blob 应为 trash、文件账应 active: %v", states)
	}

	// —— defer 全账 discard:一笔放弃 = 文件及其唯一封面(引用行级联消失、
	// blobs 行显式删),返回的封面 blob 名供调用方清出站箱产物
	if err := db.WithTx(func(tx *sql.Tx) error {
		var err error
		_, err = db.PutCover(tx, coverRow(fileID, "cover-3", CoverUploading))
		if err != nil {
			return err
		}
		return db.RegisterBlobPending(tx, "cover-3", "cover", 128)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetFileState(fileID, "uploading"); err != nil {
		t.Fatal(err)
	}
	coverBlobs, err := db.DiscardPending("file-blob")
	if err != nil {
		t.Fatal(err)
	}
	if len(coverBlobs) != 1 || coverBlobs[0] != "cover-3" {
		t.Fatalf("应返回该文件的封面 blob 名: %v", coverBlobs)
	}
	if _, err := db.GetFile(fileID); err != sql.ErrNoRows {
		t.Fatalf("文件行应被硬删: %v", err)
	}
	if has, _ := db.HasCover(fileID); has {
		t.Fatal("文件行硬删后封面应随外键级联消失")
	}
	states, _ = db.ListBlobStates()
	if len(states) != 1 || states["cover-blob"] != "trash" {
		t.Fatalf("应只剩先前被顶掉的 trash 行: %v", states)
	}

	// —— 无账可弃必须拒绝
	if _, err := db.DiscardPending("不存在"); err == nil {
		t.Fatal("无账 discard 应报错")
	}
}

// TestV5UpgradeFromV4 模拟 v4 老库(版本号回退 + 拆掉 covers 表):
// 重开时 migrate 应补建 covers,legacy thumbnails 数据无损可读。
func TestV5UpgradeFromV4(t *testing.T) {
	db := newTestDB(t)
	folderID := mustFolder(t, db, "漫画库")
	fileID := mustFileRow(t, db, row("话1.pack", folderID, true, "ready", 1000, 1000))
	mustThumb(t, db, fileID) // legacy 行随库留存
	path := db.Path
	db.Close()

	d, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DROP TABLE covers`,
		`PRAGMA user_version = 4`,
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
	// legacy 回退:升级后无 covers 行,HasCover 靠 thumbnails 兜底为真
	if has, err := db2.HasCover(fileID); err != nil || !has {
		t.Fatalf("升级后 legacy 封面应可判定: %v %v", has, err)
	}
	if _, _, err := db2.GetLegacyThumbnail(fileID); err != nil {
		t.Fatalf("legacy 缩略图应可读: %v", err)
	}
	// covers 表即刻可用
	if err := db2.WithTx(func(tx *sql.Tx) error {
		_, err := db2.PutCover(tx, coverRow(fileID, "cover-new", CoverReady))
		return err
	}); err != nil {
		t.Fatalf("升级后 v5 表可写: %v", err)
	}
}

// TestCoverSurvivesSnapshot covers 行随快照替换存活(index.enc LWW 同步的地基)。
func TestCoverSurvivesSnapshot(t *testing.T) {
	db := newTestDB(t)
	folderID := mustFolder(t, db, "作品")
	id := mustFile(t, db, folderID, "封面.jpg", "")
	if err := db.WithTx(func(tx *sql.Tx) error {
		_, err := db.PutCover(tx, coverRow(id, "cover-a", CoverReady))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	snap := fmt.Sprintf("%s-snap.db", db.Path)
	if err := db.SnapshotTo(snap); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceWith(snap); err != nil {
		t.Fatal(err)
	}
	c, err := db.GetReadyCover(id)
	if err != nil || c.BlobName != "cover-a" || c.Source != CoverCustom {
		t.Fatalf("快照替换后封面引用丢失: %+v %v", c, err)
	}
}

// TestCoverLegacyFallbackInSummary 仅有 legacy thumbnails 行时,聚合的封面
// 链仍应命中(迁移完成前的过渡期回归)。配一个无缩略图的第二话占位:
// 单槽派生会回落空切片,双槽才能证明 legacy 行真的进了链。
func TestCoverLegacyFallbackInSummary(t *testing.T) {
	db := newTestDB(t)
	folderID := mustFolder(t, db, "作品")
	id := mustFile(t, db, folderID, "第一话", "")
	mustThumb(t, db, id)
	_ = mustFile(t, db, folderID, "第二话", "")

	sums, err := db.FolderSummaries([]int64{folderID})
	if err != nil {
		t.Fatal(err)
	}
	s := sums[folderID]
	if len(s.CoverFileIDs) != 2 || s.CoverFileIDs[0] != id || s.CoverFileIDs[1] != 0 {
		t.Fatalf("legacy 行应进封面链: %v", s.CoverFileIDs)
	}
}
