package index

import (
	"database/sql"
	"fmt"
	"path"
	"strings"
)

// FileRow 与 files 表一一对应;时间戳均为 Unix 秒。
type FileRow struct {
	ID          int64
	UUID        string // = blob meta.fileID(hex)
	FolderID    int64
	Name        string
	Size        int64
	CipherSize  int64
	SHA256      string
	ChunkSize   int64
	BlobName    string
	State       string // uploading|ready|missing
	CreatedAt   int64
	ModifiedAt  int64
	EncryptedAt sql.NullInt64
	UploadedAt  sql.NullInt64
	Note        sql.NullString
	UserMeta    sql.NullString
	Pack        bool // 目录打包条目(TODO-15):明文区是 zip,get 还原成文件夹
	DeletedAt   sql.NullInt64
}

const fileColumns = `id, uuid, folder_id, name, size, cipher_size, sha256, chunk_size,
	blob_name, state, created_at, modified_at, encrypted_at, uploaded_at, note, user_meta, pack, deleted_at`

// FileHit 是搜索结果:Name/Note/Tag 任一命中,Path 为完整虚拟路径。
type FileHit struct {
	ID         int64
	Name       string
	Path       string
	Note       string
	Tags       []string
	Size       int64
	ModifiedAt int64
}

type rowScanner interface{ Scan(dest ...any) error }

func scanFile(row rowScanner) (FileRow, error) {
	var f FileRow
	err := row.Scan(&f.ID, &f.UUID, &f.FolderID, &f.Name, &f.Size, &f.CipherSize, &f.SHA256,
		&f.ChunkSize, &f.BlobName, &f.State, &f.CreatedAt, &f.ModifiedAt,
		&f.EncryptedAt, &f.UploadedAt, &f.Note, &f.UserMeta, &f.Pack, &f.DeletedAt)
	return f, err
}

