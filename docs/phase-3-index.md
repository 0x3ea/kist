# Phase 3 — SQLite 明文索引 internal/index

> 状态:已完成
> 前置:Phase 1(仅 go.mod 共享,无代码依赖,可与 Phase 2 并行)
> 产出:`internal/index/`(db.go、schema.go、folders.go、files.go、blobs.go + *_test.go)

## 目标

明文索引是虚拟目录结构的**唯一真源**:网盘上只有随机名 blob,文件名/层级/对应关系全在这里。同时为 Phase 6 的云备份提供 revision 与快照能力。

## 要做什么(任务清单)

- [x] `go get modernc.org/sqlite`(纯 Go,无 CGO)
- [x] `schema.go`:内嵌 DDL + `PRAGMA user_version` 迁移
- [x] `db.go`:Open/Close/迁移/根目录/device_id/WithTx(revision)/快照与替换
- [x] `folders.go`:路径创建、目录列表、面包屑、软删
- [x] `files.go`:插入、查询、搜索、软删
- [x] `blobs.go`:远端对象登记与状态
- [x] `thumbnails.go`:缩略图存取;files 表增 `encrypted_at` / `note` / `user_meta` 列
- [x] 全套单测

实施要点(与文档设计的差异说明):
- PRAGMA 全部挂在 DSN(`file:path?_pragma=...`)而非 Open 后 Exec——database/sql 连接池里每条连接都要生效,后者只会作用到当时那条连接
- `Revision()`/`DeviceID()` 返回 `(值, error)` 而非裸值
- `SetLastBackupAt` 直写不经 WithTx:它是本地簿记不是内容变更,若参与 revision 会造成"备份→变更→再备份"循环
- `ReplaceWith` 先 `wal_checkpoint(TRUNCATE)` 再关库,替换后清理旧 `-wal`/`-shm` 残留;失败路径尽力重开句柄
- 搜索结果的祖先过滤:一次性载入目录表,任一祖先软删则该文件不计入

## 设计说明

### Schema(首版 DDL)

```sql
CREATE TABLE folders (
  id         INTEGER PRIMARY KEY,
  uuid       TEXT    NOT NULL UNIQUE,
  parent_id  INTEGER REFERENCES folders(id) ON DELETE CASCADE,  -- 根固定 id=1, parent NULL
  name       TEXT    NOT NULL,
  created_at INTEGER NOT NULL,
  deleted_at INTEGER);
CREATE UNIQUE INDEX ux_folders_live ON folders(parent_id, name) WHERE deleted_at IS NULL;

CREATE TABLE files (
  id          INTEGER PRIMARY KEY,
  uuid        TEXT    NOT NULL UNIQUE,     -- = blob meta.fileID(hex)
  folder_id   INTEGER NOT NULL REFERENCES folders(id),
  name        TEXT    NOT NULL,            -- 明文文件名(仅本地)
  size        INTEGER NOT NULL,            -- 明文大小
  cipher_size INTEGER NOT NULL,
  sha256      TEXT    NOT NULL,            -- 明文 sha256(hex)
  chunk_size  INTEGER NOT NULL,
  blob_name   TEXT    NOT NULL UNIQUE,     -- 远端对象名(32hex)
  state       TEXT    NOT NULL DEFAULT 'ready',  -- uploading|ready|missing
  created_at  INTEGER NOT NULL,
  modified_at INTEGER NOT NULL,
  encrypted_at INTEGER,                    -- 加密完成时间(用户可见)
  uploaded_at INTEGER,                     -- 上传完成时间
  note        TEXT,                        -- 用户备注:可编辑、参与搜索
  user_meta   TEXT,                        -- 用户自定义 JSON(扩展位)
  deleted_at  INTEGER);
CREATE INDEX ix_files_folder ON files(folder_id) WHERE deleted_at IS NULL;
CREATE INDEX ix_files_name   ON files(name);

CREATE TABLE thumbnails (                  -- 图片缩略图:存于索引库,随加密备份同步到新设备
  file_id INTEGER PRIMARY KEY REFERENCES files(id) ON DELETE CASCADE,
  data    BLOB NOT NULL,                   -- ≤128KB;不透明→JPEG q80,含透明→PNG
  width   INTEGER NOT NULL, height INTEGER NOT NULL,
  mime    TEXT NOT NULL);                  -- image/jpeg | image/png

CREATE TABLE blobs (
  name       TEXT PRIMARY KEY,
  size       INTEGER NOT NULL,             -- 密文大小
  kind       TEXT    NOT NULL DEFAULT 'file',   -- file|index|keyfile
  state      TEXT    NOT NULL DEFAULT 'active', -- active|orphan|trash
  created_at INTEGER NOT NULL);

CREATE TABLE sync_state (key TEXT PRIMARY KEY, value TEXT NOT NULL);
-- 初始行: schema_version='1', revision='0', device_id='<8B hex>', last_backup_at='0'
```

连接 PRAGMA:`journal_mode=WAL`、`foreign_keys=ON`、`busy_timeout=5000`。

### 打开与迁移(db.go)

