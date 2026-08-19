# kist — WebDAV 加密网盘管理器(Go + Wails v2)

> 按进度细化的分阶段实施文档(每步的任务/设计/预期结果/验收标准)见 [`docs/`](docs/) 目录,从 [docs/README.md](docs/README.md) 开始。

## Context

用户要一个网盘管理器:本地选择文件/文件夹后自动加密上传到网盘(WebDAV),下载自动解密;本地维护明文索引便于查找;所有加密文件共用一个密钥。项目目录 `/home/ubuntu/Projects/kist` 为空,从零开始。

已确认的决策:
- **GUI 桌面应用**(Wails v2:Go 后端 + vue-ts 前端,系统 WebView)
- **通用 WebDAV**(按最保守兼容性:整文件 PUT、不做分块 PUT)
- **远端扁平布局**:网盘上只有 `/kist/` 目录下的随机名加密 blob + `keyfile` + `index.enc`,目录结构/文件名只存在于本地索引(隐私优先)
- **索引云备份**:索引变更后加密备份上传,新设备凭密码拉回恢复
- **索引含用户自定义数据**:每文件记录加密时间、备注(可编辑、可搜索)、图片自动缩略图(最长边 ≤512px、JPEG q80、上限 128KB,存于索引库随备份同步;视频缩略图需 ffmpeg,留作增强)

本机环境:Go 1.26.6 ✓、Node 24 ✓;**wails CLI 与 GTK/WebKit 系统库未装**(按环境策略推迟到 Phase 6——CLI 全流程跑通后再配置;Ubuntu 24.04 只有 webkit2gtk-4.1,所有 wails 命令需带 `-tags webkit2_41`)。

## 架构总览

```
Wails WebView (vue-ts):Lock / Files / Transfers / Settings
        │ bind 调用 + EventsEmit 事件
app.go  App struct(绑定方法、生命周期、事件发射;GUI 只是壳)
        │
internal/transfer   上传/下载管线(并发、进度、取消、临时文件)
internal/backup     索引云备份 / 拉取恢复 / LWW
internal/index      SQLite(唯一真源:虚拟目录、文件、blob 登记)
internal/remote     远端对象语义(keyfile / blob / index.enc)
internal/dav        gowebdav 封装 + 自实现整文件 PUT + 重试退避
internal/crypto     keyfile(Argon2id 包装主密钥)+ blob 流式分块加密
internal/config     config.json、路径、设置
cmd/kistctl         纯 CLI 入口(GUI 之前先打通核心管线)
```

依赖(全部无 CGO,便于交叉编译):`wails/v2`、`golang.org/x/crypto`(argon2/chacha20poly1305/hkdf)、`modernc.org/sqlite`、`github.com/studio-b12/gowebdav`、`golang.org/x/net/webdav`(仅测试用,起本地 WebDAV 服务)。不用 uuid 库(16B 随机→hex)、不用 cobra(标准库 flag)。

关键取舍:
- **XChaCha20-Poly1305 而非 AES-GCM**:纯 Go 下无需 AES-NI 性能稳定,192bit nonce 允许"随机前缀+计数器"STREAM 式派生。
- **主密钥 MK 为随机 32B,Argon2id 只做 KEK 包装**:改口令只重写 keyfile,无需重加密任何 blob。
- **密码默认不落盘**(每次启动输入),"记住密码"为显式 opt-in 明文写入 config.json 并在 UI 警示。不用 go-keyring(Linux 需 libsecret CGO)。

## 加密格式(核心规范)

### keyfile(本地 `~/.config/kist/keyfile` 与远端 `/kist/keyfile` 同一份字节,110B)