// InsertFile 在事务内插入文件记录(上传管线在 PUT 成功后与 RegisterBlob 同事务调用),
// 返回新行 id。
func (db *DB) InsertFile(tx *sql.Tx, f FileRow) (int64, error) {
	if f.State == "" {
		f.State = "ready"
	}
	if f.CreatedAt == 0 {
		f.CreatedAt = now()
	}
	res, err := tx.Exec(`INSERT INTO files
		(uuid, folder_id, name, size, cipher_size, sha256, chunk_size, blob_name,
		 state, created_at, modified_at, encrypted_at, uploaded_at, note, user_meta, pack)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		f.UUID, f.FolderID, f.Name, f.Size, f.CipherSize, f.SHA256, f.ChunkSize, f.BlobName,
		f.State, f.CreatedAt, f.ModifiedAt, f.EncryptedAt, f.UploadedAt, f.Note, f.UserMeta, f.Pack)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetFile 按 id 取文件记录(含已软删的)。
func (db *DB) GetFile(id int64) (FileRow, error) {
	return scanFile(db.QueryRow(`SELECT `+fileColumns+` FROM files WHERE id = ?`, id))
}

// GetFileByUUID 按 uuid 取文件记录。
func (db *DB) GetFileByUUID(uuid string) (FileRow, error) {
	return scanFile(db.QueryRow(`SELECT `+fileColumns+` FROM files WHERE uuid = ?`, uuid))
}

// Search 在文件名、备注与 tag 中做子串匹配:ASCII 不区分大小写(SQLite LIKE
// 默认),中文直接子串;LIKE 通配符被转义为字面量。祖先目录已软删的文件不计入。
// tag 命中走 EXISTS 子查询(ix_file_tags_tag),与 SearchFolders 的目录侧对称。
func (db *DB) Search(q string, limit int) ([]FileHit, error) {
	if limit <= 0 {
		limit = 50
	}
	like := "%" + escapeLike(q) + "%"
	rows, err := db.Query(`
		SELECT id, name, folder_id, size, modified_at, COALESCE(note, '')
		FROM files
		WHERE deleted_at IS NULL
		  AND (name LIKE ? ESCAPE '\'
		       OR note LIKE ? ESCAPE '\'
		       OR EXISTS (SELECT 1 FROM file_tags ft JOIN tags t ON t.id = ft.tag_id
		                  WHERE ft.file_id = files.id AND t.name LIKE ? ESCAPE '\'))
		ORDER BY name
		LIMIT ?`, like, like, like, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	folders, err := db.loadFolderInfos()
	if err != nil {
		return nil, err
	}
	hits := []FileHit{}
	for rows.Next() {
		var h FileHit
		var folderID int64
		if err := rows.Scan(&h.ID, &h.Name, &folderID, &h.Size, &h.ModifiedAt, &h.Note); err != nil {
			return nil, err
		}
		dir, ok := virtualPath(folders, folderID)
		if !ok {
			continue // 祖先目录已软删,视为不可见
		}
		// path.Join 顺带规整拼接:根目录 dir 为 "/" 时不会产生 "//" 前缀
		h.Path = path.Join(dir, h.Name)
		h.Tags = []string{}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return hits, attachFileTags(db, hits)
}

// attachFileTags 一次批量查询回填命中文件的 tag(GUI 搜索结果展示用,
// 与 FolderHit.Tags 对称)。查询失败不致命:命中本身已成立,tag 只是补充。
func attachFileTags(db *DB, hits []FileHit) error {
	if len(hits) == 0 {
		return nil
	}
	ids := make([]any, len(hits))
	byID := make(map[int64]*FileHit, len(hits))
	for i := range hits {
		ids[i] = hits[i].ID
		byID[hits[i].ID] = &hits[i]
	}
	rows, err := db.Query(
		`SELECT ft.file_id, t.name FROM file_tags ft JOIN tags t ON t.id = ft.tag_id
		 WHERE ft.file_id IN (`+placeholders(len(hits))+`) ORDER BY t.name`, ids...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return err
		}
		if h := byID[id]; h != nil {
			h.Tags = append(h.Tags, name)
		}
	}
	return rows.Err()
}

// ---- 目录检索与移动(TODO-16)----

// FolderHit 是目录搜索/列表结果。Search 只查文件的时代,目录名不在检索面——
// 按作品名找,除非文件名恰好含作品名,否则搜不到(FolderHit 补上这一面)。
type FolderHit struct {
	ID          int64
	Name        string
	Path        string // 完整虚拟路径
	Note        string
	Tags        []string
	CoverFileID int64 // 自定义封面,0 = 未指定
}

// SearchFolders 在目录名、目录 note 与 tag 名中做子串匹配(与 Search 同风格:
// ASCII 不区分大小写,LIKE 通配符转义为字面量)。祖先已软删的目录不计入。
func (db *DB) SearchFolders(q string, limit int) ([]FolderHit, error) {
	if limit <= 0 {
		limit = 50
	}
	like := "%" + escapeLike(q) + "%"
	rows, err := db.Query(`
		SELECT id, name, COALESCE(note, ''), COALESCE(cover_file_id, 0)
		FROM folders
		WHERE deleted_at IS NULL
		  AND id != ? -- 根目录名为空串,不参与检索
		  AND (name LIKE ? ESCAPE '\'
		       OR COALESCE(note, '') LIKE ? ESCAPE '\'
		       OR EXISTS (SELECT 1 FROM folder_tags ft JOIN tags t ON t.id = ft.tag_id
		                  WHERE ft.folder_id = folders.id AND t.name LIKE ? ESCAPE '\'))
		ORDER BY name
		LIMIT ?`, rootFolderID, like, like, like, limit)
	if err != nil {
		return nil, err
	}
	hits, err := db.scanFolderHits(rows)
	if err != nil {
		return nil, err
	}
	return hits, nil
}

// ListFoldersWithMeta 列出带任一元数据(note/tag/封面引用)的活跃目录,meta list 用。
func (db *DB) ListFoldersWithMeta() ([]FolderHit, error) {
	rows, err := db.Query(`
		SELECT id, name, COALESCE(note, ''), COALESCE(cover_file_id, 0)
		FROM folders
		WHERE deleted_at IS NULL
		  AND id != ?
		  AND (COALESCE(note, '') != ''
		       OR COALESCE(cover_file_id, 0) != 0
		       OR EXISTS (SELECT 1 FROM folder_tags ft WHERE ft.folder_id = folders.id))
		ORDER BY name`, rootFolderID)
	if err != nil {
		return nil, err
	}
	return db.scanFolderHits(rows)
}

// scanFolderHits 消费目录行集:拼虚拟路径过滤软删祖先,再批量补全 tags。
// 目录规模小,tags 一次 IN 查询比逐行 EXISTS 省事。
func (db *DB) scanFolderHits(rows *sql.Rows) ([]FolderHit, error) {
	folders, err := db.loadFolderInfos()
	if err != nil {
		rows.Close()
		return nil, err
	}
	hits := []FolderHit{}
	var ids []int64
	for rows.Next() {
		var h FolderHit
		if err := rows.Scan(&h.ID, &h.Name, &h.Note, &h.CoverFileID); err != nil {
			rows.Close()
			return nil, err
		}
		dir, ok := virtualPath(folders, h.ID)
		if !ok {
			continue // 祖先目录已软删,视为不可见
		}
		h.Path = dir
		h.Tags = []string{}
		hits = append(hits, h)
		ids = append(ids, h.ID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(ids) == 0 {
		return hits, nil
	}
	trows, err := db.Query(`
		SELECT ft.folder_id, t.name FROM folder_tags ft JOIN tags t ON t.id = ft.tag_id
		WHERE ft.folder_id IN (`+placeholders(len(ids))+`)
		ORDER BY t.name`, toAny(ids)...)
	if err != nil {
		return nil, err
	}
	defer trows.Close()
	byID := map[int64][]string{}
	for trows.Next() {
		var fid int64
		var name string
		if err := trows.Scan(&fid, &name); err != nil {
			return nil, err
		}
		byID[fid] = append(byID[fid], name)
	}
	if err := trows.Err(); err != nil {
		return nil, err
	}
	for i := range hits {
		hits[i].Tags = byID[hits[i].ID]
	}
	return hits, nil
}

// MoveFiles 把文件移动到目标目录(纯索引操作,零远端流量):MoveEntries
// 的文件 only 薄包装(目录移动、环检测、空转 no-op 语义见 MoveEntries)。
func (db *DB) MoveFiles(ids []int64, destFolderID int64) error {
	return db.MoveEntries(ids, nil, destFolderID)
}

// SetNote 设置/清空备注;经 WithTx,改动会计入 revision 并触发后续备份。
func (db *DB) SetNote(id int64, note string) error {
	return db.WithTx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE files SET note = ? WHERE id = ?`, note, id)
		return err
	})
}

// RenameFile 重命名文件:文件名只存本地索引(blob 密封元数据只有 uuid,
// 网盘侧对象名是随机名),纯索引零流量,语义与 RenameFolder 一致——
//   - 同名 no-op 成功(事务外判定:不进 WithTx 就不多计 revision);
//   - 撞名报错,不自动 "(1)" 消解(显式单发动作,撞名多半是选错目标);
//   - 新名必须是单段合法名。
//
// 撞名判定复用上传侧 nameTakenTx(只查同目录文件;文件与目录同名可共存,
// 与 UniqueFileName 消解口径一致,改名不会制造上传消解的新分支)。
func (db *DB) RenameFile(fileID int64, name string) error {
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return fmt.Errorf("index: 非法文件名 %q(需为单段,不含 \"/\",非 \".\"/\"..\")", name)
	}
	var folderID int64
	var old string
	err := db.QueryRow(
		`SELECT folder_id, name FROM files WHERE id = ? AND deleted_at IS NULL`,
		fileID).Scan(&folderID, &old)
	if err != nil {
		return fmt.Errorf("index: 文件 %d 不存在: %w", fileID, err)
	}
	if name == old {
		return nil
	}
	return db.WithTx(func(tx *sql.Tx) error {
		// 撞名在事务内复核:无 (folder_id,name) 唯一索引,这里挡常规并发窗口
		taken, err := nameTakenTx(tx, folderID, name)
		if err != nil {
			return err
		}
		if taken {
			return fmt.Errorf("index: 同目录已有同名文件 %q", name)
		}
		_, err = tx.Exec(`UPDATE files SET name = ? WHERE id = ?`, name, fileID)
		return err
	})
}

// SetUserMeta 设置用户自定义 JSON;JSON 合法性由上层保证,本层只存取。
func (db *DB) SetUserMeta(id int64, metaJSON string) error {
	return db.WithTx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE files SET user_meta = ? WHERE id = ?`, metaJSON, id)
		return err
	})
}

// ---- 文件元数据(TODO-17):note 已有,这里补 tag 的读写 ----

// FileMeta 是文件的用户元数据投影:文件封面不走这里——封面就是文件自己的
// 缩略图行(thumbnails),由传输管线上传时生成或 GUI 显式导入,与目录的
// cover_file_id 引用式不同(目录自身没有字节,文件直接持有)。
type FileMeta struct {
	Note string   // 备注,参与搜索
	Tags []string // 无序去重;空切片 = 无 tag
}

// FileMetaUpdate 是文件元数据的增量写请求,指针语义与 FolderMetaUpdate 一致:
// nil = 不动,指向零值 = 清除——一个事务内原子生效,经 WithTx 计一次 revision。
type FileMetaUpdate struct {
	Note *string
	Tags []string // 非 nil 即全量覆盖(nil = 不动,空切片 = 清空)
}

// GetFileMeta 读文件元数据;文件不存在或已软删时报错。
func (db *DB) GetFileMeta(fileID int64) (FileMeta, error) {
	var m FileMeta
	var note sql.NullString
	err := db.QueryRow(
		`SELECT note FROM files WHERE id = ? AND deleted_at IS NULL`, fileID).Scan(&note)
	if err != nil {
		return m, fmt.Errorf("index: 文件 %d 不存在: %w", fileID, err)
	}
	m.Note = note.String

	rows, err := db.Query(
		`SELECT t.name FROM file_tags ft JOIN tags t ON t.id = ft.tag_id
		 WHERE ft.file_id = ? ORDER BY t.name`, fileID)
	if err != nil {
		return m, err
	}
	defer rows.Close()
	m.Tags = []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return m, err
		}
		m.Tags = append(m.Tags, name)
	}
	return m, rows.Err()
}

// UpdateFileMeta 原子更新文件元数据,写入四步与 UpdateFolderMeta 同款:
// 删旧关联、词表 get-or-create、挂关联(先 trim 去重)、清死词。
// 清死词必须 UNION 两张挂点表——只查一张会把另一形态(目录/文件)
// 仍在用的同名词误删(TODO-17 坑记录,测试先行覆盖)。
func (db *DB) UpdateFileMeta(fileID int64, u FileMetaUpdate) error {
	return db.WithTx(func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(
			`SELECT 1 FROM files WHERE id = ? AND deleted_at IS NULL`, fileID).Scan(&n); err != nil {
			return fmt.Errorf("index: 文件 %d 不存在: %w", fileID, err)
		}
		if u.Note != nil {
			if _, err := tx.Exec(`UPDATE files SET note = ? WHERE id = ?`, *u.Note, fileID); err != nil {
				return err
			}
		}
		if u.Tags != nil {
			if _, err := tx.Exec(`DELETE FROM file_tags WHERE file_id = ?`, fileID); err != nil {
				return err
			}
			seen := map[string]bool{}
			for _, name := range u.Tags {
				name = strings.TrimSpace(name)
				if name == "" || seen[name] {
					continue
				}
				seen[name] = true
				if _, err := tx.Exec(
					`INSERT INTO tags (name) VALUES (?) ON CONFLICT(name) DO NOTHING`, name); err != nil {
					return err
				}
				if _, err := tx.Exec(
					`INSERT OR IGNORE INTO file_tags (file_id, tag_id)
					 SELECT ?, id FROM tags WHERE name = ?`, fileID, name); err != nil {
					return err
				}
			}
			// 清死词:词表被两形态共享,存活判定必须看全两张挂点表
			if _, err := tx.Exec(
				`DELETE FROM tags WHERE id NOT IN (
					SELECT tag_id FROM folder_tags UNION SELECT tag_id FROM file_tags)`); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetFileState 更新文件状态(uploading|ready|missing)。
func (db *DB) SetFileState(id int64, state string) error {
	return db.WithTx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE files SET state = ? WHERE id = ?`, state, id)
		return err
	})
}

