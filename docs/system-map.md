# kist 系统现状图(system-map)

> **定位**:CLI 全流程完成时点(Phase 0–5 + TODO 07/08/11/12/13)的**现状快照**——回答"系统现在是什么样、动哪里会影响什么"。
> 历史沿革看 `phase-*.md` 与 git 历史;加密格式全文规范看 `PLAN.md`;网盘实测特性看 `provider-notes.md`;用户视角看 `quickstart.md`。
>
> **维护约定**:每个 TODO/阶段合入时同步本文对应小节(与 quickstart 的同步约定并列)。
> 发现本文与代码不一致:以代码为准,并立刻修正本文——失真的地图比没有地图更糟。

## 1. 一图流

```
cmd/kistctl(CLI 壳,1134 行)        未来 Wails app.go(GUI 壳)
        │                                   │
        └────────────┬──────────────────────┘
                     ▼
          transfer.Manager        上传/下载/push 管线:单调度器 + 动态并发(1–4),
          (internal/transfer)     进度/取消/临时文件/出站箱/gc
             │        │
             │        └────────► internal/backup   索引云备份/恢复(LWW)
             │                    internal/migrate  网盘间纯密文迁移
             ▼
   ┌─────────────────┬──────────────────┬────────────────────┐
   │ internal/crypto  │ internal/index   │ internal/remote     │
   │ keyfile+blob 加密 │ SQLite 明文索引  │ 远端对象语义        │
   │ (无其他依赖)      │ (唯一目录真源)   │ (blob/keyfile/      │
   │                  │                  │  index.enc)         │
   └─────────────────┴────────┬─────────┴────────────────────┘
                              ▼
                   internal/dav    WebDAV 客户端(自管 PUT/GET/PROPFIND
                                    + 指数退避重试;PROPFIND/MKCOL/DELETE/MOVE 走 gowebdav)
                              ▼
                       网盘 /kist/  (扁平:随机名 blob + keyfile + index.enc)

  辅助:internal/config(KIST_HOME 与设置)、internal/errs(错误码)、internal/logging(kist.log)
```

依赖方向单向:crypto 最底层不依赖任何人;CLI/GUI 只是壳。**改动的爆炸半径**大致等于在图上跨了几层。

## 2. 模块地图

| 包 | 行数 | 职责 | 关键文件/入口 |
|---|---|---|---|
| transfer | 1212 | 管线全部行为:并发调度、进度、取消、临时文件、出站箱、gc、缩略图 | `manager.go`(调度)、`upload.go`/`download.go`(旅程)、`push.go`(出站箱)、`gc.go`、`thumb.go` |
| index | 935 | SQLite 明文索引:虚拟目录、文件账本、blob 登记、revision、快照/替换 | `db.go`(打开/WithTx/快照替换)、`schema.go`(迁移)、`files.go`、`folders.go`、`blobs.go`、`outbox.go` |
| crypto | 754 | 加密核心:口令→MK 包装(keyfile)、流式分块加解密(blob)、v2 大小量化 | `blob.go`(Writer/Reader)、`keyfile.go`、`format.go`(常量与档位)、`keys.go`(HKDF) |
| dav | 542 | WebDAV 语义 + 网络可靠性:定长 PUT、流式 GET、O(1) Probe、重试退避 | `client.go`、`retry.go` |
| backup | 209 | 索引云备份与多设备恢复,LWW | `backup.go` |
| migrate | 188 | 网盘间纯密文搬运:断点续搬、双端校验 | `migrate.go` |
| remote | 151 | 远端对象语义:名字→路径、保留名、blob 增删查 | `store.go` |
| config | 112 | KIST_HOME 路径、config.json、设置归一化 | `config.go` |
| errs | 64 | AppError 错误码(CLI/GUI 共用文案映射) | `errs.go` |
| logging | 58 | slog → KIST_HOME/kist.log,启动轮转留一代 | `logging.go` |
| cmd/kistctl | 1134 | CLI 壳:16 个子命令、口令获取、参数重排、虚拟路径 | `main.go` |