```
偏移 长度  字段
0    8    magic "KISTKEY1"
8    2    version u16le (=1)
10   2    flags u16le
12   16   argon2 salt(随机)
28   4    argon2 time u32le
32   4    argon2 memKiB u32le        (默认 65536)
36   1    argon2 threads u8
37   1    keyLen u8 (=32)
38   24   wrapNonce(随机)
62   48   wrappedMK = XChaCha20-Poly1305(KEK, pt=MK[32], aad=bytes[0..62))
```

- `KEK = Argon2id(pass, salt, time, memKiB, threads, 32)`;解锁时 AEAD Open 失败 = 口令错误(天然校验,无需额外 verifier)。
- `Rewrap(oldPass, newPass)`:解出 MK 后重新包装,重写本地+远端。

### blob(加密文件,索引备份 index.enc 复用同一格式)

```
偏移 长度  字段
0    8    magic "KISTBLB1"
8    2    version u16le (=1)
10   2    flags u16le
12   4    metaLen u32le (=116)
16   24   metaNonce(随机)
40   116  sealedMeta = XChaCha20-Poly1305(metaKey, pt=Meta[100], aad=bytes[0..40))
--- Meta 明文(加密,不泄露文件名/大小/摘要)---
  fileID[16] | origSize u64 | chunkSize u32 | noncePrefix[16] | plainSHA256[32]
  | mtime u64 | revision u64 | deviceID[8]     (后三字段普通文件=0,索引备份用)
--- 块区(自偏移 156 起,连续无块头)---
第 i 块密文 = XChaCha20-Poly1305(fileKey, nonce=noncePrefix||u64be(i),
              aad="KISTBLB1"||fileID||u64be(i)||finalFlag) ‖ 明文块 + 16B tag
```

- 密钥派生:`fileKey = HKDF-SHA256(MK, salt=fileID, info="kist/v1/blob")`;`metaKey = HKDF-SHA256(MK, salt="kist/v1", info="kist/v1/meta")`。默认块 4 MiB。
- 防护:块序号+final 标志进 AAAD(防重排/截断移花接木);期望密文总长 = `156+16n+origSize` 校验(防尾部追加);读毕校验累计大小与流式 SHA-256(防删块);空文件 = 恰好 1 个空末块。
- 明文 SHA、大小、fileID 全在 sealedMeta 内,blob 不泄露任何元数据;单遍流式(临时文件可 seek,header 先占位 Close 时回填)。

### 导出接口

```go
// internal/crypto
type MasterKey [32]byte
func GenerateMasterKey() (MasterKey, error)
func CreateKeyFile(pass string) (*KeyFile, MasterKey, error)
func ParseKeyFile(b []byte) (*KeyFile, error)
func (k *KeyFile) Unlock(pass string) (MasterKey, error)
func (k *KeyFile) Bytes() []byte
func (k *KeyFile) Rewrap(oldPass, newPass string) error

func NewBlobWriter(w io.WriteSeeker, mk MasterKey, opt EncryptOptions) (*BlobWriter, error) // Write 流式、Close 回填,Meta() 供索引
func NewBlobReader(r io.Reader, size int64, mk MasterKey) (*BlobReader, error)             // header 快速失败,Read 流式,EOF 终检
```

## WebDAV 层

```go
// internal/dav
type Config struct{ URL, Username, Password, RootPath string } // RootPath 默认 "/kist"
type Client interface {
    Ping() error                                  // PROPFIND Depth 0
    EnsureRoot() error                            // Mkdir,忽略已存在(405)
    PutFile(remotePath string, f *os.File) error  // ★自实现:Content-Length=stat.Size()(gowebdav WriteStream 走 chunked,保守网盘可能拒绝)
    GetToFile(remotePath, localPath string, prog func(int64)) (int64, error)
    List() ([]RemoteObject, error)
    Delete(remotePath string) error
    Move(old, new string) error
}
```

