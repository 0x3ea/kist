package transfer

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func mustMkdirT(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeT(t *testing.T, path string, content []byte) {
	t.Helper()
	mustMkdirT(t, filepath.Dir(path))
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func names(segs [][]string) []string {
	out := make([]string, len(segs))
	for i, s := range segs {
		out[i] = strings.Join(s, "/")
	}
	return out
}

func packSegs(ps []packRoot) [][]string {
	out := make([][]string, len(ps))
	for i, p := range ps {
		out[i] = p.RelSegs
	}
	return out
}

// TestPlanPackTreeLeafRoot 单话形态:put 根自身无子目录 → 整根一个 pack。
func TestPlanPackTreeLeafRoot(t *testing.T) {
	root := t.TempDir()
	writeT(t, filepath.Join(root, "001.jpg"), []byte("a"))
	writeT(t, filepath.Join(root, "002.jpg"), []byte("b"))
	plan, err := planPackTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.RootIsPack || plan.RootPack == nil {
		t.Fatalf("整根应成一个 pack: %+v", plan)
	}
	if plan.RootPack.Name != filepath.Base(root) || plan.RootPack.Files != 2 ||
		plan.RootPack.FirstImage != filepath.Join(root, "001.jpg") {
		t.Fatalf("根 pack 字段: %+v", plan.RootPack)
	}
	if len(plan.Packs) != 0 || len(plan.Loose) != 0 || len(plan.VirtualDirs) != 0 {
		t.Fatalf("不应有其他条目: %+v", plan)
	}
}

// TestPlanPackTreeMultiChapter 多话形态:叶子子目录各自成 pack,
// 根下散文件独立入库(漫画一话是原子消费单元的对应结构)。
func TestPlanPackTreeMultiChapter(t *testing.T) {
	root := t.TempDir()
	writeT(t, filepath.Join(root, "第01话", "p1.jpg"), []byte("a"))
	writeT(t, filepath.Join(root, "第01话", "p2.png"), []byte("b"))
	writeT(t, filepath.Join(root, "第02话", "p3.txt"), []byte("c"))
	writeT(t, filepath.Join(root, "封面说明.txt"), []byte("d"))
	plan, err := planPackTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if plan.RootIsPack {
		t.Fatal("含子目录的根不应整体成包(整部一包是被否决的粒度)")
	}
	if len(plan.Packs) != 2 {
		t.Fatalf("packs = %v", names(packSegs(plan.Packs)))
	}
	if plan.Packs[0].Name != "第01话" || plan.Packs[0].Files != 2 ||
		plan.Packs[0].FirstImage != filepath.Join(root, "第01话", "p1.jpg") {
		t.Fatalf("第01话 pack: %+v", plan.Packs[0])
	}
	if len(plan.Loose) != 1 || plan.Loose[0].Name != "封面说明.txt" {
		t.Fatalf("散文件: %+v", plan.Loose)
	}
	if len(plan.VirtualDirs) != 0 {
		t.Fatalf("两话均为叶子,不应有中间虚拟目录: %v", names(plan.VirtualDirs))
	}
}

// TestPlanPackTreeNested 卷层嵌套:逐层下降,卷保留为虚拟目录。
func TestPlanPackTreeNested(t *testing.T) {
	root := t.TempDir()
	writeT(t, filepath.Join(root, "卷一", "第01话", "p.jpg"), []byte("a"))
	writeT(t, filepath.Join(root, "卷二", "第02话", "q.jpg"), []byte("b"))
	writeT(t, filepath.Join(root, "readme.txt"), []byte("c"))
	plan, err := planPackTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Packs) != 2 ||
		names(packSegs(plan.Packs))[0] != "卷一/第01话" || names(packSegs(plan.Packs))[1] != "卷二/第02话" {
		t.Fatalf("嵌套叶子: %v", names(packSegs(plan.Packs)))
	}
	if len(plan.VirtualDirs) != 2 ||
		names(plan.VirtualDirs)[0] != "卷一" || names(plan.VirtualDirs)[1] != "卷二" {
		t.Fatalf("卷层应保留为虚拟目录: %v", names(plan.VirtualDirs))
	}
	if len(plan.Loose) != 1 || plan.Loose[0].Name != "readme.txt" {
		t.Fatalf("根下散文件: %+v", plan.Loose)
	}
}

// TestPlanPackTreeMixedAndEmpty 混杂层散文件独立入库;空目录保留为虚拟目录。
func TestPlanPackTreeMixedAndEmpty(t *testing.T) {
	root := t.TempDir()
	writeT(t, filepath.Join(root, "a", "f.txt"), []byte("a"))
	writeT(t, filepath.Join(root, "mixed", "s", "g.txt"), []byte("b"))
	writeT(t, filepath.Join(root, "mixed", "h.txt"), []byte("c"))
	mustMkdirT(t, filepath.Join(root, "空目录"))
	plan, err := planPackTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Packs) != 2 {
		t.Fatalf("packs = %v", names(packSegs(plan.Packs)))
	}
	if len(plan.VirtualDirs) != 2 ||
		names(plan.VirtualDirs)[0] != "mixed" || names(plan.VirtualDirs)[1] != "空目录" {
		t.Fatalf("虚拟目录: %v", names(plan.VirtualDirs))
	}
	if len(plan.Loose) != 1 || plan.Loose[0].Name != "h.txt" {
		t.Fatalf("混杂层散文件: %+v", plan.Loose)
	}
}

