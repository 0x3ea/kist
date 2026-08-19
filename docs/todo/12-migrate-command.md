# TODO-12 — migrate 命令:网盘间迁移

> 状态:候选(小,约 150–200 行)
> 来源:2026-08 网盘到期/搬家场景讨论

## 背景与关键认知

换网盘(到期、限流、搬家)需要把数据从旧端整体搬到新端。**加密与网盘解耦**:fileKey 派生与服务器无关,迁移是纯密文字节搬运——不解密、不重加密、索引不动(blob 随机名原样保留),只改 `config.json` 指向。

零代码替代:两个 WebDAV remote 之间 `rclone copy`(流式过机、不落盘)+ `rclone check`,再手改 config。E2E 加密下第三方搬家工具(MultCloud 等)只见密文,在威胁模型内可接受。

## 原生 migrate 的增值

- **覆盖全部对象**:keyfile、index.enc、所有 blob——手工/rclone 最容易漏前两者
- **对象粒度断点**:目标已存在且大小相同 → 跳过;中断重跑无损,可跨天搬运(到期前慢慢搬)
- **不落盘**:GET 流 → tee 哈希 → PUT(Content-Length 取自源端 size,兼容保守网盘,不用 chunked)
- **校验哲学**:blob 的 AEAD 认证保证「搬坏必在读取时暴露,不会产出坏文件」——size 校验 + 随机 N 个 blob 抽查解密 + index.enc 完整校验即可,不做全量回读
- **可放 VPS 跑**:搬运密文不需要解锁口令,本机带宽成本归零

## 接口草案

```
kistctl migrate --url <新URL> --user <u> [--pass-stdin] [--root /kist] [--switch]
```

流程:枚举旧端全部对象 → 逐对象搬运(跳过已完成)→ 抽查验证 → 提示更新 config(`--switch` 验证通过后直接切换写回)。

## 涉及模块

`internal/dav`(双 client 并存)、`internal/remote`(对象枚举)、`cmd/kistctl`;GUI 入口(Settings「迁移」)可后置。

## 验收标准

- e2e 风格双端 httptest:迁移后对新端解锁、浏览、下载全部可用,内容 sha256 与源一致
- 中断重跑只补缺失对象,已完成的不再传输
- keyfile/index.enc 缺失会被显式校验发现(而非静默报告成功)
- 全程不触碰明文:命令无口令参数,不解锁

## 边界

两个互不相识的网盘之间,字节总要经过某处(本机 / VPS / 第三方搬运工);WebDAV `COPY` 仅同一服务器内生效,跨服务商没有通用服务端复制——不存在零流量通用解,只有「流量记在谁账上」的选择。