- 重试 `withRetry`:只重试网络错误/5xx/429;指数退避 1s→30s ±20% 抖动,尊重 Retry-After,默认 5 次。PUT 目标是全新随机名故幂等。
- `internal/remote.Store`:KeyFileExists/Put/Get、PutBlob/GetBlob(name=32hex)、ListBlobs(排除 keyfile/index.enc)、DeleteBlob、Put/GetIndexBlob(`/kist/index.enc`)。首次使用 MKCOL 根目录。

## 索引(SQLite,modernc.org/sqlite)

```sql
CREATE TABLE folders (
  id INTEGER PRIMARY KEY, uuid TEXT NOT NULL UNIQUE,
  parent_id INTEGER REFERENCES folders(id) ON DELETE CASCADE, -- 根固定 id=1
  name TEXT NOT NULL, created_at INTEGER NOT NULL, deleted_at INTEGER);
CREATE UNIQUE INDEX ux_folders_live ON folders(parent_id, name) WHERE deleted_at IS NULL;

CREATE TABLE files (
  id INTEGER PRIMARY KEY, uuid TEXT NOT NULL UNIQUE,        -- = meta.fileID(hex)
  folder_id INTEGER NOT NULL REFERENCES folders(id),
  name TEXT NOT NULL, size INTEGER NOT NULL, cipher_size INTEGER NOT NULL,
  sha256 TEXT NOT NULL, chunk_size INTEGER NOT NULL,
  blob_name TEXT NOT NULL UNIQUE,                           -- 远端 32hex 对象名
  state TEXT NOT NULL DEFAULT 'ready',                      -- uploading|ready|missing
  created_at INTEGER NOT NULL, modified_at INTEGER NOT NULL,
  encrypted_at INTEGER, uploaded_at INTEGER,                -- 加密/上传完成时间
  note TEXT, user_meta TEXT,                                -- 备注(可搜索)/自定义 JSON 扩展位
  deleted_at INTEGER);
CREATE INDEX ix_files_folder ON files(folder_id) WHERE deleted_at IS NULL;
CREATE INDEX ix_files_name ON files(name);

CREATE TABLE thumbnails (                                   -- 图片缩略图,存索引库,随加密备份同步
  file_id INTEGER PRIMARY KEY REFERENCES files(id) ON DELETE CASCADE,
  data BLOB NOT NULL, width INTEGER NOT NULL, height INTEGER NOT NULL,
  mime TEXT NOT NULL);                                      -- ≤128KB,image/jpeg|image/png

CREATE TABLE blobs (
  name TEXT PRIMARY KEY, size INTEGER NOT NULL,
  kind TEXT NOT NULL DEFAULT 'file',                        -- file|index|keyfile
  state TEXT NOT NULL DEFAULT 'active',                     -- active|orphan|trash
  created_at INTEGER NOT NULL);

CREATE TABLE sync_state (key TEXT PRIMARY KEY, value TEXT NOT NULL);
-- 初始行: schema_version, revision(=0), device_id(8B hex), last_backup_at
```

- `PRAGMA journal_mode=WAL; foreign_keys=ON; busy_timeout=5000; user_version=1`(按版本迁移)。
- **revision**:每个写事务内 +1,单调计数器,LWW 依据(不信任时钟)。
- 软删除:查询一律 `WHERE deleted_at IS NULL`;搜索用 `name/note LIKE '%q%'`(FTS5 留作增强)。
- 用户自定义数据:加密时间/备注/缩略图(`thumbnails` 表,最长边 512px、上限 128KB)存于索引库,随加密备份同步;`user_meta` 为 JSON 扩展位。
- `SnapshotTo(path)` = `VACUUM INTO`(一致性快照);`ReplaceWith(path)` = 关库→旧库归档→tmp+rename 原子替换→重开。

### 索引云备份(internal/backup)

