package main

// app_test.go — GUI 绑定层的 headless 测试:不碰 Wails ctx 与 dav,只验证
// 纯索引编排(合成视图/路径拆分/删除/移动/元数据绑定)与错误码包装。
// 解锁/传输/备份等网络路径属于手测清单(phase-7 验收标准)。

import (
	"database/sql"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kist/internal/config"
	"kist/internal/errs"
	"kist/internal/index"
)

// newTestApp 构造只装配了 db 的 App(其余零值;emit* 路径对 nil ctx/nil cfg 安全)。
func newTestApp(t *testing.T) *App {
	t.Helper()
	t.Setenv("KIST_HOME", t.TempDir())
	// 活动盘档案 + 对应库文件(TODO-21:索引路径锚定盘 ID)
	id := config.NewDriveID()
	if err := config.Save(&config.StoredConfig{
		Drives: []config.Drive{{ID: id, Name: "测试盘", URL: "https://dav.example.com/dav", Username: "u"}},
		Active: id,
	}); err != nil {
		t.Fatalf("config.Save: %v", err)
	}
	db, err := index.Open(config.DriveIndexPath(id))
	if err != nil {
		t.Fatalf("index.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	a := NewApp()
	a.cfg = &config.StoredConfig{Drives: []config.Drive{{ID: id, Name: "测试盘", URL: "https://dav.example.com/dav", Username: "u"}}, Active: id}
	a.db = db
	return a
}

var seedSeq int

// seedFolder 建目录路径返回末端 id;seedFile 插入文件行返回 id。
func seedFolder(t *testing.T, db *index.DB, segs ...string) int64 {
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

func seedFile(t *testing.T, db *index.DB, folderID int64, name string, pack bool, size int64) int64 {
	t.Helper()
	seedSeq++
	row := index.FileRow{
		UUID: fmt.Sprintf("uuid-%d", seedSeq), FolderID: folderID, Name: name,
		Size: size, CipherSize: size + 16, SHA256: "sha-" + name, ChunkSize: 4096,
		BlobName: fmt.Sprintf("blob-%d", seedSeq), Pack: pack,
		ModifiedAt: 1700000000,
	}
	var id int64
	if err := db.WithTx(func(tx *sql.Tx) error {
		if err := db.RegisterBlob(tx, row.BlobName, "file", row.CipherSize); err != nil {
			return err
		}
		var err error
		id, err = db.InsertFile(tx, row)
		return err
	}); err != nil {
		t.Fatalf("InsertFile(%s): %v", name, err)
	}
	return id
}

func seedThumb(t *testing.T, db *index.DB, fileID int64) {
	t.Helper()
	if err := db.WithTx(func(tx *sql.Tx) error {
		return db.PutThumbnail(tx, fileID, []byte{1}, 8, 8, "image/jpeg")
	}); err != nil {
		t.Fatalf("PutThumbnail(%d): %v", fileID, err)
	}
}

// TestWrapCodedError 错误码包装:前端靠 "[CODE] Msg" 前缀反解。
func TestWrapCodedError(t *testing.T) {
	a := NewApp()
	if err := a.wrap(nil); err != nil {
		t.Fatalf("wrap(nil) 应为 nil,得到 %v", err)
	}
	err := a.wrap(errs.New(errs.AuthFailed, "口令错误"))
	if err.Error() != "[AUTH_FAILED] 口令错误" {
		t.Fatalf("AppError 包装格式不符:%q", err.Error())
	}
	// 未知错误归类 INTERNAL(errs.From 的兜底)
	err = a.wrap(errors.New("boom"))
	if !strings.HasPrefix(err.Error(), "[INTERNAL] boom") {
		t.Fatalf("普通错误应归 INTERNAL:%q", err.Error())
	}
	// 已包装的不重复包
	err2 := a.wrap(err)
	if err2.Error() != err.Error() {
		t.Fatalf("重复包装:%q vs %q", err2.Error(), err.Error())
	}
}

// TestListFolderComposite 合成视图:面包屑 + 条目(目录在前)+ 子目录摘要批量。
func TestListFolderComposite(t *testing.T) {
	a := newTestApp(t)
	idA := seedFolder(t, a.db, "合集A")
	idSub := seedFolder(t, a.db, "合集A", "子目录")
	seedFile(t, a.db, idA, "第01话.zip", true, 100)
	seedFile(t, a.db, idA, "第02话.zip", true, 200)
	seedFile(t, a.db, idSub, "插图.png", false, 50)

	v, err := a.ListFolder(0) // 0 归一化为根
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Crumbs) != 1 || v.Crumbs[0].ID != 1 {
		t.Fatalf("根面包屑应为单根节点:%+v", v.Crumbs)
	}
	if len(v.Entries) != 1 || !v.Entries[0].IsFolder || v.Entries[0].Name != "合集A" {
		t.Fatalf("根条目应只有目录合集A:%+v", v.Entries)
	}
	s, ok := v.Summaries[idA]
	if !ok || s.FileCount != 3 || s.PackCount != 2 || s.TotalSize != 350 {
		t.Fatalf("合集A 摘要不符:%+v", s)
	}

	// 进入合集A:子目录摘要来自批量接口
	v, err = a.ListFolder(idA)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Entries) != 3 { // 子目录 + 两个 pack
		t.Fatalf("合集A 条目数应为 3:%+v", v.Entries)
	}
	if !v.Entries[0].IsFolder {
		t.Fatalf("目录应排在文件前:%+v", v.Entries)
	}
	s, ok = v.Summaries[idSub]
	if !ok || s.FileCount != 1 {
		t.Fatalf("子目录摘要缺失:%+v", v.Summaries)
	}

	// 空目录:null 归一为数组(Go 侧保证非 nil)
	v, err = a.ListFolder(idSub)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Entries) != 1 || v.Entries == nil {
		t.Fatalf("子目录应含 1 文件:%+v", v.Entries)
	}
}

