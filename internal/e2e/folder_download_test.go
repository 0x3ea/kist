package e2e

// 文件夹下载(按虚拟结构还原子树)的行为主担保:上传侧用 pack 语义造出
// "pack 条目 + 散文件 + 虚拟目录"的混合子树,下载侧断言结构重建、逐文件
// 一致、空目录落盘、根名消解与 uploading 跳过。

import (
	"archive/zip"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"kist/internal/transfer"
)

// assertTreeMatch 递归比对源树与下载树:文件逐个 SHA、目录逐个存在
// (空目录也是结构的一部分)。
func assertTreeMatch(t *testing.T, srcRoot, dstRoot string) {
	t.Helper()
	files := 0
	err := filepath.WalkDir(srcRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcRoot, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		dst := filepath.Join(dstRoot, rel)
		if d.IsDir() {
			if fi, statErr := os.Stat(dst); statErr != nil || !fi.IsDir() {
				t.Errorf("目录缺失: %s", rel)
			}
			return nil
		}
		files++
		if fileSHA(t, dst) != fileSHA(t, p) {
			t.Errorf("SHA 不一致: %s", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatal("源树没有文件,比对无意义")
	}
}

// downloadNames 取全部 download 任务的名字集合(快照含上传历史,要过滤)。
func downloadNames(trs []transfer.Transfer) map[string]bool {
	out := map[string]bool{}
	for _, tr := range trs {
		if tr.Kind == "download" {
			out[tr.Name] = true
		}
	}
	return out
}

// TestE2EFolderDownload 主路径:多层嵌套 + pack + 散文件 + 空目录 →
// 整棵子树按虚拟结构还原,逐文件一致。
func TestE2EFolderDownload(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "作品A", "第01话"), 0o755); err != nil {
		t.Fatal(err)
	}
	makeJPEG(t, src, "作品A/第01话/p1.jpg", 600, 400)
	makeFile(t, src, "作品A/第01话/p2.txt", 200)
	makeFile(t, src, "作品A/第02话/only.bin", 2_500_000) // 多块
	makeFile(t, src, "作品A/extras/深层/deep.bin", 1000)
	makeFile(t, src, "作品A/说明.txt", 30)
	if err := os.MkdirAll(filepath.Join(src, "作品A", "空目录"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := e.m.UploadPaths(ctx, []string{filepath.Join(src, "作品A")}, 1); err != nil {
		t.Fatal(err)
	}
	allDone(t, waitIdle(t, e.m))

	root, err := e.db.ResolveFolderPath([]string{"作品A"})
	if err != nil {
		t.Fatal(err)
	}
	outDir := t.TempDir()
	queued, skipped, err := e.m.DownloadEntriesTo(ctx, nil, []int64{root}, outDir)
	if err != nil || queued != 4 || skipped != 0 { // 3 pack + 1 散文件
		t.Fatalf("入队=%d skipped=%d err=%v,期望 4/0", queued, skipped, err)
	}
	trs := waitIdle(t, e.m)
	allDone(t, trs)

	// 显示名 = 子树内相对路径(几百个同 basename 才分得清);pack 条目
	// 整体是一个传输,显示到条目名为止
	names := downloadNames(trs)
	for _, want := range []string{
		"作品A/第01话", "作品A/第02话", "作品A/extras/深层", "作品A/说明.txt",
	} {
		if !names[want] {
			t.Fatalf("传输名缺 %q: %v", want, names)
		}
	}
	assertTreeMatch(t, filepath.Join(src, "作品A"), filepath.Join(outDir, "作品A"))
	// 空目录也落盘(没有任务替它建,必须规划期 mkdir)
	if fi, err := os.Stat(filepath.Join(outDir, "作品A", "空目录")); err != nil || !fi.IsDir() {
		t.Fatalf("空目录未还原: %v", err)
	}
}

// TestE2EFolderDownloadRootClash 落盘目录已有同名根 → 包一层 " (1)",
// 内容完整且不动既有目录。
func TestE2EFolderDownloadRootClash(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	src := t.TempDir()
	makeFile(t, src, "作品A/第01话/p1.txt", 100)
	makeFile(t, src, "作品A/说明.txt", 30)
	if _, err := e.m.UploadPaths(ctx, []string{filepath.Join(src, "作品A")}, 1); err != nil {
		t.Fatal(err)
	}
	allDone(t, waitIdle(t, e.m))

	root, _ := e.db.ResolveFolderPath([]string{"作品A"})
	outDir := t.TempDir()
	junk := filepath.Join(outDir, "作品A")
	if err := os.MkdirAll(junk, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(junk, "junk.txt"), []byte("别动我"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, _, err := e.m.DownloadEntriesTo(ctx, nil, []int64{root}, outDir); err != nil {
		t.Fatal(err)
	}
	allDone(t, waitIdle(t, e.m))

	if _, err := os.Stat(filepath.Join(junk, "junk.txt")); err != nil {
		t.Fatalf("既有目录被扰动: %v", err)
	}
	assertTreeMatch(t, filepath.Join(src, "作品A"), filepath.Join(outDir, "作品A (1)"))
}

// TestE2EFolderDownloadKeepZip keep-zip 对子树内 pack 生效:落 <名>.zip
// 而非还原目录,散文件不受影响。
func TestE2EFolderDownloadKeepZip(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	src := t.TempDir()
	makeFile(t, src, "作品A/第01话/p1.jpg", 400)
	makeFile(t, src, "作品A/说明.txt", 30)
	if _, err := e.m.UploadPaths(ctx, []string{filepath.Join(src, "作品A")}, 1); err != nil {
		t.Fatal(err)
	}
	allDone(t, waitIdle(t, e.m))

	root, _ := e.db.ResolveFolderPath([]string{"作品A"})
	outDir := t.TempDir()
	if _, _, err := e.m.DownloadEntriesTo(ctx, nil, []int64{root}, outDir,
		transfer.DownloadOptions{KeepZip: true}); err != nil {
		t.Fatal(err)
	}
	allDone(t, waitIdle(t, e.m))

	rootLocal := filepath.Join(outDir, "作品A")
	if _, err := os.Stat(filepath.Join(rootLocal, "第01话")); !os.IsNotExist(err) {
		t.Fatalf("keep-zip 下不应还原目录: %v", err)
	}
	zr, err := zip.OpenReader(filepath.Join(rootLocal, "第01话.zip"))
	if err != nil {
		t.Fatalf("keep-zip 产物: %v", err)
	}
	defer zr.Close()
	if len(zr.File) == 0 || zr.File[0].Name != "p1.jpg" {
		t.Fatalf("zip 条目异常: %+v", zr.File)
	}
	if fileSHA(t, filepath.Join(rootLocal, "说明.txt")) != fileSHA(t, filepath.Join(src, "作品A", "说明.txt")) {
		t.Fatal("散文件应不受 keep-zip 影响")
	}
}

// allDownloaded 断言全部 download 任务完成(defer 等历史留在快照里,终态
// 不是 done,不能混进 allDone)。
func allDownloaded(t *testing.T, trs []transfer.Transfer) {
	t.Helper()
	for _, tr := range trs {
		if tr.Kind != "download" {
			continue
		}
		if tr.Phase != transfer.PhaseDone {
			t.Fatalf("%s 处于 %s:%s", tr.Name, tr.Phase, tr.Err)
		}
	}
}

// TestE2EFolderDownloadSkipsUploading 子树内 uploading 条目跳过并计数,
// 其所在目录照建(否则整目录从落盘结果里消失)。
func TestE2EFolderDownloadSkipsUploading(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	src := t.TempDir()
	makeFile(t, src, "作品A/第01话/p1.txt", 100)
	if _, err := e.m.UploadPaths(ctx, []string{filepath.Join(src, "作品A")}, 1); err != nil {
		t.Fatal(err)
	}
	allDone(t, waitIdle(t, e.m))

	// 向虚拟子目录 defer 一条:记账 uploading、产物在出站箱、不发 PUT
	later := makeFile(t, t.TempDir(), "later.bin", 500)
	pending := mustFolder(t, e.db, "作品A", "待传")
	if _, err := e.m.DeferPaths(ctx, []string{later}, pending); err != nil {
		t.Fatal(err)
	}
	// defer 的终态永远是 deferred,不能拿 allDone(只认 done)来等
	if tr := lastTr(t, waitIdle(t, e.m)); tr.Phase != transfer.PhaseDeferred {
		t.Fatalf("defer 终态 = %s(%s),期望 deferred", tr.Phase, tr.Err)
	}

	root, _ := e.db.ResolveFolderPath([]string{"作品A"})
	outDir := t.TempDir()
	queued, skipped, err := e.m.DownloadEntriesTo(ctx, nil, []int64{root}, outDir)
	if err != nil || queued != 1 || skipped != 1 {
		t.Fatalf("入队=%d skipped=%d err=%v,期望 1/1", queued, skipped, err)
	}
	allDownloaded(t, waitIdle(t, e.m))

	if _, err := os.Stat(filepath.Join(outDir, "作品A", "待传", "later.bin")); !os.IsNotExist(err) {
		t.Fatalf("uploading 条目不应落盘: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(outDir, "作品A", "待传")); err != nil || !fi.IsDir() {
		t.Fatalf("只剩 uploading 的目录仍应建出来: %v", err)
	}
	assertTreeMatch(t, filepath.Join(src, "作品A"), filepath.Join(outDir, "作品A"))
}

// TestE2EFolderDownloadMixed 文件与文件夹同批:文件平铺、文件夹包一层。
func TestE2EFolderDownloadMixed(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	src := t.TempDir()
	makeFile(t, src, "作品B/only.txt", 100)
	// Expand:单文件目录默认会打成单个 pack(没有虚拟目录),这里要的是目录形态
	if _, err := e.m.UploadPaths(ctx, []string{filepath.Join(src, "作品B")}, 1,
		transfer.UploadOptions{Expand: true}); err != nil {
		t.Fatal(err)
	}
	allDone(t, waitIdle(t, e.m))
	loose := makeFile(t, t.TempDir(), "散件.txt", 50)
	if _, err := e.m.UploadPaths(ctx, []string{loose}, 1); err != nil {
		t.Fatal(err)
	}
	allDone(t, waitIdle(t, e.m))
	hs, err := e.db.Search("散件.txt", 5)
	if err != nil || len(hs) != 1 {
		t.Fatalf("搜索 散件.txt: %+v %v", hs, err)
	}

	root, _ := e.db.ResolveFolderPath([]string{"作品B"})
	outDir := t.TempDir()
	queued, skipped, err := e.m.DownloadEntriesTo(ctx, []int64{hs[0].ID}, []int64{root}, outDir)
	if err != nil || queued != 2 || skipped != 0 {
		t.Fatalf("入队=%d skipped=%d err=%v,期望 2/0", queued, skipped, err)
	}
	allDone(t, waitIdle(t, e.m))

	if fileSHA(t, filepath.Join(outDir, "散件.txt")) != fileSHA(t, loose) {
		t.Fatal("平铺文件 SHA 不一致")
	}
	assertTreeMatch(t, filepath.Join(src, "作品B"), filepath.Join(outDir, "作品B"))
}

// TestE2EFolderDownloadPackSiblingClash 同一虚拟目录下 pack 条目与虚拟目录
// 同名(索引允许文件与目录同名,两套消解互不干扰):两者都要落地且互不
// 覆盖——目录必须先于 pack 任务存在,pack 的运行时 uniqueLocalName 消解
// 才"看得见"它,自动落 " (1)" 而不是 rename 撞车。
func TestE2EFolderDownloadPackSiblingClash(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	folderC := mustFolder(t, e.db, "作品C")
	mustFolder(t, e.db, "作品C", "第01话") // 纯虚拟空目录

	// 同名单话 pack:put 本地目录 第01话 → pack 条目也叫 第01话
	src := t.TempDir()
	makeFile(t, src, "第01话/page.bin", 100)
	if _, err := e.m.UploadPaths(ctx, []string{filepath.Join(src, "第01话")}, folderC); err != nil {
		t.Fatal(err)
	}
	allDone(t, waitIdle(t, e.m))

	ents, err := e.db.ListFolder(folderC)
	if err != nil {
		t.Fatal(err)
	}
	var packs, dirs int
	for _, en := range ents {
		if en.Name != "第01话" {
			t.Fatalf("意外条目 %q", en.Name)
		}
		if en.IsFolder {
			dirs++
		} else if en.Pack {
			packs++
		}
	}
	if packs != 1 || dirs != 1 {
		t.Fatalf("应为一个同名 pack + 一个同名虚拟目录,得到 %d pack / %d dir", packs, dirs)
	}

	outDir := t.TempDir()
	if _, _, err := e.m.DownloadEntriesTo(ctx, nil, []int64{folderC}, outDir); err != nil {
		t.Fatal(err)
	}
	allDone(t, waitIdle(t, e.m))

	rootLocal := filepath.Join(outDir, "作品C")
	if fi, err := os.Stat(filepath.Join(rootLocal, "第01话")); err != nil || !fi.IsDir() {
		t.Fatalf("同名虚拟目录应原样落地: %v", err)
	}
	if fileSHA(t, filepath.Join(rootLocal, "第01话 (1)", "page.bin")) != fileSHA(t, filepath.Join(src, "第01话", "page.bin")) {
		t.Fatal("同名 pack 应消解到 (1) 且内容一致")
	}
}
