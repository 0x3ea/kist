package dav

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/studio-b12/gowebdav"
)

// Config 是 WebDAV 连接配置;RootPath 为远端对象根目录,默认 "/kist"。
type Config struct {
	URL      string
	Username string
	Password string
	RootPath string
}

// RemoteObject 是远端根目录下的一个对象(PROPFIND Depth 1 的结果项)。
type RemoteObject struct {
	Name    string
	Size    int64
	ModTime time.Time
}

// Client 是本包对外暴露的最小 WebDAV 语义;所有方法接受 ctx 用于取消控制。
// 网络可靠性(重试/退避/429)全部封装在实现里,上层不感知 HTTP。
type Client interface {
	// Ping 验证地址与凭据可用。
	Ping(ctx context.Context) error
	// EnsureRoot 创建根目录;已存在(405)视为成功,幂等。
	EnsureRoot(ctx context.Context) error
	// PutFile 整文件上传:显式设置 Content-Length(部分保守网盘拒绝
	// chunked 编码的 PUT);失败整体重试,每次重试从头重传;
	// prog 以密文累计字节回调(64KiB 粒度,可为 nil)。
	PutFile(ctx context.Context, remotePath string, f *os.File, prog func(int64)) error
	// GetToFile 流式下载到本地文件;prog 以累计字节回调(64KiB 粒度,
	// 节流由上层负责;重试会从头重传,prog 可能回退后再增长)。
	GetToFile(ctx context.Context, remotePath, localPath string, prog func(int64)) (int64, error)
	// GetBody 流式打开 GET 响应体,调用方负责 Close。单次尝试、内部
	// 不重试——流不可回卷,重传由调用方在整对象粒度重新发起(migrate)。
	GetBody(ctx context.Context, remotePath string) (io.ReadCloser, error)
	// PutStream 以显式定长 PUT 一个不可回卷的流(size 取自源端枚举,
	// 兼容拒绝 chunked 的保守网盘)。同样单次尝试,重试由调用方包装
	// 整对象(新 GET + 新 PUT)。
	PutStream(ctx context.Context, remotePath string, size int64, body io.Reader) error
	// List 列出根目录下的全部对象。
	List(ctx context.Context) ([]RemoteObject, error)
	// Probe 精确路径探测:PROPFIND Depth 0 只问该资源本身,不列整目录
	// (O(1),不随库规模增长);返回是否存在与服务器报告的字节大小
	// (-1 = 未提供)。404 → (false, 0, nil),网络/权限等其余错误
	// 原样返回(TODO-11/13)。
	Probe(ctx context.Context, remotePath string) (bool, int64, error)
	Delete(ctx context.Context, remotePath string) error
	Move(ctx context.Context, oldPath, newPath string) error
}

type client struct {
	cfg   Config
	gc    *gowebdav.Client // PROPFIND/MKCOL/DELETE/MOVE:复用其解析与多状态处理
	hc    *http.Client     // PUT/GET 自管:需要 ctx、Content-Length 与 Retry-After
	retry *Retrier
}

// New 构造客户端;URL 必填,RootPath 缺省 "/kist"。
func New(cfg Config) (Client, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("dav: URL 未配置")
	}
	if cfg.RootPath == "" {
		cfg.RootPath = "/kist"
	}
	cfg.RootPath = "/" + strings.Trim(cfg.RootPath, "/")
	gc := gowebdav.NewClient(cfg.URL, cfg.Username, cfg.Password)
	// 不设整体 Timeout:大文件传输的时限由调用方 ctx 控制,
	// 这里只约束建立连接与等待响应头两个阶段
	hc := &http.Client{
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			MaxIdleConnsPerHost:   4,
			IdleConnTimeout:       90 * time.Second,
		},
	}
	return &client{cfg: cfg, gc: gc, hc: hc, retry: NewRetrier()}, nil
}

func (c *client) Ping(ctx context.Context) error {
	return c.retry.Do(ctx, func() error { return c.gc.Connect() })
}

