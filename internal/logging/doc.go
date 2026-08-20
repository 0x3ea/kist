// Package logging 提供 kist 全局日志:标准库 log/slog,落盘 KIST_HOME/kist.log,
// 启动时超过 5MB 轮转为 .old(仅留一代)。入口(CLI main、GUI startup)各调一次
// Setup 即全局生效,核心代码直接用 slog 包级函数,不感知落点。
//
// 隐私红线:只记路径名、大小、错误码;永不记口令、密钥、URL 凭据、文件内容。
// 评估与验收标准见进度看板(docs/README.md)的 TODO 记录(原 TODO-07)。
package logging
