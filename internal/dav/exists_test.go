package dav

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestProbeExactRequest 探测必须是精确路径 PROPFIND Depth 0 并带回大小,
// 不得触发整目录列举(TODO-11 验收)。
func TestProbeExactRequest(t *testing.T) {
	srv, fi := newTestServer(t)
	c := newFastClient(t, srv)
	ctx := context.Background()
	if err := c.EnsureRoot(ctx); err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp("", "kist-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.Write([]byte("xyz")); err != nil { // 3 字节:大小可核
		t.Fatal(err)
	}
	f.Close()
	fo, err := os.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer fo.Close()
	if err := c.PutFile(ctx, "/kist/probe0001", fo, nil); err != nil {
		t.Fatal(err)
	}

	fi.mu.Lock()
	start := len(fi.reqLog)
	fi.mu.Unlock()

	found, size, err := c.Probe(ctx, "/kist/probe0001")
	if err != nil || !found {
		t.Fatalf("存在探测: found=%v err=%v", found, err)
	}
	if size != 3 {
		t.Fatalf("应解析到大小 3,得到 %d", size)
	}
	found, size, err = c.Probe(ctx, "/kist/absent9999")
	if err != nil || found || size != 0 {
		t.Fatalf("不存在探测: found=%v size=%d err=%v", found, size, err)
	}

	reqs := fi.requests()[start:]
	if len(reqs) != 2 {
		t.Fatalf("应恰好 2 个探测请求: %v", reqs)
	}
	for _, r := range reqs {
		if !strings.HasPrefix(r, "PROPFIND Depth=0 ") {
			t.Fatalf("必须是精确路径 Depth 0 探测,禁止整目录列举: %v", reqs)
		}
	}
	if !strings.Contains(reqs[0], "/kist/probe0001") || !strings.Contains(reqs[1], "/kist/absent9999") {
		t.Fatalf("探测路径不符: %v", reqs)
	}
}

// TestProbeNetworkError 404 与网络错误的行为与原整列实现等价:
// 断连必须上抛 error,不能误判为"不存在"。
func TestProbeNetworkError(t *testing.T) {
	srv, _ := newTestServer(t)
	c := newFastClient(t, srv)
	srv.Close()
	if _, _, err := c.Probe(context.Background(), "/kist/x"); err == nil {
		t.Fatal("网络错误应返回 error 而非 false")
	}
}