```go
type DB struct { *sql.DB; Path string }
func Open(path string) (*DB, error)
```

Open 流程:打开 → 执行 PRAGMA → 读 `user_version`,按版本顺序执行迁移(首版建全部表)→ 确保根目录行(id=1)存在 → 确保 `device_id` 已生成(8 随机字节 hex,一次生成永久保留)。

### 写事务与 revision

```go
func (db *DB) WithTx(fn func(tx *sql.Tx) error) error
```

**所有写路径必须经 WithTx**;fn 成功后在同一事务内执行 `UPDATE sync_state SET value=CAST(value AS INTEGER)+1 WHERE key='revision'` 再提交。revision 单调递增,是 Phase 5 LWW 的依据(不信任系统时钟)。

### 各文件 API

```go
// folders.go
func (db *DB) EnsureFolderPath(tx *sql.Tx, rootID int64, segments []string) (int64, error) // 逐级 get-or-insert
type Entry struct { ID int64; IsFolder bool; Name string; Size int64; ModifiedAt int64 }
func (db *DB) ListFolder(folderID int64) ([]Entry, error)   // folderID=0 表示根;软删项不出现
type Crumb struct { ID int64; Name string }
func (db *DB) FolderPath(folderID int64) ([]Crumb, error)   // 面包屑:根 → 当前
func (db *DB) SoftDeleteFolders(ids []int64) error

// files.go
type FileRow struct { /* 与表字段一一对应 */ }
func (db *DB) InsertFile(tx *sql.Tx, f FileRow) (int64, error)
func (db *DB) GetFile(id int64) (FileRow, error)
func (db *DB) GetFileByUUID(uuid string) (FileRow, error)
type FileHit struct { ID int64; Name, Path, Note string; Size, ModifiedAt int64 }
func (db *DB) Search(q string, limit int) ([]FileHit, error)  // name/note LIKE '%q%' ESCAPE,Path 拼面包屑
func (db *DB) SoftDeleteFiles(ids []int64) error
func (db *DB) SetFileState(id int64, state string) error
func (db *DB) SetNote(id int64, note string) error          // WithTx,经 revision → 触发备份
func (db *DB) SetUserMeta(id int64, metaJSON string) error   // 同上;JSON 合法性由上层保证

// thumbnails.go
func (db *DB) PutThumbnail(tx *sql.Tx, fileID int64, data []byte, w, h int, mime string) error
func (db *DB) GetThumbnail(fileID int64) (data []byte, mime string, err error)

// blobs.go
func (db *DB) RegisterBlob(tx *sql.Tx, name, kind string, size int64) error
func (db *DB) MarkBlobTrash(names []string) error
func (db *DB) ListBlobStates() (map[string]string, error)     // name → state

// sync_state 与快照
func (db *DB) Revision() uint64
func (db *DB) DeviceID() string
func (db *DB) SetLastBackupAt(unix int64) error
func (db *DB) SnapshotTo(path string) error    // VACUUM INTO —— 一致性快照,不必停库
func (db *DB) ReplaceWith(path string) error   // Close → 旧库拷贝到 backups/ → tmp+rename 原子替换 → 重开+迁移
```

- **软删除**:`deleted_at` 置时间戳,一切查询过滤 `deleted_at IS NULL`;物理清理在 Phase 8 的孤儿清理里做
- **搜索**:SQLite `LIKE` 对 ASCII 默认不区分大小写,中文关键词直接子串匹配;LIKE 通配符(`%`/`_`)需 ESCAPE 转义。FTS5 留作后续增强,本期不做
- **用户自定义数据**:加密时间(`encrypted_at`)、备注(`note`,可编辑、参与搜索)、缩略图(`thumbnails` 表)全部存在索引库——因此**随加密备份自动同步,新设备 pull 后立即可见**;`user_meta` 是自由 JSON 扩展位(本期 GUI 不做编辑界面,绑定预留)

## 预期结果

索引层独立可测:开库、建目录树、插文件、搜索、软删、快照替换,全部不依赖网络与加密。

## 验收标准

1. `go test ./internal/index/ -race -count=1` 全绿,至少覆盖:
   - Open 幂等(重复打开不重建表)、根目录 id=1 存在、device_id 持久不变
   - `EnsureFolderPath`:创建嵌套路径 / 已存在复用同 id / 同名不同层级不冲突
   - `ListFolder`/`FolderPath`:层级与面包屑正确;软删后条目消失
   - `Search`:大小写不敏感(ASCII)、中文关键词、LIKE 通配符转义、**备注内容命中**、返回完整虚拟路径
   - `PutThumbnail`/`GetThumbnail` 往返字节一致;`SetNote` 后按备注关键词可搜到;`SetUserMeta` JSON 往返
   - **并发 revision**:并发 N=8 个 WithTx 后 revision 恰为 N(单调、不丢)
   - `SnapshotTo` → `ReplaceWith` 往返:数据完全一致;被替换的旧库出现在 `backups/`
   - `PRAGMA journal_mode` 返回 `wal`
2. `go vet ./...` 通过