核心代码约 5.4k 行(不含测试),全仓 Go 约 8.3k 行。**transfer + index + crypto + dav 四个包占核心的 85%**,掌控它们即掌控项目。

## 3. put 的字节旅程(上传端到端)

以 `kistctl put 大文件 --dest /备份` 为例,一个明文字节从磁盘到网盘的全路径:

1. **口令→MK**(`main.go:unlockMK`):本地 `KIST_HOME/keyfile` 优先;没有则从远端拉一份并缓存 0600(这就是新设备/沙盒共用网盘时的路径)。Argon2id 派生 KEK 解包 MK;AEAD tag 失败 = 口令错,天然校验。
2. **入队**(`manager.go:UploadPaths`):文件直接入队;文件夹两遍 WalkDir——先在一个事务里逐级建目录(**每级 id 都要记**,只记最深一级会让根下文件拿零值 folderID),再逐文件入队。符号链接跳过防环。
3. **调度**:单调度器 goroutine,每轮重读并发上限(默认 2,夹取 1–4,改设置即时生效),`cond` 唤醒派发。
4. **流式加密**(`upload.go:runUpload` → `crypto.BlobWriter`):明文按 1MiB 读入,攒满 4MiB(默认块,`chunk_mib` 可调)封一个 XChaCha20-Poly1305 块。**关键规则:整块只有在"缓冲满且还有后续数据"时才封出**,否则留给 Close 以 final 标志封出——块数恒为 `max(1, ceil(size/chunk))`,空文件也是 1 个零长度末块。进度先按明文计,Close 后总数改为 明文+密文。临时文件在 `/tmp/kist/<传输ID>/blob`,全程内存 ≈ 一个块。
   - **v2 量化**(默认开,TODO-08):Close 前补零到档位——≤1MiB 归 4KiB 倍数(空文件 4KiB 档),大文件按 10% 阶梯。补零走与真实数据完全相同的分块/认证路径,但**绝不计入 OrigSize 与 SHA-256**。设 `"size_padding":"off"` 则写 v1(精确大小)。
   - 头部 156B:40B 明文头 + 116B sealedMeta(加密的 fileID/origSize/chunkSize/noncePrefix/明文SHA/mtime/revision/deviceID)。Writer 先占位,Close 时 Seek 回填——源文件单遍读取。
5. **缩略图**(`thumb.go`,仅图片):嗅探 512B 判型(jpeg/png/gif/bmp/webp),最长边 512px,JPEG q80 降级 / 透明转 PNG,≤128KB。**任何失败只 Warn 不阻断**;非图片静默跳过。
6. **blob 名** = 16 随机字节 hex(32 字符),与文件名完全无关 → 并发上传永不撞名,重名消解推迟到索引事务内(`UniqueFileName` 追加 "(1)")。
7. **分岔**:
   - **直接上传**:整文件 PUT(`dav.PutFile`:显式 Content-Length 防保守网盘拒收 chunked;`SectionReader+NoCloser` 包装,因为 `http.Transport` 会 Close 裸传的 `*os.File`,重试必失败)。成功后**同一事务**写:files 行 + 缩略图 + blobs 登记 + revision+1。
   - **`--defer`(TODO-13)**:不发 PUT。同一事务写 files(state=`uploading`,无 uploaded_at)+ blobs(state=`pending`),产物挪入 `KIST_HOME/outbox/<blob名>`(/tmp 常为 tmpfs,跨设备 rename 失败回退为复制)。
8. **收尾**:临时目录删除;`index:changed` 事件(CLI 打印一行阶段变化,GUI 转 EventsEmit)。

**取消点**:加密循环每 MiB、每次 HTTP 请求(dav 层 ctx)、退避睡眠前后、索引写入前。取消/失败不留任何临时残留。

## 4. get 的字节旅程(下载端到端)

以 `kistctl get 5 --to ~/Downloads` 为例:

