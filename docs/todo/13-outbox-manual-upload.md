# TODO-13 — 出站箱(outbox):加密+索引先行,blob 运输手动/择机

> 状态:候选(评估完成,待排期;建议与 TODO-02 先后脚)
> 来源:2026-08 大文件在 123pan WebDAV 反复传败的痛点 + 官方确认不支持 Range 断点续传(TODO-01 因此挂起)后的替代路径推演

## 问题

- 123pan WebDAV 不支持断点续传(Range,官方文档确认):大文件上传遇网络波动即整体失败,dav 层重试只能从头重传,弱网 + 大文件可能永远传不完
- 123pan WebDAV 上传限速约 100 请求/分钟且与并发无关(TODO-11 实测),批量导入同样受制
- 网盘自家客户端走私有协议,大文件上传健壮——**运输能力其实存在,只是不在 WebDAV 通道上**
- E2E 模型下 kist 只需要网盘当哑存储:运输由谁完成不影响安全模型(搬的是密文随机名 blob,不泄任何信息)

## 方案:运输与记账分离

put 拆出「记账先行」模式,远端对象允许迟到:

- `kistctl put --defer <路径...>`(命名待定):
  - 流式加密产物持久化到 `KIST_HOME/outbox/<blob名>`(不再放 /tmp 一闪即逝)
  - 索引立即写入:`files.state='uploading'`(**v1 schema 已预留该状态,schema 零迁移**),`blobs.state='pending'`(该列为自由 TEXT)
  - ls/info 显示「待上传」;get 对 uploading 明确拒绝并提示下一步,而不是 GET 404 疑云
- `kistctl outbox` 子命令族:
  - `list`:待传对象(随机名、密文大小、对应虚拟路径、outbox 占用)
  - `push [<名>|--all]`:kist 经 WebDAV 自己 PUT(网络好时;走既有管线,重试/进度/取消照旧);结束输出汇总「成功 X / 失败 Y」并给出三条出路指引(重试 push / 手工搬运 / discard),失败对象按「push 失败政策」处置
  - `verify`:逐个 PROPFIND Depth 0 精确探测(**复用 TODO-11 的 O(1) Exists**)+ 大小核对 → 转 ready/active、删本地产物
  - `discard <名>`:回滚索引行、删产物
- **手工搬运路径**:用户用网盘官方客户端把 outbox 里的密文文件拖进远端 `/kist/`(文件名保持 32hex 随机名),再 `outbox verify` 收账
- 状态机:`uploading --(push 成功 | verify 探测到且大小符)--> ready;uploading --push 失败--> keep 档:仍 uploading(挂账)/ discard 档:回滚;uploading --discard--> 回滚`

### push 失败政策(两档)

push 重试用尽在限速网盘上是**常态而非异常**(TODO-11 实测:约 100 请求/分钟的配额下,5000 对象批量 push 有 21 个 ≈ 0.4% 重试用尽失败),失败处置不做一刀切,做成设置 `settings.outbox_push_fail`(默认 `keep`):

| 档 | 行为 | 适用 |
|---|---|---|
| `keep`(默认) | 索引行与产物**都保留**,挂账可见;push 汇总大声报失败数并给出三条出路(重试 `push` / 手工搬运 / `discard`) | 大文件、批量导入、限速常态——「加密已落袋」的价值所在 |
| `discard`(严格档) | 失败对象的索引行与产物**一起删**,干净失败;文案推荐「重新 `put --defer` 后手工搬运」 | 小文件、不容忍账面悬置的使用者 |

设计红线:**不做「删索引行、留产物」的组合**——行删掉后 verify 无行可翻、虚拟路径/大小/SHA 等元数据尽失,手动传完即成无人认领的孤儿;要救只能靠 outbox sidecar 文件重建索引,等于把 uploading 行从 SQLite 搬进 JSON,复杂度更高。**运输层的失败不回滚记账层**——两层独立失败、独立重试,是本项的设计核心。

不变量相应放宽:从「索引不指向不存在的远端对象」到「**引用携带上传状态,读者必须处理 uploading**」——错方向悬空(孤儿)由 gc 处置,反方向迟到(uploading)由 outbox 收账。

## 与既有设计的关系

- TODO-02(失败保留加密产物)是地基:产物持久化基建共用;02 很小,建议先行,13 复用
- TODO-11 的 O(1) Exists 成全 verify:万级 uploading 也廉价
- TODO-03(分块对象)仍是终极解(对象变小后 WebDAV 自扛);13 是不动远端布局、零格式变更的轻量替代
- TODO-01 挂起后,13 承接同一痛点(大文件运输可靠性)的另一条解路

## 风险与对策

| 风险 | 对策 |
|---|---|
| 手工传一半/传错 | verify 核对存在 + 大小;AEAD 块认证保证错位必在读取时暴露,不产出坏文件;blob 不可变 + 随机名,不存在半截覆盖 |
| 本地双倍磁盘 | verify/push 成功即删产物;list 显示占用 |
| gc 误判 | uploading 行在索引有登记,孤儿检测(远端有、索引无)天然不误伤;trash × pending 交叉需专测 |
| 多设备 pull 到 uploading 行 | 语义自洽:另一台看到待上传并拒绝 get;备份/revision 流程不变 |
| 用户长期不传 | 挂账可见(list);discard 回滚 |
| 重试用尽在限速网盘是常态(实测 21/5000 ≈ 0.4%) | 两档失败政策:默认 keep 保住已加密成果,严格需求走 discard 档设置 |
| outbox 里的密文产物 | 与 keyfile 同在 KIST_HOME(0700),非明文,同安全域 |

## 涉及模块

`internal/index`(零迁移,启用预留的 `files.state='uploading'` 与 `blobs.state='pending'`;ListFolder/Search/get 对 uploading 的可见性与拒绝语义)、`internal/transfer`(产物落 outbox、--defer 管线分支)、`internal/remote`(verify 复用 Exists 并需解析 getcontentlength 以核对大小)、`internal/config`(新增 `outbox_push_fail` 设置,默认 keep)、`cmd/kistctl`(outbox 子命令族);GUI 后置(文件状态徽标 + 出站箱页)

## 验收标准

- put --defer:索引 uploading 行 + 产物在 outbox + ls/info 显示待上传;get 拒绝并给出指引
- 模拟手工搬运(测试内直接 PUT 产物)后 verify:转 ready、产物删除、get 下载解密内容与源一致
- push 路径与普通 put 行为等价(重试/取消/进度/缩略图/同名消解)
- push 失败两档:keep 档下失败对象的行与产物不变、汇总含失败数与出路指引;discard 档下失败对象行与产物同删、成功对象不受影响;设置默认值为 keep
- verify 遇大小不符:拒绝转 ready 并报告
- gc:不误报 uploading 为孤儿、不误删;rm 掉 uploading 文件后与 discard/gc 的交叉自洽
- 多设备:pull 后可见 uploading 状态且 get 拒绝

## 工作量

put 分支与状态接线约 100–150 行;outbox 子命令族约 150–200 行;以 e2e 与边界测试为主。中量级,纯增量,不动加密格式与远端布局。