- `BackupNow`:`SnapshotTo(tmp)` → blob 格式加密(填 mtime/revision/deviceID)→ PUT index.enc(备注/缩略图随库同步)。触发:变更后防抖 30s / 手动 / 退出前。
- `PullRemote`:GET → 读 header 的 revision/deviceID 与本地比较:remote>local → 替换(旧库归档到 `backups/index-<rev>-<ts>.db`);相等 → noop;remote<local → 提示推送;device 不同互有领先 → LWW 取高者,败方归档,UI 明示。
- 明确不做行级 merge:面向单用户、同时单写者;落后方改动归档保留而非合并。

## 传输管线(internal/transfer)

```go
func NewManager(d Deps) *Manager // Deps{Remote, DB, MK func, Settings func, Emit func}
func (m *Manager) UploadPaths(ctx, paths []string, destFolderID int64) error
func (m *Manager) DownloadTo(ctx, fileIDs []int64, destDir string) error
func (m *Manager) Cancel(id string) bool
func (m *Manager) Snapshot() []Transfer
type Transfer struct{ ID, Kind, Name, Phase string; BytesDone, BytesTotal int64; Err string; ... }
```

- **上传**:stat → 同名自动 "(1)" 递增 → 流式加密到临时文件(`os.TempDir()/kist/`)→ 整文件 PUT(重试,取消点在退避与块边界)→ 同事务写 files+blobs+revision+1 → 删临时文件、发 `index:changed` → 防抖备份。文件夹用 WalkDir 展开 + `EnsureFolderPath` 逐级建索引目录。
- **下载**:查索引 → GET 到临时文件 → 流式解密到 `dest/.<name>.kistpart` → 终检通过后 rename 原子落盘。失败/取消清理临时文件。
- 并发:worker 池默认 2(可设 1–4);不做分块并行上传(保守兼容)。Manager 启动清空上次临时残留。

## CLI(cmd/kistctl,Phase 4 先于 GUI 打通链路)

