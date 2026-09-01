package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kist/internal/audit"
	"kist/internal/config"
	"kist/internal/errs"

	"golang.org/x/net/webdav"
)

func TestActionOf(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{nil, "(usage)"},
		{[]string{"ls", "/a"}, "ls"},
		{[]string{"put", "/x", "--dest", "/d"}, "put"},
		{[]string{"outbox", "push", "--all"}, "outbox push"}, // 组命令带子动作,push/discard 可分
		{[]string{"outbox"}, "outbox"},
		{[]string{"meta", "set", "/x"}, "meta set"},
		{[]string{"covers", "migrate"}, "covers migrate"},
	}
	for _, c := range cases {
		if got := actionOf(c.args); got != c.want {
			t.Errorf("actionOf(%q) = %q, want %q", c.args, got, c.want)
		}
	}
}

// invoke 复刻 main 的收口序列:run + 恰好一条审计。main 之外唯一入口,
// 让端到端测试走真路径而非模拟。
func invoke(t *testing.T, args ...string) error {
	t.Helper()
	start := time.Now()
	err := run(args)
	audit.Log(actionOf(args), start, err)
	return err
}

// auditLines 读 KIST_HOME/audit.log 并逐行解析。
func auditLines(t *testing.T) []audit.Entry {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(config.HomeDir(), "audit.log"))
	if err != nil {
		t.Fatalf("读 audit.log: %v", err)
	}
	var out []audit.Entry
	for i, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		var e audit.Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("第 %d 行非法 JSON: %q: %v", i+1, line, err)
		}
		out = append(out, e)
	}
	return out
}

// TestAuditEndToEnd 验收(TODO-14):本地 WebDAV + 真实 init/put/unlock 全跑,
// 每命令恰好一行;成功 put 带 files/bytes;unlock 错口令记 AUTH_FAILED
// 且口令本体不落盘。
func TestAuditEndToEnd(t *testing.T) {
	srv := httptest.NewServer(&webdav.Handler{
		FileSystem: webdav.Dir(t.TempDir()),
		LockSystem: webdav.NewMemLS(),
	})
	defer srv.Close()

	t.Setenv("KIST_HOME", t.TempDir())
	t.Setenv("KIST_TMPDIR", filepath.Join(t.TempDir(), "tmp"))
	const pass = "审计-e2e 口令!"
	t.Setenv("KIST_PASS", pass) // readPass 优先环境变量,init/put 经此取口令
	if err := os.MkdirAll(config.HomeDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.StoredConfig{}
	cfg.Drives = append(cfg.Drives, config.Drive{URL: srv.URL, Username: "u", Password: "p"})
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}

	// 建账户 → 上传(文件名带中文与空格)→ 错口令试错
	if err := invoke(t, "init", "--pass-stdin"); err != nil {
		t.Fatalf("init: %v", err)
	}
	src := filepath.Join(t.TempDir(), "审计 文件.txt")
	if err := os.WriteFile(src, []byte("审计测试内容"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := invoke(t, "put", src, "--dest", "/审计 目录", "--pass-stdin"); err != nil {
		t.Fatalf("put: %v", err)
	}
	t.Setenv("KIST_PASS", "错口令")
	if err := invoke(t, "unlock", "--pass-stdin"); err == nil {
		t.Fatal("unlock 用错口令应当失败")
	}

	entries := auditLines(t)
	if len(entries) != 3 {
		t.Fatalf("三命令应恰好三行: %d", len(entries))
	}
	if entries[0].Cmd != "init" || !entries[0].OK {
		t.Fatalf("init 条目不符: %+v", entries[0])
	}
	put := entries[1]
	if put.Cmd != "put" || !put.OK || put.Extra["files"] != float64(1) || put.Extra["bytes"].(float64) <= 0 {
		t.Fatalf("put 条目应成功且带 files/bytes: %+v", put)
	}
	if put.Extra["dest"] != "/审计 目录" {
		t.Fatalf("put 条目应带 dest: %+v", put.Extra)
	}
	unlock := entries[2]
	if unlock.OK || unlock.Code != errs.AuthFailed {
		t.Fatalf("unlock 条目应记 AUTH_FAILED: %+v", unlock)
	}

	// 隐私红线:口令本体与远端地址凭据形态全文检索不得命中
	b, err := os.ReadFile(filepath.Join(config.HomeDir(), "audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); strings.Contains(s, pass) || strings.Contains(s, "KIST_PASS") {
		t.Fatalf("口令相关内容不得落盘:\n%s", s)
	}
}
