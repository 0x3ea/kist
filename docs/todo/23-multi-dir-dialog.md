# TODO-23 — GUI 多选目录上传(原生对话框)

> 状态:**已完成(2026-10-06)**
> 来源:GUI「上传文件夹」一次只能选一个目录,批量上传要多次点按钮;CLI 不受限(`put` 本就收混合路径列表)

## 评估

- 核心管线零障碍:`Manager.UploadPaths` 本就吃任意文件+文件夹混合列表(`manager.go`),唯一卡点是**对话框层**——Wails v2.15 只有多选**文件**对话框(`OpenMultipleFilesDialog`),目录对话框单选写死(上游 issue #2411 挂着未做)
- 底细(读 v2.15.0 源码确认):
  - Linux:GTK C 层 `set_select_multiple` 只看 `multipleFiles` 标志,与 `SELECT_FOLDER` 动作**正交**——组合能力现成,只是没暴露
  - Windows:vendored cfd 的 `setIsMultiselect`(FOS_ALLOWMULTISELECT)与 `setPickFolders`(FOS_PICKFOLDERS)两个开关都已在 `iFileOpenDialog` 上,只是没有组合的构造器
  - darwin:NSOpenPanel 原生支持目录+多选,但 `setAllowsMultipleSelection` 被埋在 `if (allowFiles)` 块里,纯目录模式下多选被静默忽略(上游隐性 bug)
- 候选:fork 打补丁(选中)/ zenity 库(Linux 需运行时依赖 zenity 二进制,弃)/ 拖拽(可作后续)/ 自绘浏览器(重复造轮子,弃)

## 设计

- fork `wailsapp/wails` → `0x3ea/wails`,分支 `multi-dir-dialog`,钉 tag `v2.15.0-kist.1`,kist 经 `go.mod replace` 消费;**不提上游 PR**;wails CLI 仍是官方 v2.15.0(`go install` 不走 replace,Makefile/release.yml 不受影响)
- 补丁(8 文件,+85/-2,全部加法):`pkg/runtime/dialog.go` + `frontend.Frontend` 接口新增 `OpenMultipleDirectoriesDialog`;三平台各补实现(linux 复用既有调用链零 C 改动;windows 组合两个既有开关并**顺手修掉取消路径的断言 panic**;darwin 补方法 + .m 一行移位)
- 取消语义三端统一为「空表 + nil error」,kist 侧 `PickDirs` 再兜底 `nil → []string{}`,前端契约与 `PickFiles` 一致:空表=静默返回
- Windows 已知边界:虚拟 shell 位置(「此电脑」/库)无文件系统路径,选中会使整批失败(SIGDN_FILESYSPATH 取不到路径)——与上游多选文件行为一致,接受并记录

## 验收记录

- fork 门禁:`go build -tags "production webkit2_41" ./internal/... ./pkg/runtime/...` 通过(linux 前端真实编译);`GOOS=windows` 交叉编译 + vet 通过(仅上游 `winc/w32` 固有告警);darwin 仅代码审查(Linux 无法编译验证)
- 注意:`make check` 的 `go build ./...` **不编译任何平台前端**(wails 的 linux/windows 前端在 dev/production tag 之后,wails CLI 注入)——本特性起,验证对话框相关改动必须跑 `make build` / `make build-windows`
- kist:`make check` 全绿(含 e2e);`make build` 产出 GUI 且 wailsjs 再生成 `PickDirs`;`make build-windows` 产出 kist.exe
- Go 侧单测仅覆盖未就绪路径(`TestPickDirsNotReady`):对话框绑定依赖真实窗口 ctx(`getFrontend` 断言 wails 内部接口,kist 无法注入替身),多选/取消行为靠 GUI 手工验收
- GUI 手工验收:多选 N 个文件夹 → `已入队 N 个任务`(一目录一任务,叶子目录打包);Esc 取消 → 静默;单选/上传文件回归不受影响

## 后续

- 拖拽上传(文件+文件夹混合)是另一个候选入口(v2.15 Linux 文件拖拽完整实现,含目录路径),未排期
- wails 升级时:rebase 本补丁到新 tag,`-kist.N` 后缀递增;**tag 永不移位**