标准库 flag 子命令:`config set` / `init` / `unlock` / `put <paths> --dest` / `ls` / `search` / `get <uuid> --to` / `rm` / `gc --dry-run` / `backup` / `pull`。口令一律 `--pass-stdin` 或 `KIST_PASS` 环境变量。与 GUI 共享全部 internal/*。

## Wails 层

绑定方法(app.go,错误统一 `*errs.AppError{Code,Msg}`):

```go
// 状态/配置/解锁
GetAppState() AppState; SaveWebDAVConfig(WebDAVConfig) error
TestConnection(WebDAVConfig) TestResult; CreateAccount(pass) error
Unlock(pass) (UnlockResult, error); ImportFromRemote(pass) (backup.PullResult, error)
Lock() error; ChangePassphrase(old, new) error
// 索引浏览
ListFolder(folderID int64) ([]EntryView, error)   // 0=根
Breadcrumb(folderID) ([]Crumb, error); Search(q string, limit int) ([]FileHit, error)
DeleteEntries(fileIDs, folderIDs []int64) error   // 软删+blob 标 trash
// 传输
UploadPaths(paths, destFolderID) error; DownloadTo(fileIDs, destDir) error
CancelTransfer(id) error; Transfers() []transfer.Transfer
// 设置/维护
GetSettings/SaveSettings; BackupIndexNow() (BackupInfo, error)
CleanupOrphans(dryRun bool) (CleanupReport, error)
```

事件(runtime.EventsEmit):`transfer:update`(进度,200ms 节流)/ `transfer:done|error` / `transfers:changed` / `index:changed` / `app:state` / `notify`。

前端页面(frontend/src):App.vue 按 AppState 切换;**Lock.vue** 三分支(首次向导:配置+测试连接+建口令 / 已配置:输口令解锁 / 新设备:从远端恢复);**Files.vue** 面包屑+FileTable+SearchBox(300ms 防抖)+工具栏(上传文件/文件夹、下载、删除),对话框用 wails 运行时 `OpenMultipleFilesDialog`/`OpenDirectoryDialog`;**Transfers.vue** 进度列表+取消;**Settings.vue** WebDAV 配置、并发/记住密码/自动备份、备份按钮、孤儿清理(预览→确认)、改口令、锁定。

## 实现顺序(每阶段带验证;环境策略:前期纯 Go,CLI 全流程跑通后再装 GUI 依赖)

**Phase 0 Go 环境**:`go mod init kist`(建议 `git init`);确认 go ≥ 1.24。验证:`go build ./...` 通过。

**Phase 1 crypto**:写 internal/crypto(format/keys/keyfile/blob)。验证:`go build ./... && go vet ./... && go test ./internal/crypto/ -count=1`。

**Phase 2 dav**:client/retry + httptest(x/net/webdav 内存 FS)测试。验证:`go test ./internal/dav/`。

**Phase 3 index**:schema/db/folders/files/blobs + 测试(含 revision 并发单调、Snapshot/Replace 往返)。验证:`go test ./internal/index/`。

**Phase 4 remote + transfer + kistctl + e2e**:打通无 GUI 核心链路(含图片缩略图生成)。验证:`go test ./internal/e2e/ -timeout 600s`;可选真实网盘手动跑 kistctl put/get 后 sha256sum 对比。

**Phase 5 索引备份+多设备(CLI)**:backup 包、kistctl backup/pull、LWW 归档。验证:`go test ./internal/backup/`;双 KIST_HOME 手测恢复。**CLI 全流程到此完成。**

**Phase 6 Wails 环境准备**:apt 装 GTK/WebKit、wails CLI、vue-ts 脚手架经临时目录生成后合入仓库。验证:`wails doctor` 全绿;合入后 `go test ./... -race` 仍全绿。

**Phase 7 Wails GUI**:app.go 绑定+事件+四页面 + 备份的 UI 接线(GUI 只是壳)。验证:`wails dev -tags webkit2_41` 手测;`wails build -tags webkit2_41` 产物 `build/bin/kist` 启动冒烟。

**Phase 8 收尾**:孤儿清理、Makefile(包装 `-tags webkit2_41`)、图标、README(格式规范+恢复流程)、`go test ./... -race` 全绿 + 正式构建(Linux + Windows 交叉)。

## 测试策略

- **crypto**:keyfile 往返/错口令/参数篡改;blob 往返(0、1、chunk±1、多块 ~40MB);翻改任一字节、尾部截断、交换两块、错 MK 均须失败;流式内存峰值。
- **dav**:httptest + x/net/webdav;EnsureRoot 幂等、PUT 抓包断言带 Content-Length;"前 N 次 503"中间件验证退避。
- **index/e2e**:见 Phase 3/4;e2e 覆盖 init→put(含文件夹)→ls/search→get 比对 sha→rm→gc;backup→新家目录 pull。
- **GUI**:手测清单(首次向导、上传进度与取消、下载落盘、搜索、删除、改口令、远端恢复)。

## 明确不做进本期

分块并行上传/分块 PUT(保守兼容);下载 Range 续传(格式已预留,见「后续计划」);Digest/NTLM 认证;系统 keyring(99designs/keyring 纯 Go DBus,列增强);多设备行级 merge(LWW+归档)。

## 后续计划(v0.1 后的候选增强)

已全部拆分为独立评估文档,见 [`docs/todo/`](docs/todo/)(TODO-01 ~ TODO-10,每篇含评估、设计与验收标准);立项前先在对应文件里更新评估,做完的移出并在进度看板记录。

## Critical Files

- `internal/crypto/blob.go` — 加密格式核心:BlobWriter/BlobReader、AAD、终检
- `internal/crypto/keyfile.go` — 主密钥包装/解锁/改口令,新设备恢复的根
- `internal/transfer/manager.go` — 上传/下载管线:并发、进度、取消、临时文件
- `internal/index/db.go` — SQLite 打开/迁移/revision/快照/替换,LWW 基础
- `app.go` — Wails 绑定与事件,GUI 与核心的唯一边界
