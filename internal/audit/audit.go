package audit

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"kist/internal/config"
	"kist/internal/errs"
)

const auditFileName = "audit.log"

// errMaxRunes 失败消息截断长度:留下定位所需的首行,挡掉 usage 回显这类
// 大段噪音(未知子命令会把整份 usage 塞进错误消息)。
const errMaxRunes = 300

// Entry 是 audit.log 的一行:一次命令调用 = 一条记录。
// 隐私红线(与 kist.log 同源):只记命令、路径、大小、错误码、耗时;
// 永不记口令、密钥、URL 凭据、文件内容。
type Entry struct {
	TS    string         `json:"ts"`              // RFC3339,命令结束时刻
	Cmd   string         `json:"cmd"`             // 动作名(组命令带子动作,如 "outbox push")
	OK    bool           `json:"ok"`              // 命令退出成败
	Code  string         `json:"code,omitempty"`  // 失败时的 AppError 错误码
	Err   string         `json:"err,omitempty"`   // 失败消息(取首行、截断、脱敏)
	DurMS int64          `json:"dur_ms"`          // 耗时(毫秒)
	Extra map[string]any `json:"extra,omitempty"` // 命令摘要(put 的 files/bytes、migrate 的 copied/skipped 等)
}

// summary 本进程命令的摘要槽:CLI 一进程一命令,子命令收尾前经 Set 登记,
// main 统一在 Log 落盘。登记点都在单 goroutine 收尾处,无需并发保护。
var summary map[string]any

// Set 登记本命令的摘要字段,键自定、重复 Set 逐键合并(不同阶段各登记
// 一部分,如 put 先记 files 后记 bytes)。
func Set(fields map[string]any) {
	if summary == nil {
		summary = fields
		return
	}
	for k, v := range fields {
		summary[k] = v
	}
}

// Log 在命令收口处写一条审计记录(main 每次调用恰好一次,成败都记)。
// runErr 非 nil 时提取 AppError 错误码,其余归 INTERNAL。
// 写失败只警告 stderr、不改变命令退出码:审计是观测数据,不能反过来
// 阻断它观测的命令。
func Log(cmd string, start time.Time, runErr error) {
	e := Entry{
		TS:    time.Now().Format(time.RFC3339),
		Cmd:   cmd,
		OK:    runErr == nil,
		DurMS: time.Since(start).Milliseconds(),
		Extra: summary,
	}
	if runErr != nil {
		var ae *errs.AppError
		if !errors.As(runErr, &ae) {
			ae = &errs.AppError{Code: errs.Internal, Msg: runErr.Error()}
		}
		e.Code, e.Err = ae.Code, redact(truncate(ae.Msg))
	}
	if err := appendLine(e); err != nil {
		fmt.Fprintln(os.Stderr, "警告:审计日志写入失败:", err)
	}
	summary = nil // 一命令一摘要,写完即清,残留不得漏进下一条记录
}

// path 审计文件落点:KIST_HOME/audit.log。与 kist.log 同目录但独立生命周期
// ——不随 kist.log 轮转,长留存(事后取证的前提)。
func path() string { return filepath.Join(config.HomeDir(), auditFileName) }

// appendLine 追加一行 JSONL。O_APPEND 让多进程并发写也不会交错出半行;
// json.Marshal 把换行/中文/引号全部转义,一行天然就是一条完整记录。
func appendLine(e Entry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// truncate 失败消息瘦身:先取首行(usage 类多行消息只留第一行),再按
// rune 边界截断,不产生半个汉字。
func truncate(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) <= errMaxRunes {
		return s
	}
	return string(r[:errMaxRunes]) + "…"
}

// credRe 匹配 URL 里的 userinfo(user:pass@):--url 手滑带凭据时,上游
// 错误消息可能原样回显。net/http 层已把密码替换为 xxxxx,这里兜底其余
// 形态;只对审计落盘的文本做,不改错误传递链。
var credRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)([^/@\s:]+):([^/@\s]+)@`)

func redact(s string) string { return credRe.ReplaceAllString(s, `${1}${2}:xxxxx@`) }
