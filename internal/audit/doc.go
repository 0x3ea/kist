// Package audit 实现操作审计日志(TODO-14):KIST_HOME/audit.log,
// 每条 CLI 命令恰好一行 JSONL,由 cmd/kistctl 的 main 单点收口写入。
//
// 与 kist.log 的分工:audit 回答 what/when(何时、何事、成败、耗时),
// kist.log 回答 why(传输起止、重试、孤儿的案发过程)——同一次失败两边
// 各就各位,时间戳可对齐。
//
// 格式定死 JSONL 的理由:字段异构、值含中文/空格;append-only 历史
// 格式一步定终身(中途换格式会新旧混杂);消费者是程序(GUI 活动历史页、
// jq)在先、人在后。审计是观测数据不是状态,永不进 SQLite(写库推高
// revision → 每敲一条命令触发一次备份放大)。
package audit
