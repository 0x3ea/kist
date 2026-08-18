// Package remote 定义远端对象语义:keyfile / blob / index.enc 的统一读写与列表。
//
// 远端布局为扁平:网盘 /kist/ 目录下只有随机名(32hex)的加密 blob 与两个保留名
// (keyfile、index.enc),不携带任何明文信息。
// 设计文档:docs/phase-4-pipeline-cli.md
package remote
