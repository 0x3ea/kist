package dav

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestCheckPanicsOnInvalid 非法配置(Max<1 / Base≤0 / Cap<Base)必须在
// op 执行之前 panic——校验发生在 Do 入口(调用之前),而非默默回填默认值。
func TestCheckPanicsOnInvalid(t *testing.T) {
	cases := []struct {
		name string
		r    Retrier
	}{
		{"零值(部分构造)", Retrier{}},
		{"Max=0", Retrier{Base: time.Second, Cap: 30 * time.Second}},
		{"Max<0", Retrier{Max: -1, Base: time.Second, Cap: 30 * time.Second}},
		{"Base=0", Retrier{Max: 5, Cap: 30 * time.Second}},
		{"Cap<Base", Retrier{Max: 5, Base: time.Second, Cap: 500 * time.Millisecond}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			defer func() {
				v := recover()
				if v == nil {
					t.Error("非法配置应 panic,实际正常返回")
					return
				}
				if !strings.Contains(fmt.Sprint(v), "Retrier 配置非法") {
					t.Errorf("panic 消息应指明配置非法,得到: %v", v)
				}
				if called {
					t.Error("panic 应发生在 op 执行之前")
				}
			}()
			r := tc.r
			r.Sleep = func(context.Context, time.Duration) error { return nil }
			_ = r.Do(context.Background(), func() error { called = true; return nil })
		})
	}
}

// TestCheckAcceptsValid 合法配置照常工作:Max=1 是显式"不重试"的合法语义。
func TestCheckAcceptsValid(t *testing.T) {
	var calls int
	r := &Retrier{Max: 1, Base: time.Millisecond, Cap: 10 * time.Millisecond}
	r.Sleep = func(context.Context, time.Duration) error { return nil }
	if err := r.Do(context.Background(), func() error { calls++; return nil }); err != nil {
		t.Fatalf("合法配置不应报错: %v", err)
	}
	if calls != 1 {
		t.Fatalf("Max=1 时 op 执行 %d 次,期望恰好 1", calls)
	}
}

// TestNewRetrierPassesCheck 默认三元组必须过检(成功路径无睡眠,测试瞬时)。
func TestNewRetrierPassesCheck(t *testing.T) {
	if err := NewRetrier().Do(context.Background(), func() error { return nil }); err != nil {
		t.Fatalf("NewRetrier 默认配置不应报错: %v", err)
	}
}
