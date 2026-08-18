package dav

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/studio-b12/gowebdav"
	"golang.org/x/net/webdav"
)

// faultInjector 故障注入中间件:前 N 次请求返回指定状态码(可附 Retry-After);
// 同时记录请求计数与每次 PUT 的 Content-Length。
type faultInjector struct {
	mu             sync.Mutex
	failsRemaining int
	failCode       int
	retryAfter     string
	totalRequests  int
	putLengths     map[string]int64
}

func (f *faultInjector) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.totalRequests++
		if r.Method == http.MethodPut {
			if f.putLengths == nil {
				f.putLengths = map[string]int64{}
			}
			f.putLengths[r.URL.Path] = r.ContentLength
		}
		fail := f.failsRemaining > 0
		if fail {
			f.failsRemaining--
		}
		code, ra := f.failCode, f.retryAfter
		f.mu.Unlock()

		if fail {
			if ra != "" {
				w.Header().Set("Retry-After", ra)
			}
			if code == 0 {
				code = 503
			}
			w.WriteHeader(code)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (f *faultInjector) snapshot() (int, map[string]int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	put := make(map[string]int64, len(f.putLengths))
	for k, v := range f.putLengths {
		put[k] = v
	}
	return f.totalRequests, put
}

// newTestServer 起一个内存 WebDAV 服务(x/net/webdav + 临时目录)。
func newTestServer(t *testing.T) (*httptest.Server, *faultInjector) {
	t.Helper()
	fi := &faultInjector{}
	h := &webdav.Handler{
		FileSystem: webdav.Dir(t.TempDir()),
		LockSystem: webdav.NewMemLS(),
	}
	srv := httptest.NewServer(fi.wrap(h))
	t.Cleanup(srv.Close)
	return srv, fi
}

// newFastClient 用毫秒级退避构造客户端(测试不等秒)。
func newFastClient(t *testing.T, srv *httptest.Server) *client {
	t.Helper()
	cfg := Config{URL: srv.URL, Username: "u", Password: "p", RootPath: "/kist"}
	return &client{
		cfg:   cfg,
		gc:    gowebdav.NewClient(cfg.URL, cfg.Username, cfg.Password),
		hc:    srv.Client(),
		retry: &Retrier{Max: 5, Base: time.Millisecond, Cap: 10 * time.Millisecond},
	}
}

func pseudoData(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*131 + int(seed))
	}
	return b
}

func TestEnsureRootIdempotent(t *testing.T) {
	srv, _ := newTestServer(t)
	c := newFastClient(t, srv)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := c.EnsureRoot(ctx); err != nil {
			t.Fatalf("第 %d 次 EnsureRoot: %v", i+1, err)
		}
	}
}