func (c *client) EnsureRoot(ctx context.Context) error {
	return c.retry.Do(ctx, func() error {
		err := c.gc.Mkdir(c.cfg.RootPath, 0o755)
		if err != nil && gowebdav.IsErrCode(err, http.StatusMethodNotAllowed) {
			return nil // 405 = 目录已存在,MKCOL 的预期幂等结果
		}
		return err
	})
}

func (c *client) PutFile(ctx context.Context, remotePath string, f *os.File, prog func(int64)) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	return c.retry.Do(ctx, func() error {
		// http.Transport 结束请求时会 Close 请求体——直接把 *os.File 当 body,
		// 首次尝试后句柄就被关掉,重试必失败(测试驱动发现)。
		// 用 SectionReader + NoCloser 包装:每次尝试从偏移 0 重新读,
		// 句柄生命周期留在调用方手里;外层 progressReader 顺带上报进度。
		rd := func() (io.ReadCloser, error) {
			return io.NopCloser(&progressReader{r: io.NewSectionReader(f, 0, st.Size()), prog: prog}), nil
		}
		body, _ := rd()
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.url(remotePath), body)
		if err != nil {
			return err
		}
		req.ContentLength = st.Size() // 关键:带定长,避免 chunked 编码
		req.GetBody = rd              // 支持极少数网盘的 307/308 重定向重发
		c.auth(req)
		resp, err := c.hc.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10)) // 排空以便复用连接
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		return &StatusError{Op: "PUT", Path: remotePath, Code: resp.StatusCode,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	})
}

func (c *client) GetToFile(ctx context.Context, remotePath, localPath string, prog func(int64)) (n int64, err error) {
	err = c.retry.Do(ctx, func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(remotePath), nil)
		if err != nil {
			return err
		}
		c.auth(req)
		resp, err := c.hc.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
			return &StatusError{Op: "GET", Path: remotePath, Code: resp.StatusCode,
				RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
		}
		out, err := os.OpenFile(localPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		defer out.Close()
		w, err := copyWithProg(out, resp.Body, prog)
		if err != nil {
			return err
		}
		n = w
		return nil
	})
	return n, err
}

