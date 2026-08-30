// Package config 管理本地路径与 config.json。
//
// 数据根目录默认 os.UserConfigDir()/kist,可用环境变量 KIST_HOME 覆盖
// (单元测试与多设备模拟依赖此机制)。config.json 权限 0600,密码仅在
// 用户显式选择"记住密码"时落盘(TODO-21 起逐盘判断)。
//
// schema v2(TODO-21 多网盘多库):drives[] + active + 全局 Settings,
// 一盘一库——每个 Drive 是独立库(自己的 blobs/index.enc/本地索引文件
// index-<盘ID>.db),共用同一把本地 keyfile;旧单盘格式首次 Load 自动迁移
// 落盘(盘 ID 随机生成靠落盘钉死),索引文件的物理迁移走 index.MigrateIndexFile。
// 设计文档:docs/phase-4-pipeline-cli.md、docs/todo/21-multi-drive-vaults.md
package config