1. 解析目标(uuid 或数字 id);若 `state=uploading` **直接拒绝**并提示先 push/verify(必然 404,提前给出有指引的错误)。
2. GET 整个 blob 到 `/tmp/kist/<传输ID>/blob`(重试 = 整体重传,prog 可能回退后再增长)。
3. `crypto.NewBlobReader` **打开即校验**:magic/版本 → sealedMeta 解密(**失败 = ErrWrongKey**,典型场景:config 指向了另一个账户的网盘)→ 长度总校验 `156 + 16n + 明文区总长`(v2 按 bucketSize 算),不符即拒——截断/追加在此拦截。
4. 流式解密到目标目录 `.<名字>.kistpart`(0600)。逐块 Open(块 AAD 绑定序号+final,重排/移花接木在此暴露);v2 只交付前 OrigSize 字节,纯补零块认证后整块丢弃。
5. **EOF 即终检**:底层必须恰好耗尽(末块后无剩余)+ 流式 SHA-256 与头部声明一致。任一不过 → 删除 .part,报 CORRUPT,**绝不落盘**。
6. 全部通过 → rename 原子落盘(目标已存在则 "(1)" 递增)。

## 5. 加密格式速查(全文规范见 PLAN.md)

| 对象 | 形态 |
|---|---|
| keyfile(110B) | `KISTKEY1` + Argon2 参数(明文,默认 t=1/64MiB/4线程,**解析期设上界防资源耗尽**)+ wrapNonce + wrappedMK=XChaCha20-Poly1305(KEK, MK)。MK 随机 32B,明文永不落盘;改口令只重写这 110 字节 |
| blob 头(156B) | 40B 明文头(magic/版本/metaLen/metaNonce)+ 116B sealedMeta(全部元数据加密,v1/v2 头布局相同) |
| 密钥派生 | `fileKey = HKDF-SHA256(MK, salt=fileID)` 每文件独立;`metaKey = HKDF-SHA256(MK, salt="kist/v1")` 全局一份 |
| 块加密 | XChaCha20-Poly1305,nonce=`noncePrefix‖u64be(i)`,AAD=`magic‖fileID‖u64be(i)‖final` |
| v1/v2 | 版本只决定明文区长度语义:v1=origSize;v2=bucketSize(origSize),读侧两版兼容 |
| 档位函数 | `bucketSize`:≤1MiB 向上取整到 4KiB 倍数;>1MiB 以 orig/10 为步长向上取整。**纯整数运算,一经发布即是格式的一部分,不可改动**(否则既存 v2 对象过不了长度校验) |
| index.enc | 同一 blob 格式,Meta 的 mtime/revision/deviceID 三个字段启用(普通文件恒 0),同样默认 v2 量化 |

## 6. 布局

**网盘 `/kist/`(永远扁平)**:`keyfile`、`index.enc`、其余全是 32hex 随机名 blob。没有任何目录结构、文件名、可读元数据;v2 下连密文长度都只到档位。

**本地 `KIST_HOME`**(Linux 默认 `~/.config/kist`,`KIST_HOME` 可覆盖):

| 路径 | 内容 |
|---|---|
| config.json | WebDAV 配置 + settings(0600;密码仅显式 opt-in 落盘,那是网盘密码不是口令) |
| keyfile | 110B 主密钥包装(与远端同一份字节) |
| index.db | 明文索引(+WAL/SHM) |
| outbox/ | 出站箱:待上传加密产物,**有意持久**,push/verify 后清 |
| backups/ | 被替换/归档的旧索引(`index-<rev>-<ts>.db`) |
| kist.log | 运行日志(启动时超 5MiB 轮转留一代 .old) |

## 7. 状态机与账本

**files.state**:`uploading`(--defer 记账,不可 get)→ `ready`(PUT 完成/push/verify 收账)。`missing` **预留未启用**(尚无生产调用方,预留给"远端对象被外力删除"的检测)。

**blobs.state**(gc 账本,只登记文件 blob;keyfile/index.enc 不入账):`active` → `trash`(rm 时标记)→ 物理删除(gc 确认远端存在后);`pending` 为出站箱挂账。`orphan` 不是落库状态,是 gc 的报告概念(远端有、索引无)。

**Transfer.Phase**(一次传输的生命周期):`queued → encrypting → uploading → done`;下载 `queued → downloading → decrypting → done`;出站箱分支终态 `deferred`;异常 `error | canceled`。

