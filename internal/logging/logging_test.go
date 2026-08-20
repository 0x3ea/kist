package logging

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func logPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(os.Getenv("KIST_HOME"), logFileName)
}

// TestSetupRotatesOversizedLog 超阈值轮转为 .old 且只留一代(TODO-07 验收)。
func TestSetupRotatesOversizedLog(t *testing.T) {
	t.Setenv("KIST_HOME", t.TempDir())
	big := bytes.Repeat([]byte("x"), rotateBytes+1)
	if err := os.WriteFile(logPath(t), big, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Setup(); err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(logPath(t) + ".old")
	if err != nil || len(old) != len(big) {
		t.Fatalf(".old 应保留旧日志: len=%d err=%v", len(old), err)
	}
	slog.Info("轮转测试标记")
	b, err := os.ReadFile(logPath(t))
	if err != nil || !strings.Contains(string(b), "轮转测试标记") {
		t.Fatalf("新日志应写入新文件: %q %v", b, err)
	}

	// 模拟下次启动再次超阈值:.old 被替换,仍只有一代
	f, err := os.OpenFile(logPath(t), os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(big); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := Setup(); err != nil {
		t.Fatal(err)
	}
	gens, err := filepath.Glob(logPath(t) + ".old*")
	if err != nil || len(gens) != 1 {
		t.Fatalf(".old 只允许留一代: %v %v", gens, err)
	}
}

// TestSetupAppendsBelowThreshold 未超阈值走追加,分级正确。
func TestSetupAppendsBelowThreshold(t *testing.T) {
	t.Setenv("KIST_HOME", t.TempDir())
	if err := os.WriteFile(logPath(t), []byte("time=2026 level=INFO msg=旧记录\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Setup(); err != nil {
		t.Fatal(err)
	}
	slog.Warn("追加测试标记")
	b, err := os.ReadFile(logPath(t))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "旧记录") || !strings.Contains(s, "追加测试标记") {
		t.Fatalf("未超阈值应追加而非轮转: %q", s)
	}
	if !strings.Contains(s, "level=WARN") {
		t.Fatalf("分级应正确(Warn): %q", s)
	}
}
