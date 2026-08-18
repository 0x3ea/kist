# Phase 6 — Wails / GUI 环境准备与脚手架合入

> 状态:未开始
> 前置:Phase 5(CLI 全流程已跑通——**里程碑之后才开始装 GUI 依赖**)
> 产出:系统 GUI 依赖、wails CLI、vue-ts 脚手架合入仓库(main.go 仍为模板,Phase 7 改造)

## 目标

一次性完成 GUI 侧的所有环境工作:装 GTK/WebKit 开发库与 wails CLI,把 vue-ts 脚手架合入现有纯 Go 仓库,并验证合入没有破坏任何已有核心代码。

## 要做什么(任务清单)

- [ ] 安装系统构建依赖:
  ```bash
  sudo apt update
  sudo apt install -y build-essential pkg-config libgtk-3-dev libwebkit2gtk-4.1-dev
  ```
  若提示找不到 4.1(Ubuntu 22.04 只有 4.0):改装 `libwebkit2gtk-4.0-dev`,并记下**后续所有 wails 命令不带 `-tags webkit2_41`**。
- [ ] 安装 wails CLI:
  ```bash
  go install github.com/wailsapp/wails/v2/cmd/wails@latest
  ```
  (PATH 已在 Phase 0 配好;否则把 `$(go env GOPATH)/bin` 加入 PATH)
- [ ] `wails doctor` 检查,输出需全绿
- [ ] 脚手架生成到临时目录并合入:
  ```bash
  wails init -n kist -t vue-ts -d /tmp/kist-scaffold
  ```
  从 `/tmp/kist-scaffold/kist/` 拷入项目根(项目已非空,`wails init` 不能直接在这里跑):
  - `frontend/` 整目录
  - `wails.json`
  - `build/`(资源与图标,不含 `bin/`)
  - `main.go`、`app.go`(模板版,Phase 7 改造)
  - 合并 `go.mod`:以现有 `module kist` 为准,把模板依赖合进来后 `go mod tidy`
- [ ] 合入后跑一次全量测试,确认核心未受影响

## 设计说明

- Wails v2 在 Linux 用 WebKitGTK(C 库),构建期需要开发头文件与 CGO;Windows 目标完全不需要 CGO(WebView2 走系统调用),这是后续 `wails build -platform windows/amd64` 能从本机交叉编译出 .exe 的原因
- `webkit2_41` build tag 只影响 Linux 端选 4.1/4.0 绑定,对 Windows 目标无影响
- **脚手架为什么走临时目录**:仓库在 Phase 0–5 已有内容,`wails init` 拒绝非空目录;生成后手动合入等价且可控。frontend 模板选 vue-ts:官方维护、TS 开箱、构建链轻

## 预期结果

仓库同时具备:完整 CLI/核心代码 + GUI 脚手架;`wails build` 能产出可运行的模板窗口。

## 验收标准

1. `wails doctor` DEPENDENCIES 全部 OK
2. `pkg-config --exists webkit2gtk-4.1 && echo ok` 输出 `ok`(装 4.0 的机器改查 `webkit2gtk-4.0`)
3. `wails build -tags webkit2_41` 成功,`./build/bin/kist` 能打开模板默认窗口(仅冒烟,功能在 Phase 7)
4. 合入后 `go test ./... -race -count=1` 仍全绿、`go vet ./...` 通过、`head -1 go.mod` 仍为 `module kist`
