package transfer

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kist/internal/crypto"
)

// TestTempDirFailureLogged 临时目录清不掉/建不起来必须留痕(TODO-07 静默黑洞)。
func TestTempDirFailureLogged(t *testing.T) {
	// 让 tempRoot 的父链中隔一个普通文件:RemoveAll 无东西可删返回 nil,
	// 随后 MkdirAll 必败——正好覆盖原 `_ = e` 的静默分支
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", filepath.Join(blocker, "sub"))

	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)

	_ = NewManager(Deps{MK: func() (crypto.MasterKey, bool) { return crypto.MasterKey{}, true }})
	s := buf.String()
	if !strings.Contains(s, "临时目录失败") || !strings.Contains(s, "level=WARN") {
		t.Fatalf("临时目录失败应记 Warn: %q", s)
	}
}
