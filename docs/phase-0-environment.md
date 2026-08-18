# Phase 0 — Go 基础环境与项目骨架

> 状态:已完成
> 前置:无
> 产出:`go.mod`(module `kist`)、目录骨架、(可选)git 仓库

## 目标

只准备纯 Go 开发环境。**CLI 全流程(Phase 1–5)不需要任何 GUI 系统依赖**;Wails/GTK/WebKit 环境有意推迟到 Phase 6,等 CLI 跑通后再配置。这保证了核心开发阶段在任何只有 Go 的机器上都能进行,也把 GUI 依赖的变化隔离在后半程。

## 要做什么(任务清单)

- [x] 确认 Go ≥ 1.24(本机 1.26.6,已满足):`go version`
- [x] 初始化模块:
  ```bash
  cd /home/ubuntu/Projects/kist
  go mod init kist
  ```
- [x] 建目录骨架(各阶段往里填内容):
  ```
  internal/{crypto,dav,index,remote,transfer,backup,config,errs,e2e}/
  cmd/kistctl/
  ```
- [x] (建议)`git init` 并做首次提交,后续每阶段一提交
- [x] 确认 PATH 含 `$(go env GOPATH)/bin`(后续 `go install` 的工具要用;可写入 `~/.zshrc`)

## 设计说明

- 纯 CLI 阶段零系统依赖、零 CGO(SQLite 用 modernc、不用 keyring)——这同时是从 Linux 交叉编译 Windows .exe 的前提
- **不预留任何 wails 结构**:`wails init` 拒绝写入非空目录,GUI 脚手架到 Phase 6 用"临时目录生成 → 拷贝合入"的方式处理,本阶段不需要为它做任何准备

## 预期结果

`go.mod` 存在且 module 名为 `kist`;目录骨架就位;`go build ./...` 通过。

## 验收标准

1. `go build ./...` 通过(空工程无报错)
2. `head -1 go.mod` 输出 `module kist`
3. (可选)`git log --oneline` 有首次提交