**sync_state**:`revision`(单调,LWW 判据)、`device_id`(8B hex,生成后永不变)、`last_backup_at`(**有意不进 revision**——否则备份本身会推高版本造成循环)。

## 8. 核心不变量(改代码前默诵)

**安全**

1. 网盘上除密文总长(及 v2 档位)外**零元数据**:文件名/大小/摘要/mtime 全在 sealedMeta 内。
2. MK 明文永不落盘;keyfile 是唯一凭证,**口令丢失 = 数据不可恢复,没有后门**(这是特性不是缺陷)。
3. 每文件独立 fileKey + noncePrefix,构造上杜绝"同密钥同 nonce"跨文件/跨块复用。
4. 块 AAD 绑定 magic+fileID+序号+final:防重排、跨文件拼接、截断后拿中间块冒充末块。
5. v2 补零并入块加密受认证保护,但**绝不进 OrigSize/SHA**;交付只取前 OrigSize 字节。
6. 下载"校验不过不落盘"(.part + 原子 rename);打开即长度总校验,EOF 即完整性终检。

**一致性**

7. 索引是目录结构唯一真源;远端布局永远扁平。
8. files + blobs + revision+1 **同一事务**:索引有记录 ⟺ blobs 有登记;PUT 成功而索引失败只可能留下孤儿 blob(gc 报告,**不自动删**);反向不成立——"索引有而远端无"只能以 uploading/missing 状态显式表达,不会静默。
9. revision 只经 `WithTx` 单调 +1,是 LWW 唯一判据,不信任系统时钟。
10. 软删是默认删除语义;`DiscardPending` 只能动 uploading 行,**ready 文件必须走 rm**,不能无声消失。
11. pull 替换本地库前必须完整解密验证(块认证+SHA 全过);被替换的库必归档 backups/。
12. push 与 migrate 是**纯密文搬运**:不解锁、不触碰明文,不需要口令。
13. 迁移 `Failed>0` 时禁止 `--switch`。
14. 远端对象名与文件名无关:并发上传不撞名,重名消解只在索引事务内发生。

## 9. 失败模式与恢复路径

| 场景 | 系统行为 | 恢复 |
|---|---|---|
| 加密/解密中途取消 | 每 MiB 检查 ctx,临时文件全清 | 重来,无残留 |
| PUT 反复失败 | dav 层重试 5 次(1s→30s ±20% 抖动,尊重 Retry-After 秒数形式;只重试网络错/5xx/429),整体重传无断点 | 任务失败;弱网改用 `put --defer` + 择机 push/手工搬运 |
| PUT 成功但写索引失败 | 远端留下孤儿 blob,Warn 留痕 | `gc --dry-run` 确认后 `gc` 清理 |
| 下载校验失败 | .part 删除,报 CORRUPT/WRONG_KEY,**不落盘** | WRONG_KEY 先查 config 是否指向别的账户网盘 |
| outbox push 失败 | keep 档(默认):挂账留证,可重试/手工搬运;discard 档:整笔回滚(索引行+产物) | keep → 重试 push 或手工搬运后 verify;discard → 重新 `put --defer` |
| verify 大小不符 | 拒绝收账(疑似手工传输不完整) | 清掉远端对象重传 |
| verify 时服务器不报大小(size=-1) | 仅按存在收账,结果里注明 | — |
| pull 拿到坏备份 | 解密中止,本地不动 | 无损;检查网盘上的 index.enc |
| rm 后 gc,网盘删除有秒级一致性延迟(123pan 实测) | 该对象暂时仍"存在",跳过 | 再跑一次 gc |
| 远端 blob 被外力删除 | get 时 404/CORRUPT | `missing` 状态预留,尚无自动检测 |

## 10. 辅助流程速览

**backup / pull(LWW)**:backup = 读 revision/device → VACUUM INTO 一致性快照 → blob 加密(Meta 启用 mtime/revision/deviceID)→ PUT index.enc。pull = GET → 只解头部比 revision:远端新才完整解密并 ReplaceWith(旧库归档);相等 noop;本地新不动。**明确不做行级合并**:单用户、同时单写者,落后方归档保留。多设备铁律:换设备前旧端 backup、新端先 pull。

