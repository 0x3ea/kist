package index

// migrations 按版本顺序排列,由 db.migrate() 按 PRAGMA user_version 逐个应用。
// 结构变更时只允许追加新脚本,不得修改历史脚本(老库要靠它们升级)。
var migrations = []string{v1Schema, v2AddPack, v3FolderMeta, v4FileTags, v5Covers, v6FolderCovers}

// v5(TODO-10):封面出库——封面字节不再持有于 thumbnails.data,改为
// 「一封面一 blob」+ 轻引用。thumbnails 表保留为 legacy 只读回退
// (covers migrate 清空后闲置,历史脚本不可改,故原定义留在 v1);
// 写入方已全部迁移到 covers,靠"删掉 PutThumbnail/DeleteThumbnail"兜底。
//   - source:derived(上传自动生成,可再生的派生缓存)| custom(GUI 导入,
//     不可再生的用户内容)——gc 分叉的依据;迁移回填行一律记 custom
//     (legacy 行无法区分来源,按用户数据保守保护)。
//   - state:uploading(出站箱挂账,产物未上远端)| ready——未 ready 的
//     引用对外不可见(GetReadyCover/HasCover/summary 全部过滤),否则会去
//     远端拉一个尚不存在的对象。
const v5Covers = `
CREATE TABLE IF NOT EXISTS covers (
  file_id    INTEGER PRIMARY KEY REFERENCES files(id) ON DELETE CASCADE,
  blob_name  TEXT    NOT NULL UNIQUE,   -- 远端 /kist/covers/ 对象名(32hex)
  size       INTEGER NOT NULL,          -- 密文大小(读侧长度总校验用)
  width      INTEGER NOT NULL,
  height     INTEGER NOT NULL,
  mime       TEXT    NOT NULL,
  source     TEXT    NOT NULL,          -- derived|custom
  state      TEXT    NOT NULL,          -- uploading|ready
  created_at INTEGER NOT NULL);

CREATE INDEX IF NOT EXISTS ix_covers_state ON covers(state);`

// v6:目录封面改为持有式——目录不再引用库内文件,而是像文件封面一样
// 独立持有封面 blob(folder_covers 表,结构与 covers 逐列镜像,仅属主
// 从 files 换成 folders)。GUI「元数据」对话框从填文件 ID 改为直接导入
// 本地图片(transfer.ImportFolderCover,字节走 /kist/covers/ 命名空间)。
// 同时 DROP v3 的 cover_file_id 列:引用式机制整体退场,存量引用失效、
// 目录封面回退派生拼贴(引用目标文件仍在库里,可重新导入为封面)。
// 列上带 REFERENCES files(id) 的外键随列一并消失(已实测 DROP COLUMN
// 在 foreign_keys=ON 下合法,FK 方向是本表指出去而非被引用)。
const v6FolderCovers = `
CREATE TABLE IF NOT EXISTS folder_covers (
  folder_id  INTEGER PRIMARY KEY REFERENCES folders(id) ON DELETE CASCADE,
  blob_name  TEXT    NOT NULL UNIQUE,   -- 远端 /kist/covers/ 对象名(32hex)
  size       INTEGER NOT NULL,          -- 密文大小(读侧长度总校验用)
  width      INTEGER NOT NULL,
  height     INTEGER NOT NULL,
  mime       TEXT    NOT NULL,
  source     TEXT    NOT NULL,          -- custom(GUI 导入;目录无上传派生)
  state      TEXT    NOT NULL,          -- uploading|ready
  created_at INTEGER NOT NULL);

CREATE INDEX IF NOT EXISTS ix_folder_covers_state ON folder_covers(state);

ALTER TABLE folders DROP COLUMN cover_file_id;`

// v2(TODO-15):files.pack 标记"目录打包条目"——明文区是一个 zip,
// get 侧解压还原成文件夹。size 记量化后的明文区总长(显示值,
// 真实 origSize 以 sealedMeta 为准,不得用索引 size 推明文长度)。
const v2AddPack = `ALTER TABLE files ADD COLUMN pack INTEGER NOT NULL DEFAULT 0;`

// v3(TODO-16):目录用户元数据与 tag。
// note/user_meta 与 files 对齐;tag 走独立表:过滤是 tag 的全部意义,
// LIKE-over-JSON 撑不起检索面。
// (cover_file_id 引用式封面曾是本版一部分——目录封面指向库内文件、悬空回退
// 拼贴;v6 起改为持有式 folder_covers 并 DROP 本列,详见 v6 注释。)
const v3FolderMeta = `
ALTER TABLE folders ADD COLUMN note TEXT;
ALTER TABLE folders ADD COLUMN user_meta TEXT;
ALTER TABLE folders ADD COLUMN cover_file_id INTEGER REFERENCES files(id) ON DELETE SET NULL;

CREATE TABLE IF NOT EXISTS tags (
  id   INTEGER PRIMARY KEY,
  name TEXT NOT NULL UNIQUE);

CREATE TABLE IF NOT EXISTS folder_tags (
  folder_id INTEGER NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
  tag_id    INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
  UNIQUE(folder_id, tag_id));

-- 按 tag 反查目录(SearchFolders 的 EXISTS 子查询走这头)
CREATE INDEX IF NOT EXISTS ix_folder_tags_tag ON folder_tags(tag_id);`

// v4(TODO-17):文件 tag 挂点,镜像 folder_tags、共享 tags 词表——
// 文件形态作品(epub/mp4/直挂 pack)与目录作品同一词典,"按 tag 捞作品"
// 横跨两形态。tags 表本就 UNIQUE(name),两挂点表 get-or-create 同一词,
// 不会分家。
const v4FileTags = `
CREATE TABLE IF NOT EXISTS file_tags (
  file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  tag_id  INTEGER NOT NULL REFERENCES tags(id)   ON DELETE CASCADE,
  UNIQUE(file_id, tag_id));

-- 按 tag 反查文件(文件搜索的 EXISTS 子查询走这头)
CREATE INDEX IF NOT EXISTS ix_file_tags_tag ON file_tags(tag_id);`

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
