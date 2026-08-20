# CLAUDE.md

kist —— WebDAV 加密网盘管理器(Go + Wails v2)。CLI 与 GUI 共享 `internal/*` 核心,是同一核心的两个薄壳入口。

## 文档体系(动工前先看)

- `PLAN.md` — 总体设计(加密格式/schema/管线)+「后续计划」(候选增强的评估都在那)
- `docs/README.md` — **阶段进度看板**(每完成一阶段更新状态)
- `docs/phase-*.md` — 各阶段的任务清单/设计说明/验收标准,验收全过才算完成
- `docs/quickstart.md` — 用户视角的快速上手

## 常用命令

```bash
# 阶段收尾验收(必跑,全绿才算完)
go build ./... && go vet ./... && gofmt -l . && go test ./... -race -count=1

# 本地试用(零污染:数据在 .sandbox/,已 gitignore)
./kistctl-sandbox <子命令>          # 沙盒模式
./kistctl <子命令>                  # 正式模式(→ dist/ 下的发布二进制)

# GUI(Phase 6 系统依赖装好后):所有 wails 命令必须带 -tags webkit2_41
```

## 代码与提交约定

- 必要处添加中文注释:包文档(doc.go)、格式常量、关键算法步骤、易错边界;标识符保持英文
- git 提交信息用中文,写得详细(做了什么/为什么/如何验证);每阶段一提交,提交前更新对应 phase 文档与进度看板
- 测试先行:边界用例(空文件、块大小整数倍、篡改、截断、中途取消)必须覆盖;测试驱动发现的设计缺陷要在代码注释和 phase 文档里留记录
- 依赖保持纯 Go、无 CGO(SQLite 用 modernc.org/sqlite,不用系统 keyring)——这是从 Linux 交叉编译 Windows 产物的前提

## 架构速览

```
internal/crypto   加密核心(keyfile/blob 流式分块)
internal/dav      WebDAV 客户端 + 重试退避(PUT/GET 自管,其余 gowebdav)
internal/index    SQLite 明文索引(虚拟目录/文件/备注/缩略图/revision)
internal/remote   远端对象语义(/kist/ 下随机名 blob + keyfile + index.enc)
internal/transfer 传输管线(并发/进度/取消)+ 缩略图生成 + gc + 出站箱(--defer/push/verify)
internal/backup   索引云备份与多设备恢复(LWW)
internal/config   KIST_HOME 路径与 config.json
internal/errs     AppError 错误码(前端按 Code 映射文案)
internal/logging  全局日志(slog → KIST_HOME/kist.log,启动轮转留一代)
internal/migrate  网盘间纯密文迁移(枚举/断点搬运/双端校验)
入口:cmd/kistctl(CLI)、Wails app.go(Phase 7)
```

- 进度上报走 `transfer.Deps.Emit` 回调:CLI 接 `fmt.Printf`,GUI 接 Wails `EventsEmit`,核心代码不感知 UI
- 远端布局永远扁平:网盘上看不到文件名/目录结构,映射只存在本地索引

## 关键机制与坑(实测踩过)

- SQLite 的 PRAGMA 必须挂在 DSN(`_pragma=...&_txlock=immediate`):database/sql 连接池每条连接都要生效;`immediate` 防并发写事务 SQLITE_BUSY
- PUT 请求体不能裸传 `*os.File`:`http.Transport` 结束请求时会 Close 它,重试必失败;用 `SectionReader + NoCloser` 包装
- 上传文件夹时目录 id 必须**逐级**记录(只记最深一级会让文件夹根下的文件拿到零值 folderID)
- Go flag 遇到首个位置参数即停止解析:CLI 参数经 `parseArgs` 重排,允许 `put /路径 --dest /x` 混写
- zsh 不做变量分词:脚本里封装命令要用函数,不是 `K="go run ..."` 加 `$K`
- `--dest` 是虚拟目录(索引侧路径),`--to` 才是本地目录;虚拟路径容忍 `./` 前缀,拒绝 `..`
- Argon2 参数必须设上界(keyfile 携带天文数字参数是资源耗尽攻击向量)

## Claude Code 工具坑(实测踩过)

- 工具是延迟加载的,需经 ToolSearch 检索;其正则匹配**区分大小写**——小写 `read|bash` 搜不到 `Read`/`Bash`,`.*` 全量枚举也返回空。怀疑工具缺失时,先用 `select:Read,Bash`(确切名)点名验证,勿凭一次搜索失败断言"没有该工具"(2026-08 曾因此误判"本会话读不了文件")
