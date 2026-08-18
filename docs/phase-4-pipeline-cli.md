# Phase 4 — 远端语义 / 传输管线 / CLI 端到端打通

> 状态:未开始
> 前置:Phase 1、2、3
> 产出:`internal/remote/store.go`、`internal/config/config.go`、`internal/errs/errs.go`、`internal/transfer/`(manager.go、upload.go、download.go、tempfiles.go)、`cmd/kistctl/main.go`、`internal/e2e/e2e_test.go`

## 目标

把 crypto + dav + index 三层串成完整的上传/下载管线,并用纯 CLI 端到端跑通——**在写任何 GUI 之前证明核心可用**。完成后:配置网盘 → `init` → `put` → 远端只有随机名 blob;`get` 回来与源文件逐字节一致。

## 要做什么(任务清单)

- [ ] `internal/config`:KIST_HOME 覆盖、config.json 读写(0600)
- [ ] `internal/errs`:AppError 错误码
- [ ] `internal/remote`:远端对象语义(keyfile / blob / index.enc)
- [ ] `internal/transfer`:Manager(worker 池、进度、取消、临时文件)
- [ ] 缩略图生成:图片上传时自动产出(`golang.org/x/image`,纯 Go)
- [ ] `cmd/kistctl`:全部子命令
- [ ] `internal/e2e`:本地 WebDAV 上的全链路测试
- [ ] (可选)真实网盘手动演练

## 设计说明

### internal/config

```go
func HomeDir() string            // KIST_HOME 环境变量优先,否则 os.UserConfigDir()/kist;确保目录存在
func ConfigPath() string         // HomeDir()/config.json
func KeyFilePath() string        // HomeDir()/keyfile
func IndexPath() string          // HomeDir()/index.db
func BackupDir() string          // HomeDir()/backups
type Settings struct { Concurrency int; ChunkMiB int; RememberPassword bool; AutoBackup bool }
type StoredConfig struct { URL, Username, Password, RootPath string; Settings Settings }
func Load() (*StoredConfig, error); func Save(*StoredConfig) error
```

`KIST_HOME` 覆盖是测试和多设备模拟的钥匙(Phase 6 e2e 靠它模拟第二台设备)。

### internal/errs

```go
type AppError struct { Code, Msg string } // 实现 error 接口
```

初始错误码:`BAD_CONFIG` / `NOT_CONFIGURED` / `DAV_ERROR` / `AUTH_FAILED`(口令错)/ `NOT_FOUND` / `CORRUPT`(文件损坏)/ `LOCKED` / `INTERNAL`。crypto 哨兵错误在此映射。

### internal/remote(远端语义层)

```go
type Store struct { /* dav.Client */ }
func NewStore(c dav.Client) *Store
func (s *Store) EnsureReady() error          // EnsureRoot
func (s *Store) KeyFileExists() (bool, error)
func (s *Store) PutKeyFile(b []byte) error   // /kist/keyfile(小文件,bytes 直接 PUT)
func (s *Store) GetKeyFile() ([]byte, error)
func (s *Store) PutBlob(name string, f *os.File) error
func (s *Store) GetBlob(name, tmpPath string, prog func(int64)) error
func (s *Store) ListBlobs() ([]string, error)   // PROPFIND,排除 keyfile / index.enc
func (s *Store) DeleteBlob(name string) error
// Phase 6 增:PutIndexBlob / GetIndexBlob(/kist/index.enc)
```

### internal/transfer(管线)

```go
type Deps struct {
    Remote      *remote.Store
    DB          *index.DB
    MK          func() (crypto.MasterKey, bool) // 未解锁 → 拒绝并报 LOCKED
    Concurrency func() int                      // 默认 2,可设 1–4
    Emit        func(event string, payload any) // 桥接事件;测试中直接收集
}
type Manager struct{ /* 队列 + worker 池 */ }
func NewManager(d Deps) *Manager
func (m *Manager) UploadPaths(ctx context.Context, paths []string, destFolderID int64) error // 展开+入队即返回
func (m *Manager) DownloadTo(ctx context.Context, fileIDs []int64, destDir string) error
func (m *Manager) Cancel(id string) bool
func (m *Manager) Snapshot() []Transfer

type Transfer struct {
    ID, Kind, Name, UUID string          // Kind: upload|download
    Phase string                          // queued|encrypting|uploading|downloading|decrypting|done|error|canceled
    BytesDone, BytesTotal int64
    StartedAt int64; Err string
}
```

**上传单文件**(worker 内串行,文件间并行):