// TestSearchAllComposite 搜索合成:目录命中(名/tag)+ 文件命中(名/备注)。
func TestSearchAllComposite(t *testing.T) {
	a := newTestApp(t)
	idA := seedFolder(t, a.db, "银河铁道")
	ch := seedFile(t, a.db, idA, "夜行列车.zip", true, 10)
	if err := a.db.SetNote(ch, "作者:宫泽贤治"); err != nil {
		t.Fatal(err)
	}
	tag := "科幻"
	if err := a.db.UpdateFolderMeta(idA, index.FolderMetaUpdate{Tags: []string{tag}}); err != nil {
		t.Fatal(err)
	}

	// 按目录名命中
	v, err := a.SearchAll("银河", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Folders) != 1 || v.Folders[0].Name != "银河铁道" {
		t.Fatalf("目录命中不符:%+v", v.Folders)
	}
	// 按 tag 命中
	v, err = a.SearchAll("科幻", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Folders) != 1 {
		t.Fatalf("tag 命中不符:%+v", v.Folders)
	}
	// 按备注关键词命中文件
	v, err = a.SearchAll("宫泽", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Files) != 1 || v.Files[0].Name != "夜行列车.zip" {
		t.Fatalf("备注命中不符:%+v", v.Files)
	}
	// 无命中:空数组而非 null
	v, err = a.SearchAll("不存在的词", 50)
	if err != nil {
		t.Fatal(err)
	}
	if v.Folders == nil || v.Files == nil || len(v.Folders) != 0 || len(v.Files) != 0 {
		t.Fatalf("无命中应返回空数组:%+v", v)
	}
}

// TestSplitVirtualPath 路径拆分:容忍 "./" 与空段,拒绝 ".."。
func TestSplitVirtualPath(t *testing.T) {
	segs, err := splitVirtualPath("./合集//子目录/.")
	if err != nil || len(segs) != 2 || segs[0] != "合集" || segs[1] != "子目录" {
		t.Fatalf("拆分不符:%v %v", segs, err)
	}
	if _, err := splitVirtualPath("/a/../b"); err == nil {
		t.Fatal("\"..\" 应被拒绝")
	}
}

// TestEnsureFolderAndMoveFiles 建目录 + 纯索引移动(MoveDialog 的后端链路)。
func TestEnsureFolderAndMoveFiles(t *testing.T) {
	a := newTestApp(t)
	idA := seedFolder(t, a.db, "合集A")
	f := seedFile(t, a.db, 1, "流浪文件.zip", true, 10)

	dest, err := a.EnsureFolder("/合集A/新家")
	if err != nil {
		t.Fatal(err)
	}
	if dest == 1 || dest == idA {
		t.Fatalf("EnsureFolder 应建到最深一级:%d", dest)
	}
	if err := a.MoveFiles([]int64{f}, dest); err != nil {
		t.Fatal(err)
	}
	// 幂等:同一路径再建返回同一 id
	dest2, err := a.EnsureFolder("合集A/新家")
	if err != nil || dest2 != dest {
		t.Fatalf("幂等不符:%d vs %d (%v)", dest2, dest, err)
	}
	// 文件确实搬过去了
	v, err := a.ListFolder(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Entries) != 1 || v.Entries[0].Name != "流浪文件.zip" {
		t.Fatalf("移动后落点不符:%+v", v.Entries)
	}
}