func TestPutGetRoundTrip(t *testing.T) {
	srv, fi := newTestServer(t)
	c := newFastClient(t, srv)
	ctx := context.Background()
	if err := c.EnsureRoot(ctx); err != nil {
		t.Fatal(err)
	}

	data := pseudoData(1<<20, 7)
	src, err := os.CreateTemp("", "kist-put-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(src.Name())
	if _, err := src.Write(data); err != nil {
		t.Fatal(err)
	}
	src.Close()

	const name = "/kist/a1b2c3d4e5f60718293a4b5c6d7e8f90"
	f, err := os.Open(src.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := c.PutFile(ctx, name, f, nil); err != nil {
		t.Fatalf("PutFile: %v", err)
	}

	// PUT 必须带定长 Content-Length(-1 即 chunked,是防回归重点)
	_, puts := fi.snapshot()
	if got, ok := puts[name]; !ok || got != int64(len(data)) {
		t.Fatalf("PUT Content-Length = %d(ok=%v),期望 %d(绝不能是 -1)", got, ok, len(data))
	}

	dst := filepath.Join(t.TempDir(), "out.bin")
	var lastProg int64
	n, err := c.GetToFile(ctx, name, dst, func(done int64) { lastProg = done })
	if err != nil {
		t.Fatalf("GetToFile: %v", err)
	}
	if n != int64(len(data)) || lastProg != int64(len(data)) {
		t.Fatalf("下载数量/进度不符: n=%d prog=%d 期望 %d", n, lastProg, len(data))
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != string(data) {
		t.Fatalf("往返内容不一致(len=%d): %v", len(got), err)
	}
}

func TestListDeleteMove(t *testing.T) {
	srv, _ := newTestServer(t)
	c := newFastClient(t, srv)
	ctx := context.Background()
	if err := c.EnsureRoot(ctx); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"aaaa1111", "bbbb2222"} {
		f, err := os.CreateTemp("", "kist-*")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(pseudoData(100, name[0])); err != nil {
			t.Fatal(err)
		}
		f.Close()
		fo, err := os.Open(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		if err := c.PutFile(ctx, "/kist/"+name, fo, nil); err != nil {
			t.Fatalf("PutFile %s: %v", name, err)
		}
		fo.Close()
		os.Remove(f.Name())
	}

	objects, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objects) != 2 {
		t.Fatalf("对象数 = %d,期望 2: %+v", len(objects), objects)
	}

	if err := c.Delete(ctx, "/kist/aaaa1111"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := c.Move(ctx, "/kist/bbbb2222", "/kist/cccc3333"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	objects, err = c.List(ctx)
	if err != nil || len(objects) != 1 || objects[0].Name != "cccc3333" {
		t.Fatalf("删除+移动后列表不符: %+v %v", objects, err)
	}
}

func TestRetryOn503(t *testing.T) {
	srv, fi := newTestServer(t)
	c := newFastClient(t, srv)
	ctx := context.Background()
	if err := c.EnsureRoot(ctx); err != nil {
		t.Fatal(err)
	}
	before, _ := fi.snapshot()
	fi.mu.Lock()
	fi.failsRemaining = 2 // 前两次 PUT 吃 503,第三次应成功
	fi.mu.Unlock()

	f, err := os.CreateTemp("", "kist-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.Write(pseudoData(10, 1))
	f.Close()
	fo, _ := os.Open(f.Name())
	defer fo.Close()
	if err := c.PutFile(ctx, "/kist/retry503", fo, nil); err != nil {
		t.Fatalf("503 后重试应成功: %v", err)
	}
	after, _ := fi.snapshot()
	if after-before != 3 {
		t.Fatalf("PUT 请求次数 = %d,期望 3(两次 503 + 一次成功)", after-before)
	}
}

func TestNoRetryOn4xx(t *testing.T) {
	srv, fi := newTestServer(t)
	c := newFastClient(t, srv)
	ctx := context.Background()
	before, _ := fi.snapshot()
	fi.mu.Lock()
	fi.failsRemaining = 1
	fi.failCode = 404
	fi.mu.Unlock()

	f, err := os.CreateTemp("", "kist-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.Close()
	fo, _ := os.Open(f.Name())
	defer fo.Close()
	err = c.PutFile(ctx, "/kist/nope404", fo, nil)
	if err == nil {
		t.Fatal("404 应立即失败")
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 404 {
		t.Fatalf("期望 StatusError{404},得到 %v", err)
	}
	after, _ := fi.snapshot()
	if after-before != 1 {
		t.Fatalf("4xx 不应重试:请求次数 = %d,期望 1", after-before)
	}
}

func TestRetryAfterRespected(t *testing.T) {
	srv, fi := newTestServer(t)
	c := newFastClient(t, srv)
	if err := c.EnsureRoot(context.Background()); err != nil {
		t.Fatal(err)
	}
	fi.mu.Lock()
	fi.failsRemaining = 1
	fi.failCode = 429
	fi.retryAfter = "2" // 服务端要求等 2 秒
	fi.mu.Unlock()
	var slept []time.Duration
	c.retry.Sleep = func(ctx context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}

	f, err := os.CreateTemp("", "kist-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.Close()
	fo, _ := os.Open(f.Name())
	defer fo.Close()
	if err := c.PutFile(context.Background(), "/kist/ra429", fo, nil); err != nil {
		t.Fatalf("429 后应重试成功: %v", err)
	}
	if len(slept) != 1 || slept[0] < 2*time.Second {
		t.Fatalf("应遵守 Retry-After≥2s,实际睡眠: %v", slept)
	}
}

func TestCtxCanceled(t *testing.T) {
	srv, fi := newTestServer(t)
	c := newFastClient(t, srv)
	before, _ := fi.snapshot()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f, err := os.CreateTemp("", "kist-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.Close()
	fo, _ := os.Open(f.Name())
	defer fo.Close()
	err = c.PutFile(ctx, "/kist/canceled", fo, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("期望 context.Canceled,得到 %v", err)
	}
	after, _ := fi.snapshot()
	if after != before {
		t.Fatalf("ctx 已取消时不应发起任何请求")
	}
}
