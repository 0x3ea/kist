# kist 系统现状图(system-map)

> **定位**:Phase 0–7(CLI 全流程 + GUI 壳)+ TODO 07/08/11/12/13/15/16 时点的**现状快照**——回答"系统现在是什么样、动哪里会影响什么"。
> 历史沿革看 `phase-*.md` 与 git 历史;加密格式全文规范看 `PLAN.md`;网盘实测特性看 `provider-notes.md`;用户视角看 `quickstart.md`。
>
> **维护约定**:每个 TODO/阶段合入时同步本文对应小节(与 quickstart 的同步约定并列)。
> 发现本文与代码不一致:以代码为准,并立刻修正本文——失真的地图比没有地图更糟。

## 1. 一图流

```
cmd/kistctl(CLI 壳,1440 行)        main.go+app*.go(Wails GUI 壳,Phase 7:31 个绑定 + 四页面)
        │                                   │
        └────────────┬──────────────────────┘
                     ▼
          transfer.Manager        上传/下载/push 管线:单调度器 + 动态并发(1–4),
          (internal/transfer)     进度/取消/临时文件/文件夹打包/出站箱/gc
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
| transfer | 1780 | 管线全部行为:并发调度、进度、取消、临时文件、文件夹打包(TODO-15)、出站箱、gc、缩略图 | `manager.go`(调度)、`upload.go`/`download.go`(旅程)、`pack.go`(粒度/zip/解压)、`push.go`(出站箱)、`gc.go`、`thumb.go` |
| index | 1543 | SQLite 明文索引:虚拟目录、文件账本、目录元数据/tag(TODO-16)、子树聚合、blob 登记、revision、快照/替换 | `db.go`(打开/WithTx/快照替换)、`schema.go`(迁移)、`files.go`、`folders.go`、`summary.go`(聚合+封面三级链)、`blobs.go`、`outbox.go` |
| crypto | 754 | 加密核心:口令→MK 包装(keyfile)、流式分块加解密(blob)、v2 大小量化 | `blob.go`(Writer/Reader)、`keyfile.go`、`format.go`(常量与档位)、`keys.go`(HKDF) |
| dav | 542 | WebDAV 语义 + 网络可靠性:定长 PUT、流式 GET、O(1) Probe、重试退避 | `client.go`、`retry.go` |
| backup | 209 | 索引云备份与多设备恢复,LWW | `backup.go` |
| migrate | 188 | 网盘间纯密文搬运:断点续搬、双端校验 | `migrate.go` |
| remote | 151 | 远端对象语义:名字→路径、保留名、blob 增删查 | `store.go` |
| config | 112 | KIST_HOME 路径、config.json、设置归一化 | `config.go` |
| errs | 64 | AppError 错误码(CLI/GUI 共用文案映射) | `errs.go` |
| logging | 58 | slog → KIST_HOME/kist.log,启动轮转留一代 | `logging.go` |
| cmd/kistctl | 1440 | CLI 壳:18 个子命令、口令获取、参数重排、虚拟路径 | `main.go` |
| 根 main/app | ~1300 | Wails GUI 壳(Phase 7):31 个绑定方法、事件转发、防抖自动备份、退出前备份;四页面 vue-ts ~2.2k 行(手写 CSS,零新前端依赖) | `app.go`(状态/解锁/生命周期)、`app_browse.go`(浏览/元数据)、`app_transfer.go`(传输/维护)、`main.go`、`frontend/src/` |

核心代码约 6.8k 行(不含测试,另含 GUI 壳 ~1300 行),全仓 Go 约 12.1k 行。**transfer + index + crypto + dav 四个包占核心的 85%**,掌控它们即掌控项目。

### 2.5 Wails GUI 壳(Phase 7)

GUI 是纯壳:**零业务逻辑,只编排 internal/***。与 CLI 的关系是同一核心的两个薄壳入口(CLI 的 `loadStore/unlockMK/newManager` helper 在 `app.go` 有同构实现)。

**App 结构**(`app.go`):`mu` 保护 cfg/store/mk/unlocked/backupTimer/lastBackupRev;`db`/`mgr` 在 startup 一次构造视为不可变(`index.ReplaceWith` 原句柄换库,恢复后 Manager 仍有效)。MK 经 `mkSnapshot()` 闭包注入管线,未解锁返回 false → 任务以 LOCKED 失败。

**绑定分组**(31 个,全部 `defer panicGuard` 防 panic 崩窗口,错误出口统一 `wrap`):

| 分组 | 方法 | 说明 |
|---|---|---|
| 状态/解锁 | GetAppState / Get·SaveWebDAVConfig / TestConnection / CreateAccount / Unlock / ImportFromRemote / Lock / ChangePassphrase | AppState 含 HasLocalKeyfile(Lock 页三分支判定);Unlock 的 SuggestPullIndex = 本地空库 + O(1) Probe 远端 index.enc |
| 浏览 | ListFolder / SearchAll / FileInfo / GetThumbnail / SetNote / SetUserMeta / DeleteEntries / EnsureFolder / MoveFiles / Get·UpdateFolderMeta | ListFolder 绑定层合成面包屑+条目+FolderSummaries(一次往返);FileDetail 是摊平 NullInt64 的 DTO;UpdateFolderMeta 直传指针语义(nil=不动/零值=清除) |
| 传输 | PickFiles / PickDir / UploadPaths / DownloadTo / CancelTransfer / Transfers | 对话框在 Go 侧(v2.15 JS 运行时无 Open*Dialog);上传默认 pack、下载默认解压,不暴露 expand/keepZip |
| 设置/维护 | Get·SaveSettings / BackupIndexNow / PreviewGC / RunGC | GC 两步确认;孤儿只报告 |

**事件接线**:管线的 `Emit` 回调即 `onTransferEvent`——全部事件透传 `runtime.EventsEmit`,其中 `index:changed` 同时驱动壳层防抖备份(App 是转发器+消费者,不经 EventsOn 自我订阅)。startup 事件先于前端订阅即丢失 → 前端 `store.init()` 主动拉初值。

**防抖自动备份**(壳层机制,phase-7 约定):`index:changed` → 重置 30s `time.AfterFunc`(仅 AutoBackup 开且已解锁)→ `BackupNow`(Background+60s 超时,忙则重排不丢变更)→ notify。**退出前备份不受 AutoBackup 限制**:shutdown 时已解锁且 `db.Revision() > lastBackupRev`(会话内追踪,基线=解锁时;`sync_state.last_backup_at` 只存时间不存 revision)则补一次,失败仅记日志。

**错误码传递**:Wails 把绑定 error 序列化为纯字符串 reject promise,而 `AppError.Error()` 只有 Msg——壳层 `codedError` 统一包 `"[CODE] Msg"`,前端 `errors.ts` 正则反解映射中文文案。`internal/errs` 不动(CLI 输出不受影响)。

**SaveWebDAVConfig 后热更新**:重建 store 后调 `mgr.SetRemote()`(Phase 7 给 Manager 补的方法,worker 持 `remoteSnapshot()` 快照防 data race)——向导首配时 mgr 的 Remote 还是 nil,也靠这里补上。调用在 `a.mu` 外:worker 的 Emit 回调反向拿 a.mu,锁内互嵌会 ABBA。

## 3. put 的字节旅程(上传端到端)

以 `kistctl put 大文件 --dest /备份` 为例,一个明文字节从磁盘到网盘的全路径:

1. **口令→MK**(`main.go:unlockMK`):本地 `KIST_HOME/keyfile` 优先;没有则从远端拉一份并缓存 0600(这就是新设备/沙盒共用网盘时的路径)。Argon2id 派生 KEK 解包 MK;AEAD tag 失败 = 口令错,天然校验。
2. **入队**(`manager.go:UploadPaths`):文件直接入队;文件夹默认按**打包粒度**解析(`pack.go:planPackTree`,TODO-15)——唯一判据"有无子目录":根为叶子则整根一个 pack 直挂 dest,否则逐层下降、每个叶子目录一个 pack,混杂层散文件独立入库,空目录保留为虚拟目录;一次事务建全部虚拟目录后入队。**校验遍一次收全问题再整次 put 拒绝**:非 UTF-8 文件名(提示 convmv 转码)、symlink/FIFO 等特殊文件。`--expand` 走旧的逐文件展开(两遍 WalkDir,先在一个事务里逐级建目录——**每级 id 都要记**,只记最深一级会让根下文件拿零值 folderID;符号链接静默跳过防环)。
3. **调度**:单调度器 goroutine,每轮重读并发上限(默认 2,夹取 1–4,改设置即时生效),`cond` 唤醒派发。
4. **流式加密**(`upload.go:runUpload` → `crypto.BlobWriter`):明文按 1MiB 读入,攒满 4MiB(默认块,`chunk_mib` 可调)封一个 XChaCha20-Poly1305 块。**关键规则:整块只有在"缓冲满且还有后续数据"时才封出**,否则留给 Close 以 final 标志封出——块数恒为 `max(1, ceil(size/chunk))`,空文件也是 1 个零长度末块。进度先按明文计,Close 后总数改为 明文+密文。临时文件在 `/tmp/kist/<传输ID>/blob`,全程内存 ≈ 一个块。
   - **v2 量化**(默认开,TODO-08):Close 前补零到档位——≤1MiB 归 4KiB 倍数(空文件 4KiB 档),大文件按 10% 阶梯。补零走与真实数据完全相同的分块/认证路径,但**绝不计入 OrigSize 与 SHA-256**。设 `"size_padding":"off"` 则写 v1(精确大小)。
   - **pack 任务**(TODO-15):源是目录——`zip.NewWriter` 直挂 BlobWriter 单遍流式(条目全 Store 不再压缩,空目录写显式条目,条目 mtime 入 zip),词法序第一张图记为封面候选。索引行 `pack=1`,size 记 `bw.PlainTotal()`(v2 即档位值,**显示口径**——真实 origSize 在 sealedMeta,不得用索引 size 推明文长度),原目录规模(orig_size/entries)写 user_meta。
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
6. 全部通过 → 落盘:普通文件 rename 原子落盘(目标已存在则 "(1)" 递增);**pack 条目**(TODO-15)默认解压到 `.<名>.kistdirpart` 再**整体 rename** 还原成文件夹——解压断言:条目名合法 UTF-8 且不逃逸目标目录(zip-slip 同套防御),条目 mtime 尽力还原;`--keep-zip` 则改落 `<名>.zip`(喂阅读器可改名 .cbz)。

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

**files.pack**(TODO-15,schema v2):目录打包条目标记——明文区是一个标准 zip(Store),get 还原成文件夹;其 size 为量化后显示值。

**目录元数据**(TODO-16,schema v3):`folders.note/user_meta/cover_file_id` 与 `tags`/`folder_tags` 两表。tag 走独立表(过滤是 tag 的全部意义);`cover_file_id` 指向普通文件,复用全部文件管线,不发明封面 blob 类别。**元数据三不原则**:不继承、不合并、无告警——聚合面(`FolderSummary`)只聚合计数,永不聚合元数据。`FolderSummary` 纯查询零维护:全量内存建树(不用递归 CTE)后序聚合 PackCount/FileCount/TotalSize/LatestAt/PendingCount,封面三级回退链同一次遍历解析(自定义单图 → 子条目名称序前四的 2×2 宫格、空位记 0 不跳过 → 空作品交渲染端)。

**文件元数据**(TODO-17,schema v4):`file_tags` 镜像 folder_tags、共享 `tags` 词表——两种作品形态同一词典,死词清理必须 UNION 双表(任一侧清空不得误删另一形态仍在用的同名词);文件封面不是引用而是**自身的 thumbnails 行**,GUI「导入封面」走上传同款 `MakeThumbnail` 管线直写(覆盖语义:pack 的自动首页缩略图被顶掉后清除不恢复;坏图报错不静默,与上传侧的"失败即跳过"相反);`Search` 文件侧补 tag 命中(EXISTS 子查询),`FileHit` 回填 Tags。

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
15. pack 条目(TODO-15)明文区恰为一个标准 zip;上传校验遍**整次拒绝**坏树(非 UTF-8 名/特殊文件)不留半套索引;解压侧断言(UTF-8/不逃逸)+ 临时目录整体 rename,失败不落半个目录。
16. 目录元数据(TODO-16)**不继承、不合并、无告警**:tag/作者/note 是目录自身属性,聚合面永不聚合元数据;写在哪个目录就属于哪个目录,kist 不做解释(哑远端约束同款)。
17. mv 是纯索引操作:改挂点+重名消解单事务,**远端对象零变化**;修改 modified_at 不属于移动(最近更新保持内容语义)。

## 9. 失败模式与恢复路径

| 场景 | 系统行为 | 恢复 |
|---|---|---|
| 加密/解密中途取消 | 每 MiB 检查 ctx,临时文件全清 | 重来,无残留 |
| PUT 反复失败 | dav 层重试 5 次(1s→30s ±20% 抖动,尊重 Retry-After 秒数形式;只重试网络错/5xx/429),整体重传无断点 | 任务失败;弱网改用 `put --defer` + 择机 push/手工搬运 |
| PUT 成功但写索引失败 | 远端留下孤儿 blob,Warn 留痕 | `gc --dry-run` 确认后 `gc` 清理 |
| 下载校验失败 | .part 删除,报 CORRUPT/WRONG_KEY,**不落盘** | WRONG_KEY 先查 config 是否指向别的账户网盘 |
| pack 解压断言失败/中途取消 | .kistdirpart 与 zip 临时文件整体删除,不落半个目录 | 重试 get |
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

## 11. CLI 命令速查(18 个子命令)

| 命令 | 语义 | 需要口令 | 关键路径 |
|---|---|---|---|
| config set | 存 WebDAV 配置 + Ping | 网盘密码 | `cmdConfig` |
| init | 建 keyfile + 远端初始化(远端已有则拒绝) | ✓ | `cmdInit` |
| unlock | 校验口令 | ✓ | `cmdUnlock` |
| put [--dest] [--defer] [--expand] | 加密上传(文件夹默认打包)/ 入出站箱 / 逐文件展开 | ✓ | `Manager.UploadPaths/DeferPaths` |
| outbox list/push/verify/discard | 出站箱搬运与收账 | ✗(纯密文) | `push.go` |
| ls / search / info / note | 索引浏览与备注(ls 目录行附子树摘要;search 兼查目录名/tag) | ✗ | `index` 各查询 |
| meta set/list | 目录/文件元数据:tag/note/封面引用(TODO-16/17;文件目标用 uuid\|id,--cover 仅目录,文件封面导入走 GUI) | ✗ | `UpdateFolderMeta`/`UpdateFileMeta` |
| mv | 纯索引移动文件(改挂点,零远端流量;重名自动消解) | ✗ | `MoveFiles` |
| mkdir | 建虚拟目录(多级、幂等 mkdir -p 语义;纯索引零流量) | ✗ | `EnsureFolderPath` |
| rename | 重命名目录(同名幂等 no-op;撞名报错不消解;根/软删/非法段拒绝;纯索引零流量) | ✗ | `RenameFolder` |
| get [--keep-zip] | 下载解密;pack 还原成目录(或落 zip) | ✓ | `Manager.DownloadTo` |
| rm | 软删 + blob 标 trash | ✗ | `SoftDeleteFiles`+`MarkBlobTrash` |
| gc [--dry-run] | 清 trash、报孤儿 | ✗ | `RunGC` |
| backup / pull | 索引云备份 / 恢复 | ✓ | `backup` |
| migrate [--switch] | 网盘间迁移 | ✗(不解锁) | `migrate.Run` |

注意:两个密码别混——WebDAV 账户密码 vs kist 加密口令(`KIST_PASS` 或 `--pass-stdin`)。`rm` 目前只删文件;目录软删 API(`SoftDeleteFolders`)已有、CLI 无入口(GUI 用)。

## 12. 测试地图(103 项,验收必跑 `go test ./... -race`)

| 包 | 数量 | 覆盖要点 |
|---|---|---|
| 根(GUI 壳) | 9 | headless 绑定层:codedError 格式、ListFolder 合成(null 归一/摘要批量)、SearchAll 三路命中、路径拆分(`..` 拒绝)、EnsureFolder 幂等+移动落点、删除(软删+trash/目录隐藏/空选拒绝)、元数据指针语义、缩略图 NOT_FOUND 与详情投影;顺带逼出 cover 清除的 FK 缺陷;TODO-17 文件元数据+封面绑定(往返/tag 命中/导入/清除/非图片 BAD_CONFIG/已删拒绝) |
| crypto | 16 | keyfile 往返/错口令/参数篡改/改口令;blob 往返/篡改/截断/重排/追加/final 冒充/错密钥/v2 填充完整性/流式内存峰值 |
| dav | 13 | MKCOL 幂等、PUT 定长、503/429 退避、4xx 不重试、ctx 取消、Probe 精确请求与网络错误区分 |
| index | 26 | 目录树、软删可见性、搜索、revision 并发单调、快照/替换往返、重名消解、pack 列往返;TODO-16:元数据往返/死 tag 清理、目录检索三路命中+软删祖先、子树聚合、封面三级回退(悬空/混合序/截断/留白)、纯索引移动+回滚、元数据经快照存活;TODO-17:文件元数据往返、双形态共享词表的双向死词清理、文件 tag 命中+Tags 回填、v3→v4 升级、文件元数据+手动封面经快照存活 |
| backup | 6 | 双设备往返、错密钥、无备份、损坏备份、uploading 状态随备份同步 |
| transfer | 10 | 临时目录清理留痕;打包粒度五形态、校验整次拒绝、zip 往返(空文件/空目录/unicode/mtime)、取消、zip-slip、非 UTF-8 条目 |
| logging | 2 | 轮转阈值 |
| e2e | 21 | 全生命周期(--expand 逐文件路径)、重名、取消、错密钥、孤儿留痕;出站箱 6 例(verify 大小不符/push 两档政策/discard);迁移 4 例(断点续跑/无 keyfile/无 index.enc);pack 2 例(多话对象数=叶数/还原 SHA/keep-zip、单话直挂);TODO-16 两例(真实缩略图管线下的 meta/mv/摘要/备份恢复/封面回退) |

e2e 起本地内存 WebDAV(x/net/webdav)跑真实 Manager——**transfer 的行为正确性实际由这 19 项 e2e 担保**,改管线后必跑 `go test ./internal/e2e/ -timeout 600s`。