// SoftDeleteFiles 软删除文件;远端 blob 由清理流程(Phase 8)按 blobs 表处理。
func (db *DB) SoftDeleteFiles(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	return db.WithTx(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`UPDATE files SET deleted_at = ? WHERE id IN (`+placeholders(len(ids))+`) AND deleted_at IS NULL`,
			append([]any{now()}, toAny(ids)...)...)
		return err
	})
}

// UniqueFileName 在同一目录内为 name 找不冲突的名字:冲突时追加 "(1)"、"(2)"…。
// 必须在写文件记录的同一事务内调用——上传管线的 blob 名与文件名无关,
// 把重名消解推迟到索引事务,并发上传也不会撞名。
func (db *DB) UniqueFileName(tx *sql.Tx, folderID int64, name string) (string, error) {
	taken, err := nameTakenTx(tx, folderID, name)
	if err != nil {
		return "", err
	}
	if !taken {
		return name, nil
	}
	ext := ""
	if i := strings.LastIndex(name, "."); i > 0 {
		ext = name[i:]
	}
	base := strings.TrimSuffix(name, ext)
	for i := 1; i <= 9999; i++ {
		candidate := fmt.Sprintf("%s (%d)%s", base, i, ext)
		taken, err := nameTakenTx(tx, folderID, candidate)
		if err != nil {
			return "", err
		}
		if !taken {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("index: 目录 %d 内 %q 的重名消解超出上限", folderID, name)
}

func nameTakenTx(tx *sql.Tx, folderID int64, name string) (bool, error) {
	var one int
	err := tx.QueryRow(
		`SELECT 1 FROM files WHERE folder_id = ? AND name = ? AND deleted_at IS NULL LIMIT 1`,
		folderID, name).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// folderInfo 供虚拟路径拼接:一次载入全部目录(个人规模下目录数很小)。
type folderInfo struct {
	parent  sql.NullInt64
	name    string
	deleted bool
}

func (db *DB) loadFolderInfos() (map[int64]folderInfo, error) {
	rows, err := db.Query(`SELECT id, parent_id, name, deleted_at IS NOT NULL FROM folders`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[int64]folderInfo{}
	for rows.Next() {
		var id int64
		var fi folderInfo
		if err := rows.Scan(&id, &fi.parent, &fi.name, &fi.deleted); err != nil {
			return nil, err
		}
		m[id] = fi
	}
	return m, rows.Err()
}

// virtualPath 拼出目录的虚拟路径(如 "/a/b");任一祖先被软删则返回 false。
// 根目录名为空串,贡献最前导的 "/"。
func virtualPath(folders map[int64]folderInfo, id int64) (string, bool) {
	var segs []string
	cur := id
	for {
		fi, ok := folders[cur]
		if !ok || fi.deleted {
			return "", false
		}
		if fi.name != "" {
			segs = append(segs, fi.name)
		}
		if !fi.parent.Valid {
			break
		}
		cur = fi.parent.Int64
	}
	for i, j := 0, len(segs)-1; i < j; i, j = i+1, j-1 {
		segs[i], segs[j] = segs[j], segs[i]
	}
	return "/" + strings.Join(segs, "/"), true
}
