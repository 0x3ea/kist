// Package transfer 实现上传/下载管线:worker 池并发、进度上报、取消、临时文件管理。
//
// 上传 = 流式加密 → 整文件 PUT → 写索引(同一事务,索引永远不指向不存在的远端对象);
// 下载 = GET → 流式解密 → .part 校验通过后原子落盘。
// 设计文档:docs/phase-4-pipeline-cli.md
package transfer
