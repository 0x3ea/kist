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

// FileHit 是搜索结果:Name 命中文件名或 Note 命中备注,Path 为完整虚拟路径。
type FileHit struct {
	ID         int64
	Name       string
	Path       string
	Note       string
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

// Search 在文件名与备注中做子串匹配:ASCII 不区分大小写(SQLite LIKE 默认),
// 中文直接子串;LIKE 通配符被转义为字面量。祖先目录已软删的文件不计入。
func (db *DB) Search(q string, limit int) ([]FileHit, error) {
	if limit <= 0 {
		limit = 50
	}
	like := "%" + escapeLike(q) + "%"
	rows, err := db.Query(`
		SELECT id, name, folder_id, size, modified_at, COALESCE(note, '')
		FROM files
		WHERE deleted_at IS NULL
		  AND (name LIKE ? ESCAPE '\' OR note LIKE ? ESCAPE '\')
		ORDER BY name
		LIMIT ?`, like, like, limit)
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
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// SetNote 设置/清空备注;经 WithTx,改动会计入 revision 并触发后续备份。
func (db *DB) SetNote(id int64, note string) error {
	return db.WithTx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE files SET note = ? WHERE id = ?`, note, id)
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