// GetBody 打开流式 GET;详见接口注释。
func (c *client) GetBody(ctx context.Context, remotePath string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(remotePath), nil)
	if err != nil {
		return nil, err
	}
	c.auth(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		return nil, &StatusError{Op: "GET", Path: remotePath, Code: resp.StatusCode,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	return resp.Body, nil
}

// PutStream 定长 PUT 一个不可回卷的流;详见接口注释。
func (c *client) PutStream(ctx context.Context, remotePath string, size int64, body io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.url(remotePath), body)
	if err != nil {
		return err
	}
	req.ContentLength = size // 关键:带定长,避免 chunked 编码
	c.auth(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return &StatusError{Op: "PUT", Path: remotePath, Code: resp.StatusCode,
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
}

// progressReader 把读取量转成累计字节回调。
type progressReader struct {
	r    io.Reader
	n    int64
	prog func(int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 && p.prog != nil {
		p.n += int64(n)
		p.prog(p.n)
	}
	return n, err
}

func copyWithProg(dst io.Writer, src io.Reader, prog func(int64)) (int64, error) {
	buf := make([]byte, 64<<10)
	var total int64
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
			if prog != nil {
				prog(total)
			}
		}
		if rerr == io.EOF {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}

func (c *client) List(ctx context.Context) ([]RemoteObject, error) {
	var out []RemoteObject
	err := c.retry.Do(ctx, func() error {
		fis, err := c.gc.ReadDir(c.cfg.RootPath)
		if err != nil {
			return err
		}
		out = make([]RemoteObject, 0, len(fis))
		for _, fi := range fis {
			out = append(out, RemoteObject{Name: fi.Name(), Size: fi.Size(), ModTime: fi.ModTime()})
		}
		return nil
	})
	return out, err
}

// propfindBody 最小 PROPFIND 请求体:resourcetype 判存在性,
// getcontentlength 供 outbox verify 核对大小(TODO-13)。
const propfindBody = `<?xml version="1.0" encoding="utf-8"?><d:propfind xmlns:d="DAV:"><d:prop><d:resourcetype/><d:getcontentlength/></d:prop></d:propfind>`

// Probe 对精确路径发 PROPFIND Depth 0:只问这一个资源,不触发整目录列举。
// 服务器普遍返回 207 Multi-Status,少数宽松实现直接 200,均视为存在,
// 并从响应解析 getcontentlength(解析不到返回 -1,verify 侧据此跳过大小核对);
// 404 视为不存在,其余状态(401/403/5xx…)以 StatusError 上抛——
// "查无此物"与"查询失败"必须区分,否则新设备检测会把权限问题误判为未初始化。
func (c *client) Probe(ctx context.Context, remotePath string) (bool, int64, error) {
	found, size := false, int64(0)
	err := c.retry.Do(ctx, func() error {
		req, err := http.NewRequestWithContext(ctx, "PROPFIND", c.url(remotePath), strings.NewReader(propfindBody))
		if err != nil {
			return err
		}
		req.Header.Set("Depth", "0")
		req.Header.Set("Content-Type", "application/xml")
		c.auth(req)
		resp, err := c.hc.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			found = true
			if n, ok := parseContentLength(body); ok {
				size = n
			} else {
				size = -1
			}
			return nil
		case resp.StatusCode == http.StatusNotFound:
			found, size = false, 0
			return nil
		default:
			return &StatusError{Op: "PROPFIND", Path: remotePath, Code: resp.StatusCode,
				RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
		}
	})
	return found, size, err
}

// reContentLength 匹配任意命名空间前缀(d:/D:/无前缀)的 getcontentlength。
var reContentLength = regexp.MustCompile(`(?i)<[A-Za-z0-9_.-]*:?getcontentlength>\s*(-?\d+)\s*<`)

func parseContentLength(body []byte) (int64, bool) {
	m := reContentLength.FindSubmatch(body)
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseInt(string(m[1]), 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func (c *client) Delete(ctx context.Context, remotePath string) error {
	return c.retry.Do(ctx, func() error { return c.gc.Remove(remotePath) })
}

func (c *client) Move(ctx context.Context, oldPath, newPath string) error {
	return c.retry.Do(ctx, func() error { return c.gc.Rename(oldPath, newPath, false) })
}

func (c *client) auth(req *http.Request) {
	if c.cfg.Username != "" || c.cfg.Password != "" {
		req.SetBasicAuth(c.cfg.Username, c.cfg.Password)
	}
}

// url 拼出绝对地址;路径逐段转义、保留 "/"(对象名是 32hex 本无特殊字符,
// 转义只是对异常路径的保险)。
func (c *client) url(remotePath string) string {
	p := remotePath
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return strings.TrimRight(c.cfg.URL, "/") + escapePath(p)
}

func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// StatusError 是自管请求(PUT/GET)的错误,携带状态码与 Retry-After 时长。
type StatusError struct {
	Op         string
	Path       string
	Code       int
	RetryAfter time.Duration // 0 表示服务端未提供
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("dav: %s %s: HTTP %d", e.Op, e.Path, e.Code)
}

// parseRetryAfter 只识别"秒数"形式;HTTP-date 形式忽略(由常规退避兜底)。
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if sec, err := strconv.Atoi(v); err == nil && sec > 0 {
		return time.Duration(sec) * time.Second
	}
	return 0
}

// statusCodeOf 从 gowebdav 返回的 *os.PathError 中剥出 HTTP 状态码。
func statusCodeOf(err error) (int, bool) {
	var pe *os.PathError
	if errors.As(err, &pe) {
		if se, ok := pe.Err.(gowebdav.StatusError); ok {
			return se.Status, true
		}
	}
	if se, ok := err.(gowebdav.StatusError); ok {
		return se.Status, true
	}
	return 0, false
}
