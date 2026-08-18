# Phase 2 — WebDAV 客户端层 internal/dav

> 状态:已完成
> 前置:Phase 1(仅 go.mod 共享,无代码依赖,可与 Phase 3 并行)
> 产出:`internal/dav/`(client.go、retry.go、client_test.go)

## 目标

一个对任意 WebDAV 提供商保守兼容的客户端层:整文件 PUT(带 Content-Length)、流式 GET、列表、删除、移动,统一的重试退避。**网络可靠性全部封装在这层,上层不感知 HTTP。**

## 要做什么(任务清单)

- [x] `go get github.com/studio-b12/gowebdav golang.org/x/net`
- [x] `retry.go`:可注入睡眠的退避重试器
- [x] `client.go`:Client 接口 + gowebdav 实现 + 自实现 `PutFile`
- [x] `client_test.go`:httptest + x/net/webdav 内存文件系统的全套测试(含故障注入)

实施要点(与文档设计的差异说明):
- 所有 Client 方法第一个参数为 `ctx`(文档签名没写):PUT/GET 挂在请求上可中途取消,其余操作在重试边界检查
- PUT 请求体必须用 `SectionReader + NoCloser` 包装而不能直接传 `*os.File`:`http.Transport` 结束请求时会 Close 请求体,裸传文件句柄在首次重试时已关闭(测试驱动发现);`GetBody` 一并补齐以支持 307/308 重定向
- 分工:PUT/GET 自管 `http.Client`(需要 ctx/Content-Length/Retry-After 头),PROPFIND/MKCOL/DELETE/MOVE 复用 gowebdav v0.13.0(其 `StatusError` 包在 `*os.PathError` 里,`IsErrCode`/`statusCodeOf` 做统一分类)
- `isRetryable` 先排除 `context.Canceled/DeadlineExceeded`:`*url.Error` 实现了 `net.Error`,否则 ctx 取消会被误判为可重试
- gowebdav 方法本身不带 ctx,取消粒度为"重试边界"(自管 PUT/GET 为传输中途)

## 设计说明

### 接口

```go
type Config struct {
    URL, Username, Password string
    RootPath string // 默认 "/kist"
}

type RemoteObject struct {
    Name string; Size int64; ModTime time.Time
}

type Client interface {
    Ping() error                                   // PROPFIND Depth 0,验证地址+凭据
    EnsureRoot() error                             // MKCOL RootPath,已存在(405)不算错
    PutFile(remotePath string, f *os.File) error   // ★ 自实现
    GetToFile(remotePath, localPath string, prog func(int64)) (int64, error)
    List() ([]RemoteObject, error)                 // PROPFIND Depth 1 于 RootPath
    Delete(remotePath string) error
    Move(oldPath, new string) error
}
```

### PutFile 为什么自实现

gowebdav 的 `WriteStream` 接 `io.Reader`,HTTP 层走 chunked 编码、不带 Content-Length,部分保守网盘会拒绝。我们的流程总是"先加密到本地临时文件",大小已知,因此自己发请求:

- `http.NewRequest(PUT, join(url, path))`,`req.ContentLength = stat.Size()`,`body = f`(先 Seek 到 0),Basic Auth
- 2xx 成功;4xx 致命;5xx/网络错误交重试器
- 路径拼接:远端对象名固定为 `32hex` / `keyfile` / `index.enc`,无特殊字符;实现统一走 `url.PathEscape` 更稳妥

### 重试器(retry.go)

```go
type Retrier struct {
    Max  int           // 默认 5 次
    Base time.Duration // 1s
    Cap  time.Duration // 30s
    Sleep func(ctx context.Context, d time.Duration) error // 可注入,测试中即时返回
}
func (r *Retrier) Do(ctx context.Context, op func() error) error
```

- **只重试**:网络错误/超时、HTTP ≥ 500、429;其余 4xx(401/403/404/409…)立即失败
- 退避:Base×2^n 封顶 Cap,±20% 抖动;响应带 `Retry-After`(秒)则取 `max(retryAfter, backoff)`
- 每次尝试与每次睡眠前检查 `ctx.Done()`
- 幂等性依据:PUT 目标总是全新随机名,GET 天然幂等,MKCOL 忽略"已存在"

## 预期结果

上层通过 `Client` 接口操作远端,完全不知道 gowebdav 的存在;临时网络故障(5xx/超时)对调用方透明。

## 验收标准

1. `go test ./internal/dav/ -race -count=1` 全绿,测试基于 `httptest.Server` + `webdav.Handler{FileSystem: webdav.MemFS(), LockSystem: webdav.NewMemLS()}`,至少覆盖:
   - `EnsureRoot` 幂等:连续两次调用不报错
   - `PutFile`/`GetToFile` 往返字节一致;**服务端断言收到的 `Content-Length` 等于文件大小**(防回归到 chunked)
   - `List`/`Delete`/`Move` 正常工作
   - 故障注入中间件:前 2 次返回 503 → 第 3 次成功(验证重试);持续 404 → 不重试直接失败;带 `Retry-After: 0` 的 429 → 按 Retry-After 处理
   - ctx 已取消 → 立即返回,不睡眠
2. `go vet ./...` 通过
