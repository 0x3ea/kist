// Package e2e 存放端到端测试:在 httptest 起的本地 WebDAV 服务上跑完整管线
// (初始化 → 上传 → 浏览/搜索 → 下载比对 → 删除/清理 → 备份恢复)。
// 设计文档:docs/phase-4-pipeline-cli.md、docs/phase-5-backup-sync.md 的验收部分
package e2e
