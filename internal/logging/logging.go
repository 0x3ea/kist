package logging

import (
	"log/slog"
	"os"
	"path/filepath"

	"kist/internal/config"
)

const (
	logFileName = "kist.log"
	// rotateBytes 轮转阈值:启动时现有日志超过该值即改名 .old,防无限增长。
	rotateBytes = 5 << 20
)

// Setup 初始化全局日志并 slog.SetDefault 生效。
// 落点 KIST_HOME/kist.log,跟随沙盒/正式环境隔离;句柄随进程生命周期持有,
// 不提供关闭(CLI 单次运行、GUI 常驻皆适用)。失败返回错误,调用方应降级为
// slog 默认(标准错误)而非阻断启动。
func Setup() error {
	path := filepath.Join(config.HomeDir(), logFileName)
	if err := rotate(path); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo})))
	return nil
}

// rotate 启动期轮转:现有日志超过阈值则改名为 .old。先显式删除上一代再改名,
// 不依赖 Rename 的覆盖语义(Windows 上目标已存在会失败)。
func rotate(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if st.Size() < rotateBytes {
		return nil
	}
	if err := os.Remove(path + ".old"); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(path, path+".old")
}
