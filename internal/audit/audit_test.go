package audit

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kist/internal/errs"
	"kist/internal/logging"
)

// resetSummary 清掉包级摘要槽:CLI 一进程一命令没有残留问题,测试会连续跑多条。
func resetSummary() { summary = nil }

// readLines 读审计文件并按行拆分(拒绝尾随空行计数)。
func readLines(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(path())
	if err != nil {
		t.Fatalf("读 audit.log: %v", err)
	}
	s := strings.TrimRight(string(b), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// decodeLine 逐行解析 JSONL:一行必须是恰好一条完整 JSON 记录。
func decodeLine(t *testing.T, line string) Entry {
	t.Helper()
	var e Entry
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		t.Fatalf("行非法 JSON: %q: %v", line, err)
	}
	return e
}

// TestLogOneLinePerCommand 验收:每条命令恰好一行,时间/动作/耗时/成败俱全,
// 失败带错误码。
func TestLogOneLinePerCommand(t *testing.T) {
	t.Setenv("KIST_HOME", t.TempDir())
	resetSummary()
	defer resetSummary()

	Log("ls", time.Now(), nil)
	Log("unlock", time.Now(), errs.New(errs.AuthFailed, "口令不正确"))

	lines := readLines(t)
	if len(lines) != 2 {
		t.Fatalf("应恰好两行(每命令一条): %d", len(lines))
	}
	okEntry := decodeLine(t, lines[0])
	if !okEntry.OK || okEntry.Cmd != "ls" || okEntry.Code != "" || okEntry.DurMS < 0 {
		t.Fatalf("成功条目字段不符: %+v", okEntry)
	}
	if _, err := time.Parse(time.RFC3339, okEntry.TS); err != nil {
		t.Fatalf("ts 应为 RFC3339: %q: %v", okEntry.TS, err)
	}
	failEntry := decodeLine(t, lines[1])
	if failEntry.OK || failEntry.Cmd != "unlock" ||
		failEntry.Code != errs.AuthFailed || failEntry.Err != "口令不正确" {
		t.Fatalf("失败条目应带动作与错误码: %+v", failEntry)
	}

	// 非 AppError 的裸错误归 INTERNAL
	Log("gc", time.Now(), errors.New("意外"))
	e := decodeLine(t, readLines(t)[2])
	if e.Code != errs.Internal {
		t.Fatalf("裸错误应归 INTERNAL: %+v", e)
	}
}

// TestLogEscapesChineseAndSpaces 验收:中文文件名、含空格路径转义正确,
// 一行一条不串行。
func TestLogEscapesChineseAndSpaces(t *testing.T) {
	t.Setenv("KIST_HOME", t.TempDir())
	resetSummary()
	defer resetSummary()

	Set(map[string]any{"name": "漫画 第01话 测试.zip", "dest": "/作品集/新版"})
	Log("put", time.Now(), nil)

	lines := readLines(t)
	if len(lines) != 1 {
		t.Fatalf("值里的空格不得断行: %d 行", len(lines))
	}
	e := decodeLine(t, lines[0])
	if e.Extra["name"] != "漫画 第01话 测试.zip" || e.Extra["dest"] != "/作品集/新版" {
		t.Fatalf("中文与空格应原样往返: %+v", e.Extra)
	}
}

// TestLogRedactsCredentialsAndSecrets 验收:口令/凭据永不落盘——
// unlock 失败只记错误码与消息;URL 里手滑带的 userinfo 被脱敏。
func TestLogRedactsCredentialsAndSecrets(t *testing.T) {
	t.Setenv("KIST_HOME", t.TempDir())
	resetSummary()
	defer resetSummary()

	const pass = "S3cret口令!"
	// 模拟 unlock 口令试错:审计收到的只有错误码与不含口令的消息
	Log("unlock", time.Now(), errs.New(errs.AuthFailed, "口令不正确"))
	// 模拟上游错误消息回显带凭据的 URL
	Log("config set", time.Now(), errs.New(errs.DavError,
		`Post "https://user:TopSecret@example.com/dav/kist/keyfile": 401`))

	b, err := os.ReadFile(path())
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, secret := range []string{pass, "TopSecret"} {
		if strings.Contains(s, secret) {
			t.Fatalf("机密 %q 不得落盘:\n%s", secret, s)
		}
	}
	if !strings.Contains(s, "xxxxx") {
		t.Fatalf("URL 凭据应被脱敏:\n%s", s)
	}
}

// TestLogTruncatesNoisyMessages 多行消息只留首行,超长按 rune 截断,
// 不产生半个汉字,也不影响 JSONL 一行一条。
func TestLogTruncatesNoisyMessages(t *testing.T) {
	t.Setenv("KIST_HOME", t.TempDir())
	resetSummary()
	defer resetSummary()

	Log("bogus", time.Now(), errs.New(errs.BadConfig,
		"未知子命令 \"bogus\"\n"+strings.Repeat("用法行 ", 500)))

	lines := readLines(t)
	if len(lines) != 1 {
		t.Fatalf("多行消息不得断行: %d 行", len(lines))
	}
	e := decodeLine(t, lines[0])
	if !strings.HasPrefix(e.Err, "未知子命令 \"bogus\"") {
		t.Fatalf("应只留首行: %q", e.Err)
	}
	runes := []rune(e.Err)
	if len(runes) > errMaxRunes+1 { // 截断尾巴带省略号
		t.Fatalf("应截断到上限附近: %d runes", len(runes))
	}
}

// TestSetMergesSummary 摘要槽逐键合并:不同阶段各登记一部分,互不覆盖。
func TestSetMergesSummary(t *testing.T) {
	t.Setenv("KIST_HOME", t.TempDir())
	resetSummary()
	defer resetSummary()

	Set(map[string]any{"files": 3})
	Set(map[string]any{"bytes": int64(42)})
	Log("put", time.Now(), nil)

	e := decodeLine(t, readLines(t)[0])
	if e.Extra["files"] != float64(3) || e.Extra["bytes"] != float64(42) {
		t.Fatalf("摘要应合并齐全: %+v", e.Extra)
	}
}

// TestFilePerm0600 审计文件新建时权限 0600(与 kist.log/keyfile 同级)。
func TestFilePerm0600(t *testing.T) {
	t.Setenv("KIST_HOME", t.TempDir())
	resetSummary()
	defer resetSummary()

	Log("version", time.Now(), nil)
	st, err := os.Stat(path())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("权限应为 0600: %v", st.Mode().Perm())
	}
}

// TestAuditSurvivesKistLogRotation 验收:长留存——kist.log 轮转不影响
// audit.log,历史记录原样保留且可继续追加。
func TestAuditSurvivesKistLogRotation(t *testing.T) {
	t.Setenv("KIST_HOME", t.TempDir())
	resetSummary()
	defer resetSummary()

	Log("rm", time.Now(), nil)
	before := readLines(t)

	// 造一个超阈值的 kist.log,触发下次启动轮转
	big := strings.Repeat("x", 5<<20)
	if err := os.WriteFile(filepath.Join(os.Getenv("KIST_HOME"), "kist.log"), []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := logging.Setup(); err != nil {
		t.Fatal(err)
	}

	if got := readLines(t); len(got) != len(before) {
		t.Fatalf("轮转后审计历史应原样保留: %d → %d", len(before), len(got))
	}
	Log("rm", time.Now(), nil)
	if got := readLines(t); len(got) != len(before)+1 {
		t.Fatalf("轮转后应可继续追加: %d", len(got))
	}
}
