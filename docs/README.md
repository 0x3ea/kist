# kist 分阶段实施文档

总体设计(架构、加密格式、目录树)见根目录 [`PLAN.md`](../PLAN.md)。本目录把实施过程拆成 9 个阶段,每阶段一份文件,统一包含:

- **要做什么**:任务清单(checkbox,可勾选跟踪进度)
- **怎么设计**:接口签名、数据格式、算法与实现要点
- **预期结果**:该阶段结束时仓库里多了什么
- **验收标准**:必须全部通过才能进入下一阶段

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
- 所有 wails 命令带 `-tags webkit2_41`(Ubuntu 24.04 仅有 webkit2gtk-4.1;若装的是 4.0 则省略该 tag)
- **依赖保持纯 Go、无 CGO**(SQLite 用 `modernc.org/sqlite`,不用系统 keyring)——这是能从 Linux 交叉编译出 Windows .exe 的前提
- 本地数据目录:`os.UserConfigDir()/kist`,可用环境变量 `KIST_HOME` 覆盖(测试与多设备模拟用):
  - `config.json` — WebDAV 地址/用户名/设置(权限 0600;密码仅显式勾选"记住密码"时写入)
  - `keyfile` — 本地缓存的密钥文件(110 字节)
  - `index.db` — 明文索引(SQLite, WAL)
  - `backups/` — 被替换/归档的旧索引
- 临时文件:`os.TempDir()/kist/`,Manager 启动时清空上次残留
- 验收命令默认在项目根 `/home/ubuntu/Projects/kist` 执行
- 每阶段完成后:把文件顶部"状态"改为"完成",勾掉任务清单
- **代码风格:必要处添加中文注释**(包文档、格式常量、关键算法步骤、易错边界);标识符保持英文,提交信息用中文并适当详细

## 进度看板

| 阶段 | 状态 |
|---|---|
| 0 Go 环境 | 完成 |
| 1 crypto | 未开始 |
| 2 dav | 未开始 |
| 3 index | 未开始 |
| 4 管线+CLI | 未开始 |
| 5 备份同步(CLI)| 未开始 |
| 6 Wails 环境 | 未开始 |
| 7 GUI | 未开始 |
| 8 收尾打包 | 未开始 |
