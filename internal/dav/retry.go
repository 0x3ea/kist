package dav

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"time"
)

// Retrier 指数退避重试器;Sleep 可注入,测试里让它瞬时返回。
type Retrier struct {
	Max   int           // 最大尝试次数(含首次),默认 5
	Base  time.Duration // 首次退避时长,默认 1s
	Cap   time.Duration // 退避上限,默认 30s
	Sleep func(ctx context.Context, d time.Duration) error
}

func NewRetrier() *Retrier {
	return &Retrier{Max: 5, Base: time.Second, Cap: 30 * time.Second}
}

// Do 执行 op:仅对"网络层错误 / HTTP 5xx / 429"重试,其余错误立即返回。
// 每次尝试与每次睡眠前都检查 ctx;错误携带 Retry-After 时取 max(Retry-After, 退避)。
// 幂等性由调用方保证:PUT 目标是全新随机名、GET 天然幂等、MKCOL 忽略"已存在"。
func (r *Retrier) Do(ctx context.Context, op func() error) error {
	maxN, base, top := r.norm()
	delay := base
	var lastErr error
	for attempt := 0; attempt < maxN; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := op()
		if err == nil {
			return nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !isRetryable(err) {
			return err
		}
		if attempt == maxN-1 {
			break
		}
		wait := jitter(delay)
		if ra := retryAfterOf(err); ra > wait {
			wait = ra
		}
		if err := r.sleep(ctx, wait); err != nil {
			return err
		}
		if delay *= 2; delay > top {
			delay = top
		}
	}
	return fmt.Errorf("dav: 重试 %d 次后仍失败: %w", maxN, lastErr)
}

func (r *Retrier) norm() (maxN int, base, top time.Duration) {
	maxN = r.Max
	if maxN < 1 {
		maxN = 1
	}
	base = r.Base
	if base <= 0 {
		base = time.Second
	}
	top = r.Cap
	if top < base {
		top = base
	}
	return
}

func (r *Retrier) sleep(ctx context.Context, d time.Duration) error {
	if r.Sleep != nil {
		return r.Sleep(ctx, d)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func retryAfterOf(err error) time.Duration {
	var se *StatusError
	if errors.As(err, &se) {
		return se.RetryAfter
	}
	return 0
}

// isRetryable 判定是否值得重试:
//   - 网络层错误(net.Error,含 *url.Error 包装的超时/断连)→ 重试
//   - HTTP ≥500 或 429(自管请求或 gowebdav 返回的)→ 重试
//   - ctx 取消/超时与其余 4xx(401/403/404/409…)→ 不重试
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	// *url.Error 也实现 net.Error,必须先排除 ctx 取消被误判为可重试
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code >= 500 || se.Code == http.StatusTooManyRequests
	}
	if code, ok := statusCodeOf(err); ok {
		return code >= 500 || code == http.StatusTooManyRequests
	}
	var ne net.Error
	return errors.As(err, &ne)
}

// jitter 给退避加 ±20% 抖动,避免多客户端同步重试风暴。
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}
