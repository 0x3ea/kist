# kist 分阶段实施文档

总体设计(架构、加密格式、目录树)见根目录 [`PLAN.md`](../PLAN.md)。本目录把实施过程拆成 9 个阶段,每阶段一份文件,统一包含:

- **要做什么**:任务清单(checkbox,可勾选跟踪进度)
- **怎么设计**:接口签名、数据格式、算法与实现要点
- **预期结果**:该阶段结束时仓库里多了什么
- **验收标准**:必须全部通过才能进入下一阶段

系统**当前形态**的速览(模块地图、put/get 字节旅程、不变量清单、失败模式)见 [`system-map.md`](system-map.md)——phase 文档回答"怎么建起来的",它回答"现在是什么样";每个 TODO 合入时同步更新它。

v0.1 后的候选增强每项一篇独立评估文档,放在 [`todo/`](todo/),总览与排期见 [`todo/README.md`](todo/README.md)。

## 阶段总览

**环境策略:前期只配纯 Go 环境;CLI 全流程跑通(Phase 5 结束)之后才安装 Wails/GTK 等 GUI 依赖(Phase 6)。**

| 阶段 | 文件 | 内容 | 依赖 |
|---|---|---|---|
| 0 | [phase-0-environment.md](phase-0-environment.md) | Go 基础环境与项目骨架 | — |
| 1 | [phase-1-crypto.md](phase-1-crypto.md) | 加密核心(密钥体系 + 流式分块加解密) | 0 |
| 2 | [phase-2-webdav.md](phase-2-webdav.md) | WebDAV 客户端层 + 本地测试服务 | 1* |
| 3 | [phase-3-index.md](phase-3-index.md) | SQLite 明文索引 | 1* |
| 4 | [phase-4-pipeline-cli.md](phase-4-pipeline-cli.md) | remote/transfer 管线 + CLI 端到端打通 | 2, 3 |
| 5 | [phase-5-backup-sync.md](phase-5-backup-sync.md) | 索引云备份与多设备恢复(CLI)——**CLI 全流程里程碑** | 4 |
| 6 | [phase-6-wails-environment.md](phase-6-wails-environment.md) | Wails/GUI 环境准备与脚手架合入 | 5 |
| 7 | [phase-7-gui.md](phase-7-gui.md) | Wails GUI(绑定、事件、四页面、备份 UI) | 6 |
| 8 | [phase-8-polish.md](phase-8-polish.md) | 孤儿清理、打包、文档、交叉编译出 Win 版 | 7 |

\* 阶段 2 与阶段 3 互相独立,可以并行推进。

## 全局约定

- Go module 名:`kist`;Go ≥ 1.24(开发机 1.26.6)
- 所有 wails 命令带 `-tags webkit2_41`(Ubuntu 24.04 仅有 webkit2gtk-4.1;若装的是 4.0 则省略该 tag;Arch 仓库已无 4.0,tag 恒必带)
- **依赖保持纯 Go、无 CGO**(SQLite 用 `modernc.org/sqlite`,不用系统 keyring)——这是能从 Linux 交叉编译出 Windows .exe 的前提
- 本地数据目录:`os.UserConfigDir()/kist`,可用环境变量 `KIST_HOME` 覆盖(测试与多设备模拟用):
  - `config.json` — WebDAV 地址/用户名/设置(权限 0600;密码仅显式勾选"记住密码"时写入)
  - `keyfile` — 本地缓存的密钥文件(110 字节)
  - `index.db` — 明文索引(SQLite, WAL)
  - `backups/` — 被替换/归档的旧索引
- 临时文件:`os.TempDir()/kist/`,Manager 启动时清空上次残留
- 验收命令默认在项目根执行(本机 `/home/0x3ea/Projects/kist`)
- 每阶段完成后:把文件顶部"状态"改为"完成",勾掉任务清单
- **代码风格:必要处添加中文注释**(包文档、格式常量、关键算法步骤、易错边界);标识符保持英文,提交信息用中文并适当详细

## 进度看板

| 阶段 | 状态 |
|---|---|
| 0 Go 环境 | 完成 |
| 1 crypto | 完成 |
| 2 dav | 完成 |
| 3 index | 完成 |
| 4 管线+CLI | 完成 |
| 5 备份同步(CLI)| 完成 |
| 6 Wails 环境 | 完成 |
| 7 GUI | 代码完成,交互手测待跑 |
| 8 收尾打包 | 未开始 |

