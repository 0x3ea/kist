package dav

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// TestRetryLogsWarn 重试必须记 Warn 且不泄露凭据(TODO-07 静默黑洞 + 隐私红线)。
func TestRetryLogsWarn(t *testing.T) {
	srv, fi := newTestServer(t)
	c := newFastClient(t, srv)
	ctx := context.Background()
	if err := c.EnsureRoot(ctx); err != nil {
		t.Fatal(err)
	}
	fi.mu.Lock()
	fi.failsRemaining = 1 // 一次 503 后成功
	fi.mu.Unlock()

	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)

	f, err := os.CreateTemp("", "kist-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.Close()
	fo, err := os.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer fo.Close()
	if err := c.PutFile(ctx, "/kist/log503", fo, nil); err != nil {
		t.Fatal(err)
	}
	s := buf.String()
	if !strings.Contains(s, "level=WARN") || !strings.Contains(s, "webdav 请求失败") {
		t.Fatalf("重试应记 Warn 日志: %q", s)
	}
	if strings.Contains(s, `"p"`) { // 测试服务密码为 p,绝不能出现在日志
		t.Fatalf("日志疑似泄露凭据: %q", s)
	}
}
