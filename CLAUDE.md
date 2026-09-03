# CLAUDE.md

kist —— WebDAV 加密网盘管理器(Go + Wails v2)。CLI 与 GUI 共享 `internal/*` 核心,是同一核心的两个薄壳入口。

## 文档体系(动工前先看)

- `PLAN.md` — 总体设计(加密格式/schema/管线)+「后续计划」(候选增强的评估都在那)
- `docs/README.md` — **阶段进度看板**(每完成一阶段更新状态)
- `docs/phase-*.md` — 各阶段的任务清单/设计说明/验收标准,验收全过才算完成
- `docs/system-map.md` — **系统现状快照**(模块地图、put/get 字节旅程、不变量清单、失败模式);phase 文档记录"怎么建成的",它回答"现在是什么样"
- `docs/quickstart.md` — 用户视角的快速上手
- `docs/provider-notes.md` — **网盘实测特性备忘**(限速/时延/截断/删除语义;批量操作与分桶决策的依据)

## 常用命令

```bash
# 阶段收尾验收(必跑,全绿才算完)
make check                          # = go build + vet + gofmt 检查 + 全量测试(-race)

# 构建与开发(所有 wails 命令已由 Makefile 统一封装 -tags webkit2_41,
# Arch 仓库已无 webkit2gtk-4.0,该 tag 恒必带;直接手敲 wails 命令时勿漏)
make build                          # GUI 发布构建 → build/bin/kist
make cli                            # CLI → build/bin/kistctl
make dev                            # 热重载开发窗口

# 本地试用(零污染:数据在 .sandbox/,已 gitignore)
./kistctl-sandbox <子命令>          # 沙盒模式(KIST_HOME 固定 .sandbox/)
build/bin/kistctl <子命令>          # 正式模式
```

## CI/CD(.github/workflows)

- `ci.yml`:每次分支 push 自动跑 `make check`( ubuntu runner 需先 `npm ci && npm run build` 产出 `frontend/dist`——go:embed 需要;装 libgtk-3-dev/libwebkit2gtk-4.1-dev,xvfb 套壳保险)。tags 推送不触发(branches `**` 只匹配分支)。
- `release.yml`:推 `v*` 标签触发,也可 Actions 页手动触发(不提交 tag;版本号输入 > 当前 tag > 最近 tag > 0.0.0-短SHA,产物挂 v<版本> Release,不存在则建)——发布前先全量测试,然后产出 Linux/Windows 各 GUI+CLI 四个裸二进制(命名沿用 quickstart:`kistctl-<版本>-<os>-<arch>`,版本经 -ldflags 注入)+ checksums.txt。wails CLI 钉 v2.15.0(与 go.mod 同源)。

## 代码与提交约定

- 必要处添加中文注释:包文档(doc.go)、格式常量、关键算法步骤、易错边界;标识符保持英文
- git 提交信息用中文,写得详细(做了什么/为什么/如何验证);每阶段一提交,提交前更新对应 phase 文档与进度看板
- 测试先行:边界用例(空文件、块大小整数倍、篡改、截断、中途取消)必须覆盖;测试驱动发现的设计缺陷要在代码注释和 phase 文档里留记录
- 依赖保持纯 Go、无 CGO(SQLite 用 modernc.org/sqlite,不用系统 keyring)——这是从 Linux 交叉编译 Windows 产物的前提
- 新增用户可见功能后同步更新 `docs/quickstart.md`(用法示例与「当前边界」),上手文档不落后于 CLI;改动系统行为/结构后同步更新 `docs/system-map.md`,现状图与代码一致(失真的地图比没有更糟)

## 架构速览

```
internal/crypto   加密核心(keyfile/blob 流式分块;v2 大小量化填充)
internal/dav      WebDAV 客户端 + 重试退避(PUT/GET 自管,其余 gowebdav)
internal/index    SQLite 明文索引(虚拟目录/文件/备注/封面引用/revision)
internal/remote   远端对象语义(/kist/ 主命名空间随机名 blob + keyfile + index.enc;/kist/covers/ 封面子命名空间)
internal/transfer 传输管线(并发/进度/取消)+ 文件夹打包(一话一对象,TODO-15)+ 缩略图生成 + gc + 出站箱(--defer/push/verify)
internal/backup   索引云备份与多设备恢复(LWW)
internal/config   KIST_HOME 路径与 config.json
internal/errs     AppError 错误码(前端按 Code 映射文案)
internal/logging  全局日志(slog → KIST_HOME/kist.log,启动轮转留一代)
internal/migrate  网盘间纯密文迁移(枚举/断点搬运/双端校验)
入口:cmd/kistctl(CLI)、Wails app.go(Phase 7)
```

- 进度上报走 `transfer.Deps.Emit` 回调:CLI 接 `fmt.Printf`,GUI 接 Wails `EventsEmit`,核心代码不感知 UI
- 远端两命名空间(TODO-10 起,有意破例于"永远扁平"):`/kist/` 主命名空间扁平随机名 blob + keyfile + index.enc,`/kist/covers/` 封面子命名空间;两侧都零明文元数据,映射只存在本地索引
- 封面字节走 blob 管线(TODO-10):索引只存 covers 轻引用 + source(derived/custom)+ state(uploading 不可见);引用与 blobs 登记同事务,自动封面不额外计 revision,替换/清除时旧 blob 同事务 trash

## 关键机制与坑(实测踩过)

- SQLite 的 PRAGMA 必须挂在 DSN(`_pragma=...&_txlock=immediate`):database/sql 连接池每条连接都要生效;`immediate` 防并发写事务 SQLITE_BUSY
- PUT 请求体不能裸传 `*os.File`:`http.Transport` 结束请求时会 Close 它,重试必失败;用 `SectionReader + NoCloser` 包装
- 上传文件夹时目录 id 必须**逐级**记录(只记最深一级会让文件夹根下的文件拿到零值 folderID)
- Go flag 遇到首个位置参数即停止解析:CLI 参数经 `parseArgs` 重排,允许 `put /路径 --dest /x` 混写
- zsh 不做变量分词:脚本里封装命令要用函数,不是 `K="go run ..."` 加 `$K`
- `--dest` 是虚拟目录(索引侧路径),`--to` 才是本地目录;虚拟路径容忍 `./` 前缀,拒绝 `..`
- Argon2 参数必须设上界(keyfile 携带天文数字参数是资源耗尽攻击向量)
- wails 绑定生成器会**整字段丢弃**空名 json tag(如 `Diff T \`json:",omitempty"\``):前端类型缺该字段,`wails dev/build` 编译失败;跨端结构体要么不挂 tag,要么 tag 写全字段名(实测踩过,TODO-22)

## Claude Code 工具坑(实测踩过)

- 工具是延迟加载的,需经 ToolSearch 检索;其正则匹配**区分大小写**——小写 `read|bash` 搜不到 `Read`/`Bash`,`.*` 全量枚举也返回空。怀疑工具缺失时,先用 `select:Read,Bash`(确切名)点名验证,勿凭一次搜索失败断言"没有该工具"(2026-08 曾因此误判"本会话读不了文件")