1. stat 源文件;同目录同名已存在 → 自动追加 `(1)`、`(2)`…(不做覆盖合并,行为可预期)
2. 临时目录 `os.TempDir()/kist/<transferID>/` 建临时文件;`NewBlobWriter` 流式加密(内存 ≈ 一个块),Close 完成即得 `encrypted_at`
3. 缩略图(仅图片):扩展名 jpg/jpeg/png/gif/bmp/webp 且 `http.DetectContentType` 嗅探确认 → 解码(GIF 取首帧)→ `x/image/draw` 缩放到最长边 ≤512px → 重编码(不透明 JPEG q80 / 含透明 PNG)→ 超 128KB 逐级降质量/尺寸(上限只是防噪点图的保险,典型 25–45KB)。**失败仅记日志,绝不阻断上传**;与加密并行或紧后执行。高清大图预览走按需解密原文件,不依赖缩略图
4. `PutBlob` 整文件 PUT(重试在 dav 层;取消点:每次重试等待与每个块边界检查 ctx)
5. 同一 WithTx:InsertFile(state=ready,含 encrypted_at/uploaded_at)+ RegisterBlob + PutThumbnail(如有)+ revision+1
6. 删临时文件;Emit `index:changed`。失败:删临时文件、索引不落任何行、错误写入 Transfer.Err

**上传文件夹**:`filepath.WalkDir` 展开,跳过符号链接环;`EnsureFolderPath(dest, 相对路径段)` 逐级建目录;每个文件入队。

**下载单文件**:

1. 查索引拿 blob_name / cipher_size / sha256
2. `GetBlob` 到临时文件(prog 驱动进度)
3. `NewBlobReader` 流式解密,写入 `destDir/.<name>.kistpart`
4. 全部校验通过 → `os.Rename` 原子落盘;失败/取消 → 删除 .part 与临时文件

**临时文件管理**(tempfiles.go):Manager 启动时清空 `os.TempDir()/kist`(单实例假设);每传输独立子目录,结束即删。

### cmd/kistctl(CLI 规格)

标准库 flag,子命令式;口令一律 `--pass-stdin`(读一行)或环境变量 `KIST_PASS`,避免进 shell history。

| 命令 | 参数 | 行为 |
|---|---|---|
| `config set` | `--url --user [--root /kist] --pass-stdin` | 写 config.json(此处密码是 WebDAV 密码) |
| `init` | `--pass-stdin` | 生成 MK + keyfile → EnsureRoot → PUT keyfile → 建空索引 |
| `unlock` | `--pass-stdin` | 本地 keyfile 优先,无则拉远端并缓存;打印 OK/失败原因 |
| `put` | `<paths...> --dest /文档/子目录` | 递归上传,打印每个文件的进度行与结果 |
| `ls` | `[路径]` | 列虚拟目录(默认 /) |
| `search` | `<关键词>` | 搜索,输出 id、虚拟路径、大小 |
| `get` | `<uuid\|id> --to <目录>` | 下载解密,校验后落盘 |
| `info` | `<uuid\|id>` | 明细:大小、加密/上传时间、sha256、备注、有无缩略图 |
| `note` | `<id> [--set 文本]` | 查看/设置备注(经 WithTx,触发 revision 与后续备份)|
| `rm` | `<id...>` | 软删 + blob 标 trash |
| `gc` | `[--dry-run]` | trash 且远端确认存在 → DELETE;报告孤儿 |
| `backup` | — | Phase 5 实现 |
| `pull` | `--pass-stdin` | Phase 5 实现 |

CLI 与 GUI 共享全部 internal/*,只是另一个入口。

## 预期结果

不打开 GUI,命令行即可完成完整生命周期:配置 → 初始化 → 上传(文件/文件夹)→ 浏览/搜索 → 下载(逐字节一致)→ 删除 → 清理。远端目录里只有随机名 blob 与 keyfile。

## 验收标准

1. `go test ./... -race -count=1` 全绿,其中 `internal/e2e` 在 httptest WebDAV(经 `KIST_HOME` 指向临时家目录)上覆盖:
   - init → 远端存在 keyfile;`KeyFileExists` 为真
   - put 单文件 + 嵌套文件夹(包含中文名、空格名、空文件、大于 2 块的文件)→ 索引结构正确
   - ls / search 命中;get 后 sha256 与源一致
   - put 一张 jpg → info 显示有缩略图、`GetThumbnail` 返回合法图片字节;`note <id> --set "会议材料"` → `search 会议` 命中该文件
   - 同名二次上传 → 出现 `(1)` 后缀
   - rm → 列表消失;gc → 远端 blob 被删除
   - 错口令 unlock → 明确报 `AUTH_FAILED`
   - 上传过程中 cancel → 无索引残留、临时目录被清
2. 手动演练(可选,任意真实 WebDAV):
   ```bash
   go run ./cmd/kistctl config set --url https://... --user me --pass-stdin
   go run ./cmd/kistctl init --pass-stdin
   go run ./cmd/kistctl put ./testdir --dest /测试
   go run ./cmd/kistctl ls / && go run ./cmd/kistctl search 关键词
   go run ./cmd/kistctl get <uuid> --to /tmp/out && sha256sum /tmp/out/<file> ./testdir/<file>
   ```
3. `go vet ./...` 通过;`gofmt -l .` 无输出
