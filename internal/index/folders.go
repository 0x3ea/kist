package index

import (
	"database/sql"
	"fmt"
	"strings"
)

// Entry 是目录列表里的一项(目录或文件)。
type Entry struct {
	ID         int64
	IsFolder   bool
	Name       string
	Size       int64  // 目录为 0
	ModifiedAt int64  // 目录为 0
	State      string // 文件:uploading|ready|missing(TODO-13,ls 标记用);目录为空
	Pack       bool   // 文件:目录打包条目(TODO-15),ls 以 P 标记
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
		`SELECT id, name, size, modified_at, state, pack FROM files
		 WHERE folder_id = ? AND deleted_at IS NULL ORDER BY name`,
		folderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.Name, &e.Size, &e.ModifiedAt, &e.State, &e.Pack); err != nil {
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

// ---- 目录元数据(TODO-16)----

// FolderMeta 是目录的用户元数据:缺席不降级,只影响检索与展示。
// 元数据三不原则:不继承、不合并、无告警——父子各自持有、互不感知。
type FolderMeta struct {
	Note        string   // 备注,参与搜索
	UserMeta    string   // 用户自定义 JSON(扩展位,本层只存取)
	CoverFileID int64    // 自定义封面指向的 files.id;0 = 未指定(走派生/默认级)
	Tags        []string // 无序去重;空切片 = 无 tag
}

// FolderMetaUpdate 是元数据的增量写请求:指针为 nil 表示该项不动,
// 指向零值表示清除——一个事务内原子生效,全部成功才计一次 revision。
type FolderMetaUpdate struct {
	Note  *string
	Cover *int64
	Tags  []string // 非 nil 即全量覆盖(nil = 不动,空切片 = 清空)
}

// GetFolderMeta 读目录元数据;目录不存在或已软删时报错。
func (db *DB) GetFolderMeta(folderID int64) (FolderMeta, error) {
	var m FolderMeta
	var note, userMeta sql.NullString
	var cover sql.NullInt64
	err := db.QueryRow(
		`SELECT note, user_meta, cover_file_id FROM folders WHERE id = ? AND deleted_at IS NULL`,
		folderID).Scan(&note, &userMeta, &cover)
	if err != nil {
		return m, fmt.Errorf("index: 目录 %d 不存在: %w", folderID, err)
	}
	m.Note, m.UserMeta = note.String, userMeta.String
	m.CoverFileID = cover.Int64

	rows, err := db.Query(
		`SELECT t.name FROM folder_tags ft JOIN tags t ON t.id = ft.tag_id
		 WHERE ft.folder_id = ? ORDER BY t.name`, folderID)
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

// UpdateFolderMeta 原子更新元数据。tag 写入走"全量覆盖":
// 删旧关联、插新关联,再把无任何目录引用的死 tag 行清掉——
// tags 表只承载有效词,搜索面与 meta list 不被残留污染。
func (db *DB) UpdateFolderMeta(folderID int64, u FolderMetaUpdate) error {
	return db.WithTx(func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(
			`SELECT 1 FROM folders WHERE id = ? AND deleted_at IS NULL`, folderID).Scan(&n); err != nil {
			return fmt.Errorf("index: 目录 %d 不存在: %w", folderID, err)
		}
		if u.Note != nil {
			if _, err := tx.Exec(`UPDATE folders SET note = ? WHERE id = ?`, *u.Note, folderID); err != nil {
				return err
			}
		}
		if u.Cover != nil {
			// Cover 指向 0 = 清除引用。列带 REFERENCES files(id),不存在
			// id=0 的行,直接写 0 会触发外键失败(Phase 7 绑定层测试发现,
			// CLI `meta set --cover 0` 同样踩雷)——清除必须落 NULL。
			v := sql.NullInt64{Int64: *u.Cover, Valid: *u.Cover != 0}
			if _, err := tx.Exec(
				`UPDATE folders SET cover_file_id = ? WHERE id = ?`, v, folderID); err != nil {
				return err
			}
		}
		if u.Tags != nil {
			if _, err := tx.Exec(`DELETE FROM folder_tags WHERE folder_id = ?`, folderID); err != nil {
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
					`INSERT OR IGNORE INTO folder_tags (folder_id, tag_id)
					 SELECT ?, id FROM tags WHERE name = ?`, folderID, name); err != nil {
					return err
				}
			}
			// 清死 tag:没有任何目录再引用的词不值得保留
			if _, err := tx.Exec(
				`DELETE FROM tags WHERE id NOT IN (SELECT DISTINCT tag_id FROM folder_tags)`); err != nil {
				return err
			}
		}
		return nil
	})
}
