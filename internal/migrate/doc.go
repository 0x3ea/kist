// Package migrate 实现网盘间迁移:纯密文字节搬运——不解密、不重加密、
// 索引不动(blob 随机名原样保留),迁移后只改 config.json 指向。
//
// 断点语义:目标已存在且大小相同的对象直接跳过,中断重跑无损,可跨天搬运;
// 校验哲学:对象粒度 size 核对 + keyfile/index.enc 双端流式哈希比对,
// 不做全量回读——blob 的 AEAD 认证保证搬坏必在读取时暴露,不会产出坏文件。
// 全程不解锁、不触碰明文,可在任意机器(含 VPS)运行。
// 评估文档见 git 历史 docs/todo/12-migrate-command.md(完成后移出)。
package migrate