// TestDeleteEntries 删除:文件走软删+blob 标 trash;目录只隐藏子树。
func TestDeleteEntries(t *testing.T) {
	a := newTestApp(t)
	idA := seedFolder(t, a.db, "合集A")
	f := seedFile(t, a.db, idA, "待删.zip", false, 10)
	seedFile(t, a.db, idA, "留存.zip", false, 20)
	idB := seedFolder(t, a.db, "空目录B")

	if err := a.DeleteEntries([]int64{f}, []int64{idB}); err != nil {
		t.Fatal(err)
	}
	// 文件从列表消失
	v, err := a.ListFolder(idA)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Entries) != 1 || v.Entries[0].Name != "留存.zip" {
		t.Fatalf("删除后剩余不符:%+v", v.Entries)
	}
	// blob 已标 trash(gc 的待删依据)
	states, err := a.db.ListBlobStates()
	if err != nil {
		t.Fatal(err)
	}
	d, err := a.FileInfo(f)
	if err != nil {
		t.Fatal(err)
	}
	if states[d.BlobName] != "trash" {
		t.Fatalf("blob 应为 trash:%+v", states)
	}
	// 目录从根消失
	v, err = a.ListFolder(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range v.Entries {
		if e.ID == idB {
			t.Fatal("已删目录不应出现在列表")
		}
	}
	// 空选择拒绝
	if err := a.DeleteEntries(nil, nil); err == nil {
		t.Fatal("空选择应报错")
	}
}

// TestFolderMetaBinding 元数据绑定透传:Tags nil 归一;指针语义(nil=不动/零值=清除)。
func TestFolderMetaBinding(t *testing.T) {
	a := newTestApp(t)
	idA := seedFolder(t, a.db, "合集A")

	m, err := a.GetFolderMeta(idA)
	if err != nil {
		t.Fatal(err)
	}
	if m.Tags == nil {
		t.Fatal("Tags 应归一为空数组(前端遍历安全)")
	}
	note := "试读:第一卷"
	cover := int64(0)
	if err := a.UpdateFolderMeta(idA, index.FolderMetaUpdate{Note: &note, Tags: []string{"连载"}}); err != nil {
		t.Fatal(err)
	}
	m, err = a.GetFolderMeta(idA)
	if err != nil {
		t.Fatal(err)
	}
	if m.Note != note || len(m.Tags) != 1 || m.Tags[0] != "连载" {
		t.Fatalf("写入后读取不符:%+v", m)
	}
	// Note=nil 不动,Tags 全量覆盖
	if err := a.UpdateFolderMeta(idA, index.FolderMetaUpdate{Tags: []string{}}); err != nil {
		t.Fatal(err)
	}
	m, _ = a.GetFolderMeta(idA)
	if m.Note != note {
		t.Fatalf("nil Note 不应改动:%+v", m)
	}
	if len(m.Tags) != 0 {
		t.Fatalf("空 Tags 应全量清空:%+v", m.Tags)
	}
	// Cover 指向零值=清除(此处本来就零,验证不报错即可)
	if err := a.UpdateFolderMeta(idA, index.FolderMetaUpdate{Cover: &cover}); err != nil {
		t.Fatal(err)
	}
}