## TODO 完成记录(v0.1 后增强)

评估文档完成后从 [`todo/`](todo/) 移出,正文见 git 历史(路径 `docs/todo/<编号>-*.md`)。

| TODO | 内容 | 完成于 |
|---|---|---|
| 07 | 日志系统:slog + `KIST_HOME/kist.log` 启动轮转;补四处静默黑洞(缩略图失败 / dav 重试 / 索引写失败致孤儿 / 临时目录清理);Info=传输起止与 pull 决策 | 2026-08-20 |
| 13 | 出站箱:`put --defer` 加密+记账先行(files.state=uploading,schema 零迁移),产物落 `KIST_HOME/outbox`;`outbox list/push/verify/discard`;push 失败两档政策(keep 默认/discard 设置);verify 用 O(1) Probe + 大小核对收账(含 02) | 2026-08-20 |
| 12 | migrate:网盘间纯密文搬运(流式不落盘、整对象断点重试、目标同大小即跳过、keyfile/index.enc 双端哈希校验、`--switch` 验证后切换 config);dav 增 GetBody/PutStream 流式原语 | 2026-08-20 |
| 11 | 存在性探测 O(1) 化:KeyFileExists 改 PROPFIND Depth 0 精确探测(Exists→Probe 带大小);123pan 万级实测:**PROPFIND 不截断、时延线性 ≈2.2ms/对象、上传限速 ~100 请求/分钟与并发无关、递归 DELETE 带秒级一致性延迟**;结论存 [provider-notes.md](provider-notes.md) | 2026-08-20 |
| 08 | 大小混淆:blobVersion 2 量化填充(≤1MiB 归 4KiB 档、大文件 10% 阶梯,开销 ≤10%);交付与 SHA 只取真实明文,补零并入块加密受认证保护;读侧兼容 v1;设置 `size_padding` 可关 | 2026-08-20 |
| 15 | 文件夹打包:put 递归下降、叶子目录成 pack(一话一对象),get 解压还原目录;--expand/--keep-zip;非 UTF-8 名/特殊文件整次 put 拒绝;schema v2 `files.pack` | 2026-08-21 |
| 16 | 作品级元数据与聚合:schema v3(folders 加 note/user_meta/cover_file_id + tags/folder_tags 表);`FolderSummary` 纯查询子树聚合(全量内存建树,不用递归 CTE);封面三级回退链(CoverFileIDs ≤4,0=占位);`meta set/list`、search 兼查目录名/tag、`mv` 纯索引移动(远端零变化)、ls 目录行子树摘要(待传单列);实测 123pan 真实网盘全链路验收 | 2026-08-24 |
| 17 | 文件元数据补齐:schema v4 `file_tags`(镜像 folder_tags、共享 tags 词表,死词清理 UNION 双表——两侧清理互不误伤同名词);文件封面 = 自身 thumbnails 行,GUI「导入封面」走 `MakeThumbnail` 同规格管线(空路径=清除,pack 覆盖有确认,坏图 BAD_CONFIG 不静默);`meta set` 支持 uuid\|id 文件目标;search 文件侧补 tag 命中、`FileHit`/`FileDetail` 回填 Tags;MetaDialog 泛化(文件形态带封面预览与即时导入/清除) | 2026-08-25 |
| 18 | 新建虚拟目录:CLI `mkdir`(多级、幂等 mkdir -p 语义,`..` 拒绝,纯索引零流量)+ GUI 工具栏「新建文件夹」(当前目录下,输入可含 `/` 建多级,重名幂等复用);沙盒实测建/幂等/拒绝/ls 可见。原诉求的上传粒度选择砍掉并存档结论:批量上传文件从不打包,逐文件形态由「mkdir → 批量传文件」组合达成,CLI `--expand` 一直在 | 2026-08-25 |
| 19 | 目录重命名:`index.RenameFolder`(纯索引零流量;同名 no-op 事务外判定不空计 revision、撞名报错不自动 "(1)" 消解、根/软删/非法段拒绝)+ CLI `rename <路径> <新名>` + GUI 工具栏「重命名」(恰好选一个目录,预填全选);单测覆盖往返/no-op 不计 revision/撞名/跨级同名/根/软删/不存在,沙盒实测全语义 | 2026-08-25 |
| 20 | GUI 右键菜单:全局抑制 webkit2gtk 原生菜单(main.ts 一处 preventDefault,锁定/设置页同效;输入框剪切/复制/粘贴原生项随之消失,Ctrl+V 不受影响,立项已接受)+ 自绘 `ContextMenu.vue`(贴边夹紧、Esc/点外部/滚动关闭、危险红、禁用态悬浮原因);右键即选(目标不在选区变唯一选中,已在选区保持多选);菜单四项与原工具栏按钮同函数——重命名(单目录)/移动(仅文件)/元数据(恰一)/删除(带数量文案);工具栏收敛为搜索/上传×2/新建文件夹/下载/视图切换 | 2026-08-30 |
| 21 | 多网盘多库:一盘一库共用 keyfile——config schema v2(drives[]+active,旧格式首启自动迁移落盘钉死盘 ID,index.db 经 MigrateIndexFile 随迁);app.go `reopenVaultLocked` 换库三件套(要求管线空闲/切走前尽力补备份/保持解锁态);`SetActiveDrive`/`SaveDrive`/`DeleteDrive`/`ListDrives` 四绑定(查重 URL+用户名+根目录,拒绝删活动盘与最后一个盘);CreateAccount 开新库分支(推现有 keyfile,口令不符 AUTH_FAILED,残留索引归档 backups/);ChangePassphrase 多盘扇出;CLI `drive list/use`(19 个子命令);GUI 设置页档案列表(增删改/切换/测试)+ Lock 页当前盘名;新增 KIST_TMPDIR 隔离临时根(测试并行与 GUI+CLI 并行共用 /tmp/kist 的坑);多盘绑定测试逼出三真缺陷——查重先于赋值形同虚设、持锁调 mkSnapshot 自锁、SetActiveDrive 漏赋 active 切换无效;真实沙盒迁移实测(库原样可见、盘 ID 跨启动稳定) | 2026-08-30 |
| 10 | 封面出库:一封面一 blob + 索引轻引用——schema v5 `covers` 表(引用 + source derived/custom + state uploading/ready,thumbnails 转 legacy 只读回退);远端布局修订为两命名空间(`/kist/` 主 + `/kist/covers/` 封面,均随机名零明文);revision 规则:自动封面随上传事务写不额外计(顺手修掉 packFolder 空事务虚增 revision)、用户封面与 SetNote 同级、迁移逐行 raw 事务不计 + 结束一次 +1;封面完整接入出站箱(defer 双产物入箱、verify 挂账集扩为 files∪covers 防无主清理误删、discard 一笔=文件及其封面);gc 两命名空间分账(按 blobs.kind 路由删除,封面孤儿单独报告不自动删);`kistctl covers migrate`(断点续跑/--max 分批/VACUUM 回收);GUI `GetCover` 三级来源(磁盘 LRU 缓存键=uuid → 远端 → legacy 回退,锁定 [LOCKED])+ `SetFileCover` 断网自动回退出站箱 + 前端可见优先有界预取(IntersectionObserver + 并发 4 队列)+ 设置页封面缓存预算(cover_cache_mb,默认 512MB);本地 WebDAV 沙盒全链路手测(直传/defer→push/迁移/gc 孤儿分账) | 2026-08-30 |
| 14 | 操作审计日志:internal/audit 新包 + cmd/kistctl main 单点收口——每条命令恰好一行 JSONL 落 `KIST_HOME/audit.log`(ts/cmd/ok/错误码/耗时/extra 摘要),append-only 0600 长留存不随 kist.log 轮转;摘要按命令登记(put 的 files/bytes、migrate 的 copied/skipped/failed、rm 的文件名、backup 的 revision 等);失败消息取首行按 rune 截断 + URL userinfo 脱敏,`Log` 消费式清空摘要槽防残留;写失败只警告不改变退出码;组命令审计带子动作(outbox push 等);GUI 活动历史页后置,直接读 JSONL 即可;测试:audit 包(逐行可解析/中文空格转义/口令与凭据不落盘/截断/合并/0600/轮转隔离)+ cmd/kistctl 端到端(本地 WebDAV 真跑 init/put/unlock 错口令);沙盒实测五类命令 jq 逐行验收 | 2026-09-01 |
| 09 | 同步冲突检测:SVN 式基线校验——sync_state 增 `last_synced_rev`(KV 键零迁移;首读以当前 revision 初始化,升级库不误报;直写不进 revision,簿记非内容变更);`BackupNow`/`PullRemote` push/pull 前三方比较(本地/基线/远端 index.enc 头部 Meta,O(1) Probe 探存活性后整拉——123pan 无 Range,廉价化留待支持),revision 从裁判变证人:**平局(两侧各写一次计数相等)也判得出分叉**;分叉拒绝静默覆盖返回 `*Conflict`(diverged/remote-ahead),裁决两方向:保留本机 = `backup --force` 覆盖远端,保留云端 = `pull --force` 本地归档(ReplaceWith 现机制),CLI 双向 flag + CONFLICT 错误码,GUI `ResolveConflict` 绑定 + `sync:conflict` 事件 + SyncConflictDialog(自动/手动/退出三备份路径接入,挂起位防重复弹窗);基线推进点:成功 push=推的 rev、采纳远端=远端 rev(快照带的是对方基线必须显式重写);远端截断(CorruptBlob)推自愈、钥匙不符(WrongKey 与头部翻转同形)拒覆盖守跨账户;已知边界:force 推后对端计数相同且无改动时其 pull 会 noop,任一侧下次写即触发检出;测试:分叉/平局/双裁决/remote-ahead/自愈/基线升级全绿,既有双设备测试零改动通过(快进语义不变) | 2026-09-01 |
| 22 | 启动同步对账:检测时机从写操作前提到解锁后,补"GUI 无日常 pull、不写就不知道远端走了"的感知缺口——解锁/切盘后后台跑 `backup.Reconcile`,**复用 TODO-09 三方比较零新决策逻辑**(动作委托 BackupNow/PullRemote,原语内部重跑检测防预检与执行间竞态):双方未动 noop;只有本地动**补推**(立项时否决"纯云权威一律回滚"方案:'本地≠云端'是一种观测两种成因——对端推了该拉/本地没推出去该推,一律回滚会把离线/崩溃遗留的未推送工作从视图抹掉、离线删除复活;云权威认的是'已同步的云端状态',补推后稳态愿景不变);只有远端动**快进拉**(空库新设备同此路径自动恢复,Unlock 的 SuggestPullIndex 手动引导退役);双方动弹 SyncConflictDialog + **文件级 diff 三栏**(仅本机/仅云端/内容不同;`index.ListLivePaths`/`SnapshotPaths` 同构清单 + `DiffIndex` 按 kind+path 比对,blob 名即内容同一性,单栏截断 200 条 Total 保真;按需 `SyncConflictDetail` 拉取而非冲突时预计算,push 拦下的冲突与启动检出的共用一入口)——决策粒度仍是整库二选一不做行级 merge,diff 只是信息;**裁决前重检是硬要求**:`EnsureRemoteRev` 核对弹窗打开时的远端 rev,不符返回 resolved=false 前端刷新重裁决(启动检测把弹窗窗口从秒级拉到分钟级,force 跳过一切检测,不核对就静默覆盖对端新推送);对账与防抖备份共用 backingUp 互斥(被抢先即让路)、失败一次性提示不重试(每次 push/pull 自带同款检测,正确性不依赖对账);runAutoBackup 增免推守卫(rev==基线不空推同一份快照,恢复后的刷新不再触发无用上传);测试:四局面矩阵/空库自动恢复/无备份两分支/错密钥/远端截断上抛不自愈/重检三态/diff 三栏+截断+local-ahead 无 diff 全绿,既有双设备与 e2e 零改动通过 | 2026-09-03 |
| 23 | GUI 多选目录上传:fork wails(v2.15.0 → `0x3ea/wails` tag `v2.15.0-kist.1`,go.mod replace,不提上游)补 `OpenMultipleDirectoriesDialog` 三端实现——Linux 端 GTK `set_select_multiple` 只看 multipleFiles 标志与 SELECT_FOLDER 动作正交(零 C 改动)、Windows 端 vendored cfd 组合既有 setIsMultiselect+setPickFolders 并顺手修掉取消路径断言 panic、darwin 把 setAllowsMultipleSelection 移出 if(allowFiles) 块(上游隐性 bug:纯目录模式多选被静默忽略);kist 增 `PickDirs` 绑定(取消契约与 PickFiles 一致:空表=静默),「上传文件夹」一次勾选多个、一目录一任务叶子打包;`make check` 的 go build 不编译平台前端(dev/production tag 由 wails CLI 注入),对话框改动必须 make build/build-windows 验证 | 2026-10-06 |
