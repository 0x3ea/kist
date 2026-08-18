// Package config 管理本地路径与 config.json。
//
// 数据根目录默认 os.UserConfigDir()/kist,可用环境变量 KIST_HOME 覆盖
// (单元测试与多设备模拟依赖此机制)。config.json 权限 0600,密码仅在
// 用户显式选择"记住密码"时落盘。
// 设计文档:docs/phase-4-pipeline-cli.md
package config
