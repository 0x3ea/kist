package dav

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// NewRetrier 构造默认重试器。参数刻意写死、不进用户配置:退避是实现策略
// 而非用户意图,适应性由 Retry-After 优先与 ctx 取消提供;默认值有
// provider-notes 的实测依据(保守网盘限速脾性)。若未来参数暴露进
// config.json(外部输入),应改为构造期校验并返回 error,而非本包的 panic。
func NewRetrier() *Retrier {
	return &Retrier{Max: 5, Base: time.Second, Cap: 30 * time.Second}
}

// Do 执行 op:仅对"网络层错误 / HTTP 5xx / 429"重试,其余错误立即返回。
// 每次尝试与每次睡眠前都检查 ctx;错误携带 Retry-After 时取 max(Retry-After, 退避)。
// 幂等性由调用方保证:PUT 目标是全新随机名、GET 天然幂等、MKCOL 忽略"已存在"。
func (r *Retrier) Do(ctx context.Context, op func() error) error {
	maxN, base, top := r.check()
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
		// 重试留痕(TODO-07 静默黑洞):错误串只含方法/路径/状态码,不含凭据
		slog.Warn("webdav 请求失败,退避后重试", "attempt", attempt+1, "max", maxN,
			"backoff", wait.String(), "err", err)
		if err := r.sleep(ctx, wait); err != nil {
			return err
		}
		if delay *= 2; delay > top {
			delay = top
		}
	}
	return fmt.Errorf("dav: 重试 %d 次后仍失败: %w", maxN, lastErr)
}

// check 在 Do 入口校验配置并原样返回;非法即 panic。
// 非法值只能来自包内(不来自任何外部输入),对程序错误选立即爆出而非静默修复
// 此前的默认值回填会把笔误吞掉:
// Max=0 悄悄变 1(重试失效但系统照跑)
// Base=0 默认 1s(测试莫名变慢)
// Cap<base 退避曲线塌掉
// 放在 Do 入口而非构造器,是因为导出字段允许 &Retrier{...} 字面量部分构造与运行中改写,
// 只有消费点能覆盖所有构造路径。零值尤其危险:Max=0 会让 op 一次都不
// 执行,还返回 wrap 着 nil 的怪错误。
func (r *Retrier) check() (maxN int, base, top time.Duration) {
	if r.Max < 1 || r.Base <= 0 || r.Cap < r.Base {
		panic(fmt.Sprintf("dav: Retrier 配置非法(Max=%d, Base=%s, Cap=%s)", r.Max, r.Base, r.Cap))
	}
	return r.Max, r.Base, r.Cap
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