// TestPlanPackTreeRejects 校验遍一次收全问题(非 UTF-8 名/symlink/FIFO),
// 整次 put 拒绝——不留半套索引或远端对象。
// 注:syscall.Mkfifo 与非 UTF-8 文件名均为 Linux 专属,其他平台跳过。
func TestPlanPackTreeRejects(t *testing.T) {
	root := t.TempDir()
	writeT(t, filepath.Join(root, "正常.txt"), []byte("x"))
	badName := filepath.Join(root, string([]byte{0xff, 0xfe})+".txt") // 非 UTF-8 名
	if err := os.WriteFile(badName, []byte("y"), 0o644); err != nil {
		t.Skipf("本平台无法创建非 UTF-8 名: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "正常.txt"), filepath.Join(root, "链.txt")); err != nil {
		t.Skipf("本平台无法建 symlink: %v", err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "管.fifo"), 0o644); err != nil {
		t.Skipf("本平台无法建 FIFO: %v", err)
	}
	_, err := planPackTree(root)
	if err == nil {
		t.Fatal("包含非 UTF-8 名/symlink/FIFO 应整次拒绝")
	}
	msg := err.Error()
	for _, want := range []string{"UTF-8", "链.txt", "管.fifo"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误应一次性列全(缺 %q):\n%s", want, msg)
		}
	}
}

// TestPackZipRoundtrip zip 打包→解压往返:字节一致、空文件/空目录保留、
// unicode 路径、条目 mtime 还原(zip 扩展时间戳,容差 2s)。
func TestPackZipRoundtrip(t *testing.T) {
	root := t.TempDir()
	writeT(t, filepath.Join(root, "empty.bin"), nil)
	writeT(t, filepath.Join(root, "子 目录", "深", "说明.txt"), []byte("内容内容"))
	mustMkdirT(t, filepath.Join(root, "子 目录", "空目录"))
	mtime := time.Unix(1700000000, 0).UTC()
	for _, p := range []string{
		filepath.Join(root, "empty.bin"),
		filepath.Join(root, "子 目录", "深", "说明.txt"),
	} {
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}

	var buf bytes.Buffer
	entries, raw, _, err := writePackZip(context.Background(), &buf, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if entries != 2 || raw != int64(len("内容内容")) {
		t.Fatalf("entries=%d raw=%d", entries, raw)
	}

	zipPath := filepath.Join(t.TempDir(), "p.zip")
	if err := os.WriteFile(zipPath, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := extractZipTree(context.Background(), zipPath, out); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(out, "子 目录", "深", "说明.txt"))
	if err != nil || string(got) != "内容内容" {
		t.Fatalf("说明.txt 往返: %q %v", got, err)
	}
	if fi, err := os.Stat(filepath.Join(out, "empty.bin")); err != nil || fi.Size() != 0 {
		t.Fatalf("empty.bin: %v %+v", err, fi)
	}
	if _, err := os.Stat(filepath.Join(out, "子 目录", "空目录")); err != nil {
		t.Fatalf("空目录未保留: %v", err)
	}
	fi, _ := os.Stat(filepath.Join(out, "empty.bin"))
	if d := fi.ModTime().Sub(mtime); d > 2*time.Second || d < -2*time.Second {
		t.Fatalf("mtime 偏差 %v", d)
	}
}

// TestPackZipCancel 打包中途取消:立即失败,不留半套产物(临时文件由调用方清理)。
func TestPackZipCancel(t *testing.T) {
	root := t.TempDir()
	writeT(t, filepath.Join(root, "f.bin"), bytes.Repeat([]byte{1}, 1<<20))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var buf bytes.Buffer
	if _, _, _, err := writePackZip(ctx, &buf, root, nil); err == nil {
		t.Fatal("已取消的 ctx 应使打包失败")
	}
}

// TestExtractZipTreeZipSlip 伪造含 ../ 逃逸条目的 zip:解压必须报错,
// 逃逸文件不得落盘(自家 zip 不应触发,纵深防御;将来 put 外部 CBZ 时为必修)。
func TestExtractZipTreeZipSlip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w1, _ := zw.CreateHeader(&zip.FileHeader{Name: "ok.txt", Method: zip.Store})
	w1.Write([]byte("ok"))
	w2, _ := zw.CreateHeader(&zip.FileHeader{Name: "../evil.txt", Method: zip.Store})
	w2.Write([]byte("evil"))
	zw.Close()

	zipPath := filepath.Join(t.TempDir(), "evil.zip")
	if err := os.WriteFile(zipPath, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := extractZipTree(context.Background(), zipPath, target); err == nil {
		t.Fatal("逃逸条目应报错")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(target), "evil.txt")); !os.IsNotExist(err) {
		t.Fatal("逃逸文件不得落盘")
	}
}

// TestExtractZipTreeRejectsNonUTF8Name 非 UTF-8 条目名:源头拒绝
// (跨系统解压按本地代码页猜必乱码)。
func TestExtractZipTreeRejectsNonUTF8Name(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.CreateHeader(&zip.FileHeader{Name: string([]byte{0xff, 0xfe}) + ".txt", Method: zip.Store})
	w.Write([]byte("x"))
	zw.Close()
	zipPath := filepath.Join(t.TempDir(), "bad.zip")
	if err := os.WriteFile(zipPath, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := extractZipTree(context.Background(), zipPath, t.TempDir()); err == nil {
		t.Fatal("非 UTF-8 条目名应报错")
	}
}
