package index

import (
	"database/sql"
	"fmt"
)

// Entry 是目录列表里的一项(目录或文件)。
type Entry struct {
	ID         int64
	IsFolder   bool
	Name       string
	Size       int64  // 目录为 0
	ModifiedAt int64  // 目录为 0
	State      string // 文件:uploading|ready|missing(TODO-13,ls 标记用);目录为空
}

// Crumb 是面包屑的一段。
type Crumb struct {
	ID   int64
	Name string // 根目录为空串
}

// EnsureFolderPath 在 rootID 下逐级 get-or-insert 目录,返回最深层目录 id。
// 空 segments 直接返回 rootID。目录段不允许 "."、".."、空串。
// 必须在事务内调用(通常与文件插入同一 WithTx,保证结构写入与 revision 一致)。
func (db *DB) EnsureFolderPath(tx *sql.Tx, rootID int64, segments []string) (int64, error) {
	cur := rootID
	for _, seg := range segments {
		if seg == "" || seg == "." || seg == ".." {
			return 0, fmt.Errorf("index: 非法目录段 %q", seg)
		}
		var id int64
		err := tx.QueryRow(
			`SELECT id FROM folders WHERE parent_id = ? AND name = ? AND deleted_at IS NULL`,
			cur, seg).Scan(&id)
		switch {
		case err == nil:
			cur = id
		case err == sql.ErrNoRows:
			u, err := newUUID()
			if err != nil {
				return 0, err
			}
			res, err := tx.Exec(
				`INSERT INTO folders (uuid, parent_id, name, created_at) VALUES (?, ?, ?, ?)`,
				u, cur, seg, now())
			if err != nil {
				return 0, err
			}
			if cur, err = res.LastInsertId(); err != nil {
				return 0, err
			}
		default:
			return 0, err
		}
	}
	return cur, nil
}

// ListFolder 列出某目录的活跃子目录与文件(目录在前,各按名称排序)。
// folderID=0 表示根目录。
func (db *DB) ListFolder(folderID int64) ([]Entry, error) {
	if folderID == 0 {
		folderID = rootFolderID
	}
	out := []Entry{}
	rows, err := db.Query(
		`SELECT id, name FROM folders WHERE parent_id = ? AND deleted_at IS NULL ORDER BY name`,
		folderID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.Name); err != nil {
			rows.Close()
			return nil, err
		}
		e.IsFolder = true
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	rows, err = db.Query(
		`SELECT id, name, size, modified_at, state FROM files
		 WHERE folder_id = ? AND deleted_at IS NULL ORDER BY name`,
		folderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.Name, &e.Size, &e.ModifiedAt, &e.State); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// FolderPath 返回从根到 folderID 的面包屑(含两端);folderID=0 视为根。
func (db *DB) FolderPath(folderID int64) ([]Crumb, error) {
	if folderID == 0 {
		folderID = rootFolderID
	}
	var chain []Crumb
	cur := folderID
	for {
		var parent sql.NullInt64
		var name string
		if err := db.QueryRow(
			`SELECT parent_id, name FROM folders WHERE id = ?`, cur).Scan(&parent, &name); err != nil {
			return nil, fmt.Errorf("index: 目录 %d 不存在: %w", cur, err)
		}
		chain = append(chain, Crumb{ID: cur, Name: name})
		if !parent.Valid {
			break
		}
		cur = parent.Int64
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain, nil
}

// ResolveFolderPath 沿 segments 逐级查找活跃目录,返回末端 id;
// 空 segments 返回根。任一段缺失即报错(用于 ls/下载前的路径解析)。
func (db *DB) ResolveFolderPath(segs []string) (int64, error) {
	cur := rootFolderID
	for _, seg := range segs {
		var id int64
		err := db.QueryRow(
			`SELECT id FROM folders WHERE parent_id = ? AND name = ? AND deleted_at IS NULL`,
			cur, seg).Scan(&id)
		if err == sql.ErrNoRows {
			return 0, fmt.Errorf("index: 目录不存在: %q", seg)
		}
		if err != nil {
			return 0, err
		}
		cur = id
	}
	return cur, nil
}

// SoftDeleteFolders 软删除目录(其下文件经查询侧过滤随之不可见)。
func (db *DB) SoftDeleteFolders(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	return db.WithTx(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`UPDATE folders SET deleted_at = ? WHERE id IN (`+placeholders(len(ids))+`) AND deleted_at IS NULL`,
			append([]any{now()}, toAny(ids)...)...)
		return err
	})
}