**outbox(TODO-13)**:把"加密+记账"与"运输"分离,应对不支持断点续传的网盘。`--defer` 落账 → `push`(kist 自己重传)/ `verify`(用户用网盘官方客户端手工搬运密文文件后,O(1) Probe 核对大小收账,顺带清无主产物)/ `discard`(整笔回滚)。手工搬运要点:**保持产物原文件名**传到远端 `/kist/`。

**migrate(TODO-12)**:网盘间纯密文字节搬运。枚举源端(要求有 keyfile)→ 每对象 Probe 目标:已存在且同大小即跳过(断点续跑)→ 否则 `GetBody→PutStream` 定长流式搬运,重试在整对象粒度。结束对 keyfile 与 index.enc 双端 SHA-256 比对(blob 不回读——AEAD 保证搬坏必在读取时暴露,这是校验哲学)。`--switch` 验证通过后切换 config,旧网盘密码不带去新端。

**gc**:blobs 表 trash 且远端确认存在 → 删;远端有、索引无 → 孤儿,**只报告不删**(可能是另一设备索引回退所致,删除权在用户)。

## 11. CLI 命令速查(16 个子命令)

| 命令 | 语义 | 需要口令 | 关键路径 |
|---|---|---|---|
| config set | 存 WebDAV 配置 + Ping | 网盘密码 | `cmdConfig` |
| init | 建 keyfile + 远端初始化(远端已有则拒绝) | ✓ | `cmdInit` |
| unlock | 校验口令 | ✓ | `cmdUnlock` |
| put [--dest] [--defer] | 加密上传 / 入出站箱 | ✓ | `Manager.UploadPaths/DeferPaths` |
| outbox list/push/verify/discard | 出站箱搬运与收账 | ✗(纯密文) | `push.go` |
| ls / search / info / note | 索引浏览与备注 | ✗ | `index` 各查询 |
| get | 下载解密 | ✓ | `Manager.DownloadTo` |
| rm | 软删 + blob 标 trash | ✗ | `SoftDeleteFiles`+`MarkBlobTrash` |
| gc [--dry-run] | 清 trash、报孤儿 | ✗ | `RunGC` |
| backup / pull | 索引云备份 / 恢复 | ✓ | `backup` |
| migrate [--switch] | 网盘间迁移 | ✗(不解锁) | `migrate.Run` |

注意:两个密码别混——WebDAV 账户密码 vs kist 加密口令(`KIST_PASS` 或 `--pass-stdin`)。`rm` 目前只删文件;目录软删 API(`SoftDeleteFolders`)已有、CLI 无入口(GUI 用)。

## 12. 测试地图(64 项,验收必跑 `go test ./... -race`)

| 包 | 数量 | 覆盖要点 |
|---|---|---|
| crypto | 16 | keyfile 往返/错口令/参数篡改/改口令;blob 往返/篡改/截断/重排/追加/final 冒充/错密钥/v2 填充完整性/流式内存峰值 |
| dav | 10 | MKCOL 幂等、PUT 定长、503/429 退避、4xx 不重试、ctx 取消、Probe 精确请求与网络错误区分 |
| index | 13 | 目录树、软删可见性、搜索、revision 并发单调、快照/替换往返、重名消解 |
| backup | 6 | 双设备往返、错密钥、无备份、损坏备份、uploading 状态随备份同步 |
| transfer | 1 | 临时目录清理失败留痕(**单测薄,管线行为由 e2e 兜底**) |
| logging | 2 | 轮转阈值 |
| e2e | 16 | 全生命周期(put→ls/search→get 比对 sha→rm→gc)、重名、取消、错密钥、孤儿留痕;出站箱 6 例(verify 大小不符/push 两档政策/discard);迁移 4 例(断点续跑/无 keyfile/无 index.enc) |

e2e 起本地内存 WebDAV(x/net/webdav)跑真实 Manager——**transfer 的行为正确性实际由这 16 项 e2e 担保**,改管线后必跑 `go test ./internal/e2e/ -timeout 600s`。
