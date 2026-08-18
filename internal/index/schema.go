package index

// migrations 按版本顺序排列,由 db.migrate() 按 PRAGMA user_version 逐个应用。
// 结构变更时只允许追加新脚本,不得修改历史脚本(老库要靠它们升级)。
var migrations = []string{v1Schema}

const v1Schema = `
CREATE TABLE IF NOT EXISTS folders (
  id         INTEGER PRIMARY KEY,
  uuid       TEXT    NOT NULL UNIQUE,
  parent_id  INTEGER REFERENCES folders(id) ON DELETE CASCADE, -- 根固定 id=1,parent 为 NULL
  name       TEXT    NOT NULL,
  created_at INTEGER NOT NULL,
  deleted_at INTEGER);

-- 同一父目录下的活跃目录不重名;软删后名字可复用
CREATE UNIQUE INDEX IF NOT EXISTS ux_folders_live
  ON folders(parent_id, name) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS files (
  id           INTEGER PRIMARY KEY,
  uuid         TEXT    NOT NULL UNIQUE,          -- = blob meta.fileID(hex)
  folder_id    INTEGER NOT NULL REFERENCES folders(id),
  name         TEXT    NOT NULL,                 -- 明文文件名(仅本地)
  size         INTEGER NOT NULL,                 -- 明文大小
  cipher_size  INTEGER NOT NULL,
  sha256       TEXT    NOT NULL,                 -- 明文 sha256(hex)
  chunk_size   INTEGER NOT NULL,
  blob_name    TEXT    NOT NULL UNIQUE,          -- 远端对象名(32hex)
  state        TEXT    NOT NULL DEFAULT 'ready', -- uploading|ready|missing
  created_at   INTEGER NOT NULL,
  modified_at  INTEGER NOT NULL,
  encrypted_at INTEGER,                          -- 加密完成时间(用户可见)
  uploaded_at  INTEGER,                          -- 上传完成时间
  note         TEXT,                             -- 用户备注:可编辑、参与搜索
  user_meta    TEXT,                             -- 用户自定义 JSON(扩展位)
  deleted_at   INTEGER);

CREATE INDEX IF NOT EXISTS ix_files_folder ON files(folder_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS ix_files_name   ON files(name);

-- 图片缩略图:存于索引库,随加密备份同步到新设备
CREATE TABLE IF NOT EXISTS thumbnails (
  file_id INTEGER PRIMARY KEY REFERENCES files(id) ON DELETE CASCADE,
  data    BLOB NOT NULL,      -- ≤128KB;不透明→JPEG q80,含透明→PNG
  width   INTEGER NOT NULL,
  height  INTEGER NOT NULL,
  mime    TEXT NOT NULL);     -- image/jpeg | image/png

-- 远端对象登记,用于孤儿检测与清理(Phase 8)
CREATE TABLE IF NOT EXISTS blobs (
  name       TEXT PRIMARY KEY,
  size       INTEGER NOT NULL,                -- 密文大小
  kind       TEXT    NOT NULL DEFAULT 'file', -- file|index|keyfile
  state      TEXT    NOT NULL DEFAULT 'active', -- active|orphan|trash
  created_at INTEGER NOT NULL);

CREATE TABLE IF NOT EXISTS sync_state (key TEXT PRIMARY KEY, value TEXT NOT NULL);
`
