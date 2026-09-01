package e2e

// TODO-16 的端到端验收:真实 put 产生 pack 与缩略图后,meta/mv/摘要/封面链
// 在完整管线(远端 blob + 索引 + 备份恢复)上的行为。

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"kist/internal/backup"
	"kist/internal/index"
)

// putSeries 上传标准两话作品:2 个 pack(JPEG 首页带缩略图)+ cover.jpg 散文件。
func putSeries(t *testing.T, e *env) (folderID int64) {
	t.Helper()
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "作品A", "第01话"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "作品A", "第02话"), 0o755); err != nil {
		t.Fatal(err)
	}
	makeJPEG(t, src, "作品A/第01话/p1.jpg", 600, 400)
	makeFile(t, src, "作品A/第01话/p2.txt", 200)
	makeJPEG(t, src, "作品A/第02话/only.jpg", 800, 600)
	makeJPEG(t, src, "作品A/cover.jpg", 300, 400)
	n, err := e.m.UploadPaths(context.Background(), []string{filepath.Join(src, "作品A")}, 1)
	if err != nil || n != 3 {
		t.Fatalf("入队 %d 个(期望 3): %v", n, err)
	}
	allDone(t, waitIdle(t, e.m))
	folderID, err = e.db.ResolveFolderPath([]string{"作品A"})
	if err != nil {
		t.Fatal(err)
	}
	return folderID
}

// TestE2EMetaMoveSummary 验收主链:meta set → search 命中 → 摘要 →
// mv 纯索引移动(远端对象零变化)→ backup/pull 恢复后元数据仍在。
func TestE2EMetaMoveSummary(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	idA := putSeries(t, e)

	// 找出 cover.jpg 的 id(缩略图管线已给它生成真实缩略图)
	entries, err := e.db.ListFolder(idA)
	if err != nil {
		t.Fatal(err)
	}
	var coverID int64
	for _, en := range entries {
		if en.Name == "cover.jpg" {
			coverID = en.ID
		}
	}
	if coverID == 0 {
		t.Fatal("cover.jpg 未入库")
	}

	// meta set:tag + note + 自定义封面
	note := "作者:某人"
	if err := e.db.UpdateFolderMeta(idA, index.FolderMetaUpdate{
		Note: &note, Tags: []string{"科幻", "已完结"}, Cover: &coverID,
	}); err != nil {
		t.Fatal(err)
	}
	m, err := e.db.GetFolderMeta(idA)
	if err != nil || m.Note != note || m.CoverFileID != coverID ||
		!slices.Equal(m.Tags, []string{"已完结", "科幻"}) {
		t.Fatalf("元数据回读: %+v %v", m, err)
	}

	// search:目录名与 tag 都能命中作品(文件名一个都不含"作品A")
	for _, q := range []string{"作品A", "科幻", "某人"} {
		hits, err := e.db.SearchFolders(q, 10)
		if err != nil || len(hits) != 1 || hits[0].Path != "/作品A" {
			t.Fatalf("搜索 %q: %+v %v", q, hits, err)
		}
	}

	// 摘要:2 话 + 1 散文件,封面引用生效(单图满铺)
	s, err := e.db.FolderSummary(idA)
	if err != nil {
		t.Fatal(err)
	}
	if s.PackCount != 2 || s.FileCount != 3 || s.PendingCount != 0 {
		t.Fatalf("摘要: %+v", s)
	}
	if !slices.Equal(s.CoverFileIDs, []int64{coverID}) {
		t.Fatalf("封面应引用 cover.jpg: %v", s.CoverFileIDs)
	}

	// mv 前远端对象清单
	before := blobNames(t, e)

	// mv:把第01话移到新作品目录(目标目录不存在,逐级建)——与 CLI mv 同款组合
	var ch1 int64
	entries, _ = e.db.ListFolder(idA)
	for _, en := range entries {
		if en.Name == "第01话" {
			ch1 = en.ID
		}
	}
	if ch1 == 0 {
		t.Fatal("第01话 pack 未入库")
	}
	var idB int64
	if err := e.db.WithTx(func(tx *sql.Tx) error {
		var err error
		idB, err = e.db.EnsureFolderPath(tx, 1, []string{"作品B"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.db.MoveFiles([]int64{ch1}, idB); err != nil {
		t.Fatal(err)
	}

	// 远端对象零变化:纯索引移动,blob 一个不多不少不改名
	after := blobNames(t, e)
	if !slices.Equal(before, after) {
		t.Fatalf("mv 改动了远端对象:\nbefore=%v\nafter=%v", before, after)
	}
	// 聚合即时更新
	sA, _ := e.db.FolderSummary(idA)
	sB, err := e.db.FolderSummary(0) // 根含全部
	if err != nil {
		t.Fatal(err)
	}
	if sA.PackCount != 1 || sB.FileCount != 3 {
		t.Fatalf("移动后聚合: A=%+v root=%+v", sA, sB)
	}

	// backup → pull 恢复:元数据随索引云备份存活
	if _, err := backup.BackupNow(ctx, e.mk, e.db, e.store, false); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.PullRemote(ctx, e.mk, e.store, e.db, false); err != nil {
		t.Fatalf("恢复: %v", err)
	}
	m, err = e.db.GetFolderMeta(idA)
	if err != nil || m.Note != note || !slices.Equal(m.Tags, []string{"已完结", "科幻"}) {
		t.Fatalf("备份恢复后元数据丢失: %+v %v", m, err)
	}
	hits, err := e.db.SearchFolders("科幻", 10)
	if err != nil || len(hits) != 1 {
		t.Fatalf("恢复后 tag 检索: %+v %v", hits, err)
	}
}

// TestE2ECoverFallbackRealThumbs 真实缩略图管线下的一二级回退:
// rm 自定义封面文件后,回退到派生四宫格(pack 首页缩略图)。
func TestE2ECoverFallbackRealThumbs(t *testing.T) {
	e := newEnv(t)
	idA := putSeries(t, e)

	// 未设自定义封面:派生级取子条目缩略图(cover.jpg + 两话首页)
	s, err := e.db.FolderSummary(idA)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.CoverFileIDs) != 3 { // 3 个子条目,各带真实缩略图
		t.Fatalf("派生封面应含全部 3 个子条目: %+v", s)
	}
	for _, id := range s.CoverFileIDs {
		if id == 0 {
			t.Fatalf("真实管线下每格都该有缩略图: %+v", s)
		}
	}

	// 设自定义封面 → 单图;rm 该文件(软删)→ 引用悬空回退派生级
	cover := s.CoverFileIDs[0]
	if err := e.db.UpdateFolderMeta(idA, index.FolderMetaUpdate{Cover: &cover}); err != nil {
		t.Fatal(err)
	}
	s, _ = e.db.FolderSummary(idA)
	if !slices.Equal(s.CoverFileIDs, []int64{cover}) {
		t.Fatalf("自定义封面: %v", s.CoverFileIDs)
	}
	if err := e.db.SoftDeleteFiles([]int64{cover}); err != nil {
		t.Fatal(err)
	}
	s, err = e.db.FolderSummary(idA)
	if err != nil || len(s.CoverFileIDs) != 2 { // 少了 cover.jpg,剩两话
		t.Fatalf("悬空回退: %+v %v", s, err)
	}
}

func blobNames(t *testing.T, e *env) []string {
	t.Helper()
	names, err := e.store.ListBlobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	return names
}
