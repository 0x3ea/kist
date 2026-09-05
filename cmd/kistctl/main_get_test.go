package main

// get 文件夹下载的 CLI 端到端:真 init/put/get 走 run() 全路径。
// 夹具与 TestAuditEndToEnd 同款(本地 WebDAV + KIST_PASS 取口令)。

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"kist/internal/config"
	"kist/internal/index"

	"golang.org/x/net/webdav"
)

// newCLIEnv 起本地 WebDAV 并 init 账户,返回后即可 invoke("put"/"get")。
func newCLIEnv(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(&webdav.Handler{
		FileSystem: webdav.Dir(t.TempDir()),
		LockSystem: webdav.NewMemLS(),
	})
	t.Cleanup(srv.Close)

	t.Setenv("KIST_HOME", t.TempDir())
	t.Setenv("KIST_TMPDIR", filepath.Join(t.TempDir(), "tmp"))
	t.Setenv("KIST_PASS", "测试口令-文件夹下载")
	if err := os.MkdirAll(config.HomeDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.StoredConfig{}
	cfg.Drives = append(cfg.Drives, config.Drive{URL: srv.URL, Username: "u", Password: "p"})
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if err := invoke(t, "init", "--pass-stdin"); err != nil {
		t.Fatalf("init: %v", err)
	}
}

// TestGetFolderEndToEnd put 多话源树 → get /作品A --to out:结构与逐文件
// 字节一致、空目录落盘;get / 与已删条目被拒绝。
func TestGetFolderEndToEnd(t *testing.T) {
	newCLIEnv(t)

	src := t.TempDir()
	for _, f := range []struct {
		rel  string
		size int
	}{
		{"作品A/第01话/p1.txt", 300},
		{"作品A/第01话/p2.txt", 200},
		{"作品A/extras/深层/deep.bin", 1000},
		{"作品A/说明.txt", 30},
	} {
		p := filepath.Join(src, filepath.FromSlash(f.rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, makeBytes(f.size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(src, "作品A", "空目录"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := invoke(t, "put", filepath.Join(src, "作品A"), "--pass-stdin"); err != nil {
		t.Fatalf("put: %v", err)
	}

	out := filepath.Join(t.TempDir(), "out")
	if err := invoke(t, "get", "/作品A", "--to", out, "--pass-stdin"); err != nil {
		t.Fatalf("get /作品A: %v", err)
	}
	rootLocal := filepath.Join(out, "作品A")
	for _, f := range []struct {
		rel  string
		size int
	}{
		{"第01话/p1.txt", 300},
		{"第01话/p2.txt", 200},
		{"extras/深层/deep.bin", 1000},
		{"说明.txt", 30},
	} {
		got, err := os.ReadFile(filepath.Join(rootLocal, filepath.FromSlash(f.rel)))
		if err != nil {
			t.Fatalf("还原文件缺失 %s: %v", f.rel, err)
		}
		if !bytes.Equal(got, makeBytes(f.size)) {
			t.Fatalf("%s 内容不一致(%d 字节)", f.rel, len(got))
		}
	}
	if fi, err := os.Stat(filepath.Join(rootLocal, "空目录")); err != nil || !fi.IsDir() {
		t.Fatalf("空目录未还原: %v", err)
	}

	// get /:整树导出未做,显式拒绝
	if err := invoke(t, "get", "/", "--to", out, "--pass-stdin"); err == nil ||
		!strings.Contains(err.Error(), "整树") {
		t.Fatalf("get / 应拒绝并提示整树导出未做, got %v", err)
	}

	// 已软删的条目拒绝下载(经 resolveEntryTarget 的统一守卫)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	db, err := index.Open(config.DriveIndexPath(cfg.ActiveDrive().ID))
	if err != nil {
		t.Fatal(err)
	}
	hits, err := db.Search("说明.txt", 5) // 散文件才单独成行;pack 内的页不在索引
	if err != nil || len(hits) != 1 {
		t.Fatalf("搜索 说明.txt: %+v %v", hits, err)
	}
	db.Close()
	if err := invoke(t, "rm", strconv.FormatInt(hits[0].ID, 10)); err != nil {
		t.Fatalf("rm: %v", err)
	}
	if err := invoke(t, "get", strconv.FormatInt(hits[0].ID, 10), "--to", out, "--pass-stdin"); err == nil ||
		!strings.Contains(err.Error(), "已删除") {
		t.Fatalf("已删条目应拒绝下载, got %v", err)
	}
}

// makeBytes 造确定性内容(长度即可校验;内容与源同函数生成,逐字节可比)。
func makeBytes(size int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i*131 + 7)
	}
	return b
}
