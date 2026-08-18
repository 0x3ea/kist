# Phase 8 — 收尾:清理、打包、文档、交叉编译

> 状态:未开始
> 前置:Phase 7
> 产出:孤儿清理 UI、Makefile 完善、应用图标、README、Linux + Windows 正式产物

## 目标

补齐运维性功能(孤儿清理)、可交付性(图标/打包/交叉编译)与文档(格式规范、灾难恢复手册),交付 v1。

## 要做什么(任务清单)

- [ ] `CleanupOrphans(dryRun bool)`:远端 blob 与索引 blobs 表的比对清理
- [ ] Settings 页接入清理(预览 → 确认执行)
- [ ] `kistctl gc` 完整化(Phase 4 已有骨架)
- [ ] Makefile:`make dev / build / build-windows / test / fmt / vet`
- [ ] 应用图标与窗口标题(build/appicon.png、wails.json)
- [ ] README.md(内容大纲见下)
- [ ] 全量测试 + 正式构建 Linux 与 Windows 产物

## 设计说明

### 孤儿清理

```go
type CleanupReport struct {
    Orphans []string   // 远端有、索引无(可能是删除中断或另一设备索引回退)
    Deleted []string   // 实际删除的 trash blob
    Errors  []string
}
func (a *App) CleanupOrphans(dryRun bool) (CleanupReport, error)
```

逻辑:`ListBlobs()`(PROPFIND)与 `blobs` 表 diff——

1. 远端有、库里无 → 标记 orphan(报告,**默认不自动删**,由用户确认)
2. 库里 state=trash 且远端确认存在 → DELETE,记 Deleted
3. dryRun=true 只生成报告

### Makefile

```makefile
TAGS := webkit2_41   # 装 webkit2gtk-4.0 的机器改为空
dev:           ; wails dev -tags $(TAGS)
build:         ; wails build -tags $(TAGS)
build-windows: ; wails build -platform windows/amd64 -tags $(TAGS)
test:          ; go test ./... -race -count=1
fmt:           ; gofmt -w .
vet:           ; go vet ./...
```

### README 大纲

1. 是什么 / 截图
2. 快速开始(配置网盘、首次建账户、上传下载)
3. **安全模型**:加密格式(keyfile/blob 字节布局)、网盘能看到什么、忘记口令 = 数据不可恢复(无后门)
4. **灾难恢复手册**:换新电脑/重装系统的恢复步骤;`backups/` 归档的作用
5. 多设备使用注意(LWW:同时只在一台设备写,先 pull 后用)
6. 构建(Linux / 交叉编译 Windows)
7. 已知边界(密文大小泄露大致明文大小等)

## 预期结果

仓库达到可发布状态:功能完整、测试全绿、双平台产物可用、文档能支撑一个新用户从零配置和灾难恢复。

## 验收标准

1. `make test` 全绿(`go test ./... -race -count=1`)、`make vet` 无告警、`gofmt -l .` 为空
2. 孤儿清理手测:手工往远端 PUT 一个假 blob → dryRun 报告含它 → 确认执行后远端消失;索引内文件不受影响
3. `make build` 产出 `build/bin/kist`,本机启动、完成一次上传+下载冒烟
4. `make build-windows` 产出 `build/bin/kist.exe`(或对应输出名),在 Windows 机器/虚拟机上启动冒烟:配置 → 解锁 → 列表可见 → 下载一个文件内容正确
5. README 完成上述大纲全部小节