// TestGetThumbnailAndFileInfo 缩略图缺失 → NOT_FOUND 码;详情投影含路径与指针时间。
func TestGetThumbnailAndFileInfo(t *testing.T) {
	a := newTestApp(t)
	idA := seedFolder(t, a.db, "合集A")
	f := seedFile(t, a.db, idA, "封面图.png", false, 10)

	// 无缩略图:错误串带 [NOT_FOUND] 前缀
	_, err := a.GetThumbnail(f)
	if err == nil || !strings.HasPrefix(err.Error(), "[NOT_FOUND]") {
		t.Fatalf("缺失缩略图应 NOT_FOUND:%v", err)
	}

	seedThumb(t, a.db, f)
	td, err := a.GetThumbnail(f)
	if err != nil || len(td.Data) != 1 || td.Mime != "image/jpeg" {
		t.Fatalf("缩略图读取不符:%+v %v", td, err)
	}

	d, err := a.FileInfo(f)
	if err != nil {
		t.Fatal(err)
	}
	if d.Path != "/合集A/封面图.png" {
		t.Fatalf("虚拟路径不符:%q", d.Path)
	}
	if !d.HasThumb {
		t.Fatal("HasThumb 应为真")
	}
	if d.EncryptedAt != nil || d.UploadedAt != nil {
		t.Fatalf("未上传文件的时间指针应为 nil:%+v", d)
	}
}

// makeCoverJPEG 生成一张小渐变 JPEG(封面导入绑定的真实图片输入)。
func makeCoverJPEG(t *testing.T, dir string) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 4), G: uint8(y * 4), B: 128, A: 255})
		}
	}
	p := filepath.Join(dir, "cover.jpg")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestFileMetaAndCoverBinding 文件元数据绑定(TODO-17):meta 往返、tag 进
// 搜索面、封面导入/覆盖/清除、非图片报错不静默。
func TestFileMetaAndCoverBinding(t *testing.T) {
	a := newTestApp(t)
	folder := seedFolder(t, a.db, "小说合集")
	f := seedFile(t, a.db, folder, "第一卷.epub", false, 100)

	// meta 往返:note + tags(含 trim/去重)
	note := "作者:某人"
	if err := a.UpdateFileMeta(f, index.FileMetaUpdate{Note: &note, Tags: []string{" 科幻 ", "科幻"}}); err != nil {
		t.Fatal(err)
	}
	m, err := a.GetFileMeta(f)
	if err != nil || m.Note != note {
		t.Fatalf("note 往返:%+v %v", m, err)
	}
	if len(m.Tags) != 1 || m.Tags[0] != "科幻" {
		t.Fatalf("tags 应去重去空白:%v", m.Tags)
	}

	// 文件 tag 进搜索面(SearchAll 与 CLI search 同一入口)
	v, err := a.SearchAll("某人", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Files) != 1 || v.Files[0].ID != f {
		t.Fatalf("tag 应命中文件:%+v", v.Files)
	}
	if len(v.Files[0].Tags) != 1 || v.Files[0].Tags[0] != "科幻" {
		t.Fatalf("搜索结果应回填 tags:%+v", v.Files[0])
	}

	// 封面导入:真实 JPEG → thumbnails 落库,GetThumbnail 可读
	cover := makeCoverJPEG(t, t.TempDir())
	if err := a.SetFileCover(f, cover); err != nil {
		t.Fatal(err)
	}
	td, err := a.GetThumbnail(f)
	if err != nil || td.Mime != "image/jpeg" || len(td.Data) == 0 {
		t.Fatalf("导入后的封面应可读:%+v %v", td, err)
	}

	// 非图片:显式用户动作,错误上抛(带 BAD_CONFIG 前缀)而非静默
	notImg := filepath.Join(t.TempDir(), "x.txt")
	if err := os.WriteFile(notImg, []byte("不是图片"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.SetFileCover(f, notImg); err == nil || !strings.HasPrefix(err.Error(), "[BAD_CONFIG]") {
		t.Fatalf("非图片应 BAD_CONFIG 报错:%v", err)
	}

	// 清除封面:空路径 → NOT_FOUND;pack 覆盖确认属前端职责,绑定只管语义
	if err := a.SetFileCover(f, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := a.GetThumbnail(f); err == nil || !strings.HasPrefix(err.Error(), "[NOT_FOUND]") {
		t.Fatalf("清除后应 NOT_FOUND:%v", err)
	}

	// 已删除文件:导入与 meta 写都拒绝
	if err := a.DeleteEntries([]int64{f}, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.SetFileCover(f, cover); err == nil || !strings.HasPrefix(err.Error(), "[NOT_FOUND]") {
		t.Fatalf("已删除文件的封面操作应 NOT_FOUND:%v", err)
	}
	if err := a.UpdateFileMeta(f, index.FileMetaUpdate{Tags: []string{"x"}}); err == nil {
		t.Fatal("已删除文件的 meta 写应报错")
	}
}
