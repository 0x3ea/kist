# Phase 7 — Wails GUI

> 状态:代码完成(2026-08-25)——自动化验收全过、沙盒冒烟通过;**交互手测清单(验收标准 1 的 11 条 + TODO-16 增量)待逐条过**,过完改"完成"
> 前置:Phase 6(环境与脚手架已合入);备份能力来自 Phase 5,本阶段负责它的 UI 接线
> 产出:改造后的 `main.go` / `app.go`(拆三文件)/ `frontend/src/` 四页面、Makefile

## 目标

给已经验证过的核心管线(含备份)套上桌面 GUI 外壳:**GUI 层不引入任何新业务逻辑**,只是 internal/* 的绑定与展示。完成后能完成"本地选文件 → 自动加密上传 → 列表搜索 → 下载自动解密"的完整体验,并支持新设备从远端恢复。

## 要做什么(任务清单)

- [x] `app.go`:App struct、生命周期(startup/shutdown)、全部绑定方法、事件发射
- [x] `internal/errs` 错误码 → 前端文案映射(壳层 codedError 方案,见下)
- [x] 前端:App.vue / store.ts / Lock.vue / Files.vue / Transfers.vue / Settings.vue
- [x] 备份 UI 接线:`BackupIndexNow` / `ImportFromRemote` / 防抖自动备份 + 退出前备份 / Lock.vue 新设备分支 / Settings 备份按钮
- [x] Makefile:`make dev` / `make build` / `make test` / `make check`(统一封装 `-tags webkit2_41`)
- [ ] 按"验收标准"逐条手测(交互部分)

## 设计说明

### app.go 绑定方法(前端可调,错误为 `*errs.AppError`)

```go
// 状态/配置/解锁
func (a *App) GetAppState() AppState
    // AppState{Configured, Unlocked, FileCount int}
func (a *App) SaveWebDAVConfig(cfg WebDAVConfig) error
func (a *App) TestConnection(cfg WebDAVConfig) TestResult     // {Ok, Detail string}
func (a *App) CreateAccount(passphrase string) error          // MK+keyfile+EnsureRoot+PUT keyfile
type UnlockResult struct{ PulledRemoteKeyfile, SuggestPullIndex bool; FileCount int }
func (a *App) Unlock(passphrase string) (UnlockResult, error) // 本地 keyfile 优先,无则拉远端缓存
func (a *App) ImportFromRemote(passphrase string) (backup.PullResult, error) // 新设备:keyfile+索引
func (a *App) Lock() error
func (a *App) ChangePassphrase(oldPass, newPass string) error

// 索引浏览
func (a *App) ListFolder(folderID int64) ([]index.Entry, error)  // 0 = 根
func (a *App) Breadcrumb(folderID int64) ([]index.Crumb, error)
func (a *App) Search(query string, limit int) ([]index.FileHit, error)
func (a *App) DeleteEntries(fileIDs, folderIDs []int64) error    // 软删 + blob 标 trash

// 用户自定义数据
type ThumbData struct { Data []byte; Mime string }               // []byte 经 Wails 序列化为 base64
func (a *App) GetThumbnail(fileID int64) (ThumbData, error)      // 前端拼 data URL 显示
func (a *App) SetNote(fileID int64, note string) error
func (a *App) SetUserMeta(fileID int64, metaJSON string) error

// 传输
func (a *App) UploadPaths(paths []string, destFolderID int64) error  // 异步,立即返回
func (a *App) DownloadTo(fileIDs []int64, destDir string) error      // 异步
func (a *App) CancelTransfer(id string) error
func (a *App) Transfers() []transfer.Transfer

// 设置/维护
func (a *App) GetSettings() config.Settings
func (a *App) SaveSettings(s config.Settings) error
func (a *App) BackupIndexNow() (backup.BackupInfo, error)      // Phase 5 能力的 UI 入口
```

App 持有 ctx、cfg、db、dav client、remote.Store、transfer.Manager、MK(互斥锁保护;`Lock()` 清零并 Wipe)。启动流程:Load config → Open index(存在时)→ 派发 `app:state`。

### 事件(后端 → 前端)

| 事件 | 载荷 | 说明 |
|---|---|---|
| `transfer:update` | transfer.Transfer | 进度,200ms 节流 |
| `transfer:done` / `transfer:error` | transfer.Transfer | 便于 toast |
| `transfers:changed` | []transfer.Transfer | 队列增删整表快照 |
| `index:changed` | {reason string} | 上传/删除/拉取后刷新文件列表;同时驱动防抖备份 |
| `app:state` | AppState | 解锁/锁定/配置变化 |
| `notify` | {level, text} | 通用提示(备份完成、LWW 覆盖提示等) |

### 自动备份时机(Phase 5 约定的 GUI 落地)

- 收到 `index:changed` 后防抖 30s(仅 Settings 里 AutoBackup 开启时)触发 BackupNow
- 应用退出前(shutdown):已解锁且 revision > 上次备份 revision 则 BackupNow
- 备份完成发 `notify`,失败也发(不阻塞传输)

### 前端页面

- **App.vue**:订阅 `app:state`;未配置/未锁定 → Lock.vue;已解锁 → 底部导航 Files / Transfers / Settings
- **store.ts**:reactive 全局状态;封装 `wailsjs/go/...` 调用与 `EventsOn` 订阅,页面不直接碰 wailsjs
- **Lock.vue** 三分支:首次(未配置)→ 表单 URL/用户名/密码/root + "测试连接" → 设置口令(两次输入)→ CreateAccount;已配置 → 输口令 Unlock;新设备(本地无 keyfile 且远端有)→ "从远端恢复"入口 → ImportFromRemote,展示 PullResult(含 LWW 覆盖与归档提示)
- **Files.vue**:面包屑(Breadcrumb)+ 列表/网格双视图(网格视图图片文件显示缩略图,调 GetThumbnail 拼 data URL)+ 搜索框(300ms 防抖,匹配文件名**与备注**,结果显示虚拟路径)+ 工具栏:上传文件 / 上传文件夹 / 下载所选 / 删除。选中文件的详情面板:缩略图、大小、加密/上传时间、sha256、备注编辑框(保存调 SetNote);查看高清大图按需解密原文件(后续增强,缩略图只服务浏览)。对话框用 Wails 运行时 `OpenMultipleFilesDialog` / `OpenDirectoryDialog`;选择后调 UploadPaths/DownloadTo 并切到 Transfers 页;监听 `index:changed` 自动刷新
- **Transfers.vue**:Snapshot 初始化 + `transfer:update` 增量渲染;每条含 Phase、进度条、取消按钮
- **Settings.vue**:WebDAV 配置 + 测试连接;并发数(1–4)/块大小/记住密码(带警示文案)/自动备份开关;"立即备份"按钮(显示 revision 与时间);ChangePassphrase;Lock

## 预期结果

`make dev` 打开窗口即可完成完整使用循环(含新设备恢复);`make build` 产出 `build/bin/kist`。

## 与签名草稿的差异(实施时定稿,均有原因)

- **TODO-16 全量纳入**(用户拍板):绑定加 `SearchAll`(目录+文件合并)、`Get/UpdateFolderMeta`、`MoveFiles`、`EnsureFolder`;`ListFolder` 绑定层合成 `FolderView{Crumbs,Entries,Summaries}`(一次往返,FolderSummaries 批量建树);前端网格视图目录卡渲染封面宫格(`CoverMosaic`,CoverFileIDs ≤4,0=按目录名 hash 占位),列表视图目录行带摘要(CLI `summaryLine` 同款措辞)。
- **`PickFiles`/`PickDir` 绑定**:Wails v2.15 的 JS 运行时**不暴露** Open*Dialog(`runtime.d.ts` 无导出,实测 wrapper 源码确认),对话框只能落 Go 侧。
- **错误码传递**:Wails 把绑定 error 序列化为纯字符串 reject,`AppError.Error()` 只有 Msg——壳层 `codedError` 统一包 `"[CODE] Msg"`,前端 `errors.ts` 正则反解;`internal/errs` 不动(CLI 输出不受影响)。
- **`AppState` 加 `HasLocalKeyfile`**:Lock 页三分支(向导/解锁/新设备恢复)的判定依据(前端无文件系统访问)。
- **GC 两步**:`PreviewGC`/`RunGC` 两个绑定(孤儿仍只报告不删)。
- **`FileDetail` DTO**:FileRow 的 `sql.NullInt64` JSON 形态是 `{Int64,Valid}` 对象,不可直接透传,摊平为 `*int64`。
- **`mgr.SetRemote()`(internal/transfer 小补全)**:SaveWebDAVConfig 重建 store 后热更新管线端点(worker 持快照防 data race);Deps 其余项都是闭包动态读,Remote 是唯一需要显式替换的依赖。
- **在途传输关窗确认**(OnBeforeClose):GTK 前端忽略自定义 Buttons,QuestionDialog 固定 Yes/No,判断按返回值 `"Yes"`。
- **上传不暴露 expand / 下载不暴露 keep-zip**:GUI 走默认(pack/解压),高级用法留 CLI。

## 实施中逼出的核心包修复

- **`index.UpdateFolderMeta` cover 清除的 FK 违例**:`cover_file_id` 带 `REFERENCES files(id)`,清除(0)直写会触发外键失败(CLI `meta set --cover 0` 同样踩雷)——修正为 0 → NULL。GUI 绑定层测试发现。
- **transfer worker 的 `deps.Remote` data race 隐患**:与 SetRemote 并发替换无同步,改为 worker 持 `remoteSnapshot()` 锁下快照。
- **`OnBeforeClose` 返回值语义写反(GUI 壳自身 bug,冒烟次日修)**:Wails v2 签名是 `func(ctx) (prevent bool)`——true = **阻止**关闭;曾按"true=允许关"实现,导致无传输时恒 prevent → SIGTERM 与点 X 都关不掉窗口、主线程永挂 `gtk_main`(goroutine dump 定位:无任何 goroutine 推进退出流程),OnShutdown 链路(退出前备份)也从未执行过。修正:无传输返回 false 放行;有传输弹确认,Yes(确认中止)放行、其余阻止。修复后 SIGTERM 3 秒内退出。教训:**框架回调的 bool 语义必须看签名命名**(prevent)而不是望文生义。

## 验收标准

1. `wails dev -tags webkit2_41` 启动无报错,以下手测清单逐条通过:
   - 首次向导:错误 URL/密码 → 测试连接给出可读失败原因;正确后建账户
   - 错口令解锁 → 明确提示,不解锁
   - 上传单个文件与文件夹:进度条推进、完成列表刷新、上传中可取消且无残留
   - 搜索(中文名)命中并显示虚拟路径;按备注关键词搜索命中
   - 图片上传后网格视图出现缩略图;详情面板显示加密/上传时间,备注可编辑保存
   - 选文件下载到指定目录:内容与源一致(抽查 sha256)
   - 删除后列表消失
   - 改口令:旧口令解锁失败、新口令成功,已上传文件仍可下载
   - Lock 后界面回到解锁页,MK 已清零
   - **新设备恢复**:复制 config.json 到空 KIST_HOME 模拟新机器 → 启动 → Lock 页出现"从远端恢复"→ 输口令 → 文件列表与原设备一致(**含缩略图与备注**);Settings"立即备份"显示 revision 与时间;自动备份在上传 30s 后触发
   - TODO-16 增量:目录行摘要/封面宫格显示正确;目录元数据(note/tag/封面)编辑保存生效;移动对话框纯索引移动(网盘零流量);搜索能按目录名/tag 命中并显示虚拟路径;**单击目录行在右栏速览元数据(封面/tag/摘要/备注)、双击进入目录**(2026-09-05 增补:目录此前只能经右键「元数据」对话框查看);同目录刷新(index:changed)不再收起右栏详情与选区,按条目仍在列表中保留
2. `wails build -tags webkit2_41` 成功,`./build/bin/kist` 启动冒烟通过
3. `make test`(= `go test ./... -race`)全绿;`go vet ./...` 通过

## 验收结果(2026-08-25)

- ✅ 标准 2:`wails build -tags webkit2_41` 产出 `build/bin/kist`(16MB);沙盒 KIST_HOME 冒烟:进程存活、SIGTERM 优雅走 shutdown(未解锁时跳过备份)、启动噪音仅 Phase 6 已知两类(SIGUSR1 借用 / appmenu-gtk-module)
- ✅ 标准 3:`go build ./... && go vet ./... && gofmt -l .` 全绿;`go test ./... -race -count=1` **97 项全过**(新增根包 headless 绑定测试 8 项:合成视图/搜索/路径拆分/删除/移动/元数据指针语义/缩略图 NOT_FOUND/错误码格式;`npm run build` 含 vue-tsc strict 亦过)
- ⏳ 标准 1:交互手测清单待跑(需窗口操作;沙盒已预置数据,清单见上)
