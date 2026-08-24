# Phase 6 — Wails / GUI 环境准备与脚手架合入

> 状态:完成(2026-08-24,Arch Linux 实机;与 Ubuntu 原稿的出入见「执行记录」)
> 前置:Phase 5(CLI 全流程已跑通——**里程碑之后才开始装 GUI 依赖**)
> 产出:系统 GUI 依赖、wails CLI、vue-ts 脚手架合入仓库(main.go 仍为模板,Phase 7 改造)

## 目标

一次性完成 GUI 侧的所有环境工作:装 GTK/WebKit 开发库与 wails CLI,把 vue-ts 脚手架合入现有纯 Go 仓库,并验证合入没有破坏任何已有核心代码。

## 要做什么(任务清单)

- [x] 安装系统构建依赖(Arch 实机**零安装**:`base-devel`/`pkgconf`/`gtk3`/`webkit2gtk-4.1` 已齐,Arch 不拆 -dev 包;Ubuntu 原命令留档备查):
  ```bash
  sudo apt update
  sudo apt install -y build-essential pkg-config libgtk-3-dev libwebkit2gtk-4.1-dev
  ```
  若提示找不到 4.1(Ubuntu 22.04 只有 4.0):改装 `libwebkit2gtk-4.0-dev`,并记下**后续所有 wails 命令不带 `-tags webkit2_41`**。
  Arch:仓库已无 4.0 包(`webkit2gtk` 查无),4.1 是唯一选项——**tag 恒必带**,无二义。
- [x] 安装 wails CLI:
  ```bash
  go install github.com/wailsapp/wails/v2/cmd/wails@latest
  ```
  (PATH 已在 Phase 0 配好;否则把 `$(go env GOPATH)/bin` 加入 PATH)
  Arch 新机器须补:`echo 'export PATH="$HOME/go/bin:$PATH"' >> ~/.zshrc`,否则 zsh 找不到 wails。
- [x] `wails doctor` 检查:gcc/gtk3/npm/pkgconf 均 Installed;docker 为可选项(Phase 8 才用);**libwebkit 报 Not Found 是误报**——根因与判据替代见「执行记录」第 3 条
- [x] 脚手架生成到临时目录并合入:
  ```bash
  wails init -n kist -t vue-ts -d /tmp/kist-scaffold
  ```
  从 `/tmp/kist-scaffold/` 拷入项目根(项目已非空,`wails init` 不能直接在这里跑;v2.15 的 `-d` 即项目根,**不再嵌套 `<name>/` 子目录**,与本清单原稿预期不同):
  - `frontend/` 整目录(修掉模板 App.vue 的 `</script>` 粘连 bug,见「执行记录」第 5 条)
  - `wails.json`(合入后把 `outputfilename` 从 `kist-scaffold` 改回 `kist`)
  - `build/`(资源与图标,不含 `bin/`)
  - `main.go`、`app.go`(模板版,Phase 7 改造)
  - 合并 `go.mod`:以现有 `module kist` 为准(保留 `go 1.26.6`,高于模板的 1.25),直接依赖仅增 wails 一项,其余交 `go mod tidy`
  - 模板的 `README.md`、`.gitignore` 不拷(项目自有文档体系与更全的 ignore 规则)
- [x] 合入后跑一次全量测试,确认核心未受影响(build/vet/gofmt/`go test -race` 全绿)

## 设计说明

- Wails v2 在 Linux 用 WebKitGTK(C 库),构建期需要开发头文件与 CGO;Windows 目标完全不需要 CGO(WebView2 走系统调用),这是后续 `wails build -platform windows/amd64` 能从本机交叉编译出 .exe 的原因
- `webkit2_41` build tag 只影响 Linux 端选 4.1/4.0 绑定,对 Windows 目标无影响
- **脚手架为什么走临时目录**:仓库在 Phase 0–5 已有内容,`wails init` 拒绝非空目录;生成后手动合入等价且可控。frontend 模板选 vue-ts:官方维护、TS 开箱、构建链轻

## 预期结果

仓库同时具备:完整 CLI/核心代码 + GUI 脚手架;`wails build` 能产出可运行的模板窗口。

## 验收标准

1. `wails doctor` DEPENDENCIES 全部 OK(Arch 修正:libwebkit 项因上游包名映射过期必报 Not Found,见执行记录第 3 条,实际判据以下面 2/3 为准)
2. `pkg-config --exists webkit2gtk-4.1 && echo ok` 输出 `ok`(装 4.0 的机器改查 `webkit2gtk-4.0`)✅ 实测 2.52.6
3. `wails build -tags webkit2_41` 成功,`./build/bin/kist` 能打开模板默认窗口(仅冒烟,功能在 Phase 7)✅ Wayland 会话窗口存活、SIGTERM 优雅退出
4. 合入后 `go test ./... -race -count=1` 仍全绿、`go vet ./...` 通过、`head -1 go.mod` 仍为 `module kist` ✅

## 执行记录(Arch Linux 差异与坑)

原稿按 Ubuntu 撰写,以下为 2026-08-24 在 Arch 实机(webkit2gtk-4.1 2.52.6 / gcc 16 / node 26 / Go 1.27)执行时的全部出入:

1. **系统依赖零安装**:Arch 不拆 -dev 包,`base-devel`(gcc/make)、`pkgconf`、`gtk3`、`webkit2gtk-4.1` 常备即够;仓库已移除 4.0 包,`-tags webkit2_41` 无条件必带。
2. **PATH**:新机器 `$(go env GOPATH)/bin` 不在 PATH,`go install` 出的 wails 调不到(`zsh: command not found`);`~/.zshrc` 加 `export PATH="$HOME/go/bin:$PATH"` 解决。
3. **wails doctor 误报 libwebkit Not Found**:doctor 的检测是问包管理器"装没装某包名",而 wails v2.15 源码 `internal/system/packagemanager/pacman.go` 把 libwebkit 映射写死为 `webkit2gtk`(4.0 包名),Arch 仓库只余 `webkit2gtk-4.1`,于是永远查无。**doctor 结论≠编译事实**——编译期实际走 cgo + pkg-config(`webkit2gtk-4.1` 解析正常)。上游映射未更新前 doctor 不可能全绿,勿为凑绿降级装 4.0 旧包。
4. **`wails init -d` 布局变化**:v2.15 的 `-d` 就是项目根,不再生成 `<d>/<name>/` 嵌套,模板文件直接在 `/tmp/kist-scaffold/` 下。连带副作用:`wails.json` 的 `outputfilename` 取了目录名 `kist-scaffold`,合入后手工改回 `kist`。
5. **官方模板 bug(vue-ts)**:`frontend/src/App.vue` 第 2 行闭合标签粘在 import 末尾(`...'./components/HelloWorld.vue'</script>`),vue-tsc 把 `<` 当 TS 源码报 TS1005,`wails build` 前端编译失败。修复=拆成两行。属 wails v2.15 模板瑕疵,升级 CLI 时可留意上游是否已修。
6. **冒烟噪音**:GUI 启动打印 "Overriding existing handler for signal 10"(WebKitGC 借用 SIGUSR1,正常)与 "Failed to load module appmenu-gtk-module"(Ubuntu 特有模块,Arch 无此物,无害),均不需处理。
7. **go.mod 合并细节**:保留仓库 `go 1.26.6`(模板 1.25 更低);模板 go.mod 末尾带一行 `// replace ... => 模块缓存` 注释,是生成机环境痕迹,不拷。`go mod tidy` 会把实际被 import 的 gowebdav/sqlite/x-image 等提为直接依赖,属正常整理。
