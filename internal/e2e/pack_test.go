package e2e

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"testing"

	"kist/internal/transfer"
)

// TestE2EFolderPack put 文件夹的默认打包语义(TODO-15):多话作品 →
// 每叶一对象、混杂层散文件独立入库、空目录保留;pack 条目带标记、
// get 还原成目录且逐文件一致、--keep-zip 落 zip;首页缩略图即封面。
func TestE2EFolderPack(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "作品A", "第01话"), 0o755); err != nil {
		t.Fatal(err)
	}
	makeJPEG(t, src, "作品A/第01话/p1.jpg", 600, 400)
	makeFile(t, src, "作品A/第01话/p2.txt", 200)
	makeFile(t, src, "作品A/第02话/only.bin", 2_500_000) // >1MiB:多块 + 10% 填充档
	makeFile(t, src, "作品A/说明.txt", 30)
	if err := os.MkdirAll(filepath.Join(src, "作品A", "空目录"), 0o755); err != nil {
		t.Fatal(err)
	}

	n, err := e.m.UploadPaths(ctx, []string{filepath.Join(src, "作品A")}, 1)
	if err != nil || n != 3 { // 2 个 pack + 1 个散文件
		t.Fatalf("入队 %d 个(期望 3): %v", n, err)
	}
	allDone(t, waitIdle(t, e.m))

	// 远端:恰 3 个 blob(一话一对象,散文件一对象)
	blobs, err := e.store.ListBlobs(ctx)
	if err != nil || len(blobs) != 3 {
		t.Fatalf("远端 blob 数 = %d,期望 3: %v", len(blobs), err)
	}

	// 索引:/作品A/ 下:2 个 P 条目 + 1 个 F 条目 + 空目录
	folderID, err := e.db.ResolveFolderPath([]string{"作品A"})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := e.db.ListFolder(folderID)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, en := range entries {
		switch {
		case en.IsFolder:
			kinds[en.Name] = "D"
		case en.Pack:
			kinds[en.Name] = "P"
		default:
			kinds[en.Name] = "F"
		}
	}
	want := map[string]string{"第01话": "P", "第02话": "P", "说明.txt": "F", "空目录": "D"}
	if len(kinds) != len(want) {
		t.Fatalf("条目 = %v,期望 %v", kinds, want)
	}
	for name, k := range want {
		if kinds[name] != k {
			t.Fatalf("%s 应为 %s,实际 %v", name, k, kinds)
		}
	}

	// 第01话 pack 行:标记、封面缩略图、user_meta 记原目录规模
	var packID, id2 int64
	for _, en := range entries {
		switch en.Name {
		case "第01话":
			packID = en.ID
		case "第02话":
			id2 = en.ID
		}
	}
	f, err := e.db.GetFile(packID)
	if err != nil || !f.Pack || !f.UserMeta.Valid {
		t.Fatalf("pack 行字段: %+v %v", f, err)
	}
	if cov, err := e.db.GetReadyCover(packID); err != nil || cov.Size <= 0 {
		t.Fatalf("pack 应有首页封面引用: %+v %v", cov, err)
	}

	// get 还原成目录:逐文件 SHA 一致
	outDir := t.TempDir()
	if _, err := e.m.DownloadTo(ctx, []int64{packID}, outDir); err != nil {
		t.Fatal(err)
	}
	allDone(t, waitIdle(t, e.m))
	if fileSHA(t, filepath.Join(outDir, "第01话", "p1.jpg")) != fileSHA(t, filepath.Join(src, "作品A", "第01话", "p1.jpg")) {
		t.Fatal("p1.jpg 还原后 SHA 不一致")
	}
	if fileSHA(t, filepath.Join(outDir, "第01话", "p2.txt")) != fileSHA(t, filepath.Join(src, "作品A", "第01话", "p2.txt")) {
		t.Fatal("p2.txt 还原后 SHA 不一致")
	}

	// get --keep-zip:落 <名>.zip,条目完整
	outDir2 := t.TempDir()
	if _, err := e.m.DownloadTo(ctx, []int64{id2}, outDir2, transfer.DownloadOptions{KeepZip: true}); err != nil {
		t.Fatal(err)
	}
	allDone(t, waitIdle(t, e.m))
	zr, err := zip.OpenReader(filepath.Join(outDir2, "第02话.zip"))
	if err != nil {
		t.Fatalf("keep-zip 产物: %v", err)
	}
	defer zr.Close()
	if len(zr.File) == 0 || zr.File[0].Name != "only.bin" {
		t.Fatalf("zip 条目: %+v", zr.File)
	}
}

// TestE2EFolderPackSingleChapter 单话形态:put 根即叶子 → 整根一个
// pack 直挂 dest,条目名 = 根目录名。
func TestE2EFolderPackSingleChapter(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	src := t.TempDir()
	makeFile(t, src, "单话作品/全一话.txt", 100)

	if _, err := e.m.UploadPaths(ctx, []string{filepath.Join(src, "单话作品")}, 1); err != nil {
		t.Fatal(err)
	}
	allDone(t, waitIdle(t, e.m))

	entries, err := e.db.ListFolder(1)
	if err != nil || len(entries) != 1 {
		t.Fatalf("根条目 = %+v(%v),期望恰一个 pack", entries, err)
	}
	if entries[0].Name != "单话作品" || !entries[0].Pack {
		t.Fatalf("应为直挂根的 pack 条目: %+v", entries[0])
	}
	outDir := t.TempDir()
	if _, err := e.m.DownloadTo(ctx, []int64{entries[0].ID}, outDir); err != nil {
		t.Fatal(err)
	}
	allDone(t, waitIdle(t, e.m))
	if fileSHA(t, filepath.Join(outDir, "单话作品", "全一话.txt")) != fileSHA(t, filepath.Join(src, "单话作品", "全一话.txt")) {
		t.Fatal("单话还原后 SHA 不一致")
	}
}
