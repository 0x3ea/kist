package index

import (
	"database/sql"
	"errors"
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

// RenameFolder 重命名目录(TODO-19):纯索引零流量——目录名与 blob 名、
// 远端布局无关,改名只动 folders.name。语义:
//   - 同名 no-op 成功(事务外判定:不进 WithTx 就不多计 revision,
//     空转不该触发一轮备份);
//   - 撞名报错,不自动 "(1)" 消解——重命名是显式单发动作,撞名多半是
//     选错目标,自动后缀会制造意外名(与 mv 的批量搬迁消解不同场景);
//   - 新名必须是单段合法目录名(EnsureFolderPath 同规则);
//   - 根(id=1)不可改:根名约定为空串,路径拼接依赖它。
func (db *DB) RenameFolder(folderID int64, name string) error {
	if folderID == rootFolderID {
		return fmt.Errorf("index: 根目录不可重命名")
	}
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return fmt.Errorf("index: 非法目录名 %q(需为单段,不含 \"/\",非 \".\"/\"..\")", name)
	}
	var parent sql.NullInt64
	var old string
	err := db.QueryRow(
		`SELECT parent_id, name FROM folders WHERE id = ? AND deleted_at IS NULL`,
		folderID).Scan(&parent, &old)
	if err != nil {
		return fmt.Errorf("index: 目录 %d 不存在: %w", folderID, err)
	}
	if name == old {
		return nil
	}
	return db.WithTx(func(tx *sql.Tx) error {
		// 撞名在事务内复核(ux_folders_live 唯一索引兜底):并发两个目录
		// 改成同名时,后进事务者在这里被拒
		var n int
		err := tx.QueryRow(
			`SELECT 1 FROM folders WHERE parent_id = ? AND name = ? AND deleted_at IS NULL AND id != ?`,
			parent, name, folderID).Scan(&n)
		if err == nil {
			return fmt.Errorf("index: 同级已有同名目录 %q", name)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.Exec(`UPDATE folders SET name = ? WHERE id = ?`, name, folderID)
		return err
	})
}

// MoveEntries 把文件与目录一起移到 destFolderID(纯索引零流量):blob 名、
// 目录 uuid 与虚拟路径无关,改挂点即可,子孙路径由查询侧派生自动跟随。
// 单事务原子生效,全部成功才计一次 revision(与 UpdateFolderMeta 同洁癖)。
// 目录侧语义:
//   - 根不可移;不存在/已软删报错;
//   - dest 在被移目录自身或其子树内 → 整批拒绝(改挂点会让目录成为自己的祖先);
//   - 选区内祖先-后代同移 → 报错(两层都改挂会脱离原层级关系,应分批);
//   - 已在 dest(目录原父即 dest / 文件现挂即 dest)→ 空转 no-op:不进事务
//     就不计 revision(同 RenameFolder 空转语义),也避免把自己当撞名占用者;
//   - 与 dest 下活跃目录撞名 → " (n)" 消解(批量搬迁语义,同文件 UniqueFileName;
//     文件与目录同名可共存,nameTakenTx 只查 files,两套消解互不干扰)。
//
// 文件侧沿用 UniqueFileName 消解;modified_at 不动——移动不是内容变更。
func (db *DB) MoveEntries(fileIDs, folderIDs []int64, destFolderID int64) error {
	if len(fileIDs) == 0 && len(folderIDs) == 0 {
		return nil
	}
	// 校验遍在事务外收齐全部问题再统一拒绝(planPackTree 同哲学);
	// 目录全量载入与搜索侧 folderInfo 同款(个人规模下目录数很小)。
	folders, err := db.loadFolderInfos()
	if err != nil {
		return err
	}
	destInfo, ok := folders[destFolderID]
	if !ok || destInfo.deleted {
		return fmt.Errorf("index: 目标目录 %d 不存在或已删除", destFolderID)
	}
	moving := make(map[int64]struct{}, len(folderIDs))
	for _, id := range folderIDs {
		if id == rootFolderID {
			return fmt.Errorf("index: 根目录不可移动")
		}
		fi, ok := folders[id]
		if !ok || fi.deleted {
			return fmt.Errorf("index: 目录 %d 不存在或已删除", id)
		}
		moving[id] = struct{}{}
	}
	// 祖先-后代同移:沿每个被移目录上溯,命中另一被移目录即拒
	for id := range moving {
		for p := folders[id].parent; p.Valid; p = folders[p.Int64].parent {
			if _, hit := moving[p.Int64]; hit {
				return fmt.Errorf("index: 选区同时含祖先与后代目录,请分批移动")
			}
		}
	}
	// 环检测:沿 dest 上溯,命中任一被移目录即 dest 落在其子树内(或即自身)
	for id := range moving {
		for cur := destFolderID; ; {
			if cur == id {
				return fmt.Errorf("index: 目标目录在被移目录 %q 自身或其子树内", folders[id].name)
			}
			p := folders[cur].parent
			if !p.Valid {
				break
			}
			cur = p.Int64
		}
	}
	// 空转过滤:已在 dest 的条目跳过;全为空转则不进事务(不计 revision)
	var moveFolders []int64
	for _, id := range folderIDs {
		if p := folders[id].parent; p.Valid && p.Int64 == destFolderID {
			continue
		}
		moveFolders = append(moveFolders, id)
	}
	var moveFiles []int64
	for _, id := range fileIDs {
		var fid int64
		if err := db.QueryRow(
			`SELECT folder_id FROM files WHERE id = ? AND deleted_at IS NULL`, id).Scan(&fid); err != nil {
			return fmt.Errorf("index: 文件 %d 不存在或已删除: %w", id, err)
		}
		if fid == destFolderID {
			continue
		}
		moveFiles = append(moveFiles, id)
	}
	if len(moveFolders) == 0 && len(moveFiles) == 0 {
		return nil
	}
	return db.WithTx(func(tx *sql.Tx) error {
		for _, id := range moveFolders {
			newName, err := uniqueFolderNameTx(tx, destFolderID, folders[id].name)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(
				`UPDATE folders SET parent_id = ?, name = ? WHERE id = ?`, destFolderID, newName, id); err != nil {
				return err
			}
		}
		for _, id := range moveFiles {
			var name string
			if err := tx.QueryRow(
				`SELECT name FROM files WHERE id = ?`, id).Scan(&name); err != nil {
				return fmt.Errorf("index: 文件 %d: %w", id, err)
			}
			newName, err := db.UniqueFileName(tx, destFolderID, name)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(
				`UPDATE files SET folder_id = ?, name = ? WHERE id = ?`, destFolderID, newName, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// uniqueFolderNameTx 在 folderID 下给目录名找空位:与活跃目录撞名时追加
// " (n)" 递增(目录无扩展名概念,不做 ext 拆分),上限同 UniqueFileName。
// 只查 folders——文件与目录同名可共存,与文件侧消解互不干扰。
func uniqueFolderNameTx(tx *sql.Tx, folderID int64, name string) (string, error) {
	taken := func(name string) (bool, error) {
		var one int
		err := tx.QueryRow(
			`SELECT 1 FROM folders WHERE parent_id = ? AND name = ? AND deleted_at IS NULL LIMIT 1`,
			folderID, name).Scan(&one)
		if err == sql.ErrNoRows {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return true, nil
	}
	ok, err := taken(name)
	if err != nil || !ok {
		return name, err
	}
	for i := 1; i <= 9999; i++ {
		candidate := fmt.Sprintf("%s (%d)", name, i)
		ok, err := taken(candidate)
		if err != nil {
			return "", err
		}
		if !ok {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("index: 目录 %d 内 %q 的重名消解超出上限", folderID, name)
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

// ---- 目录元数据(TODO-16;封面 v6 起独立为 folder_covers,不经本结构)----

// FolderMeta 是目录的用户元数据:缺席不降级,只影响检索与展示。
// 元数据三不原则:不继承、不合并、无告警——父子各自持有、互不感知。
// 封面不在其列:目录封面是持有式 blob 引用(folder_covers 表),
// 读写走 cover 侧接口,与 note/tag 的纯索引更新是两条管线。
type FolderMeta struct {
	Note     string   // 备注,参与搜索
	UserMeta string   // 用户自定义 JSON(扩展位,本层只存取)
	Tags     []string // 无序去重;空切片 = 无 tag
}

// FolderMetaUpdate 是元数据的增量写请求:指针为 nil 表示该项不动,
// 指向零值表示清除——一个事务内原子生效,全部成功才计一次 revision。
type FolderMetaUpdate struct {
	Note *string
	Tags []string // 非 nil 即全量覆盖(nil = 不动,空切片 = 清空)
}

// FolderRow 是目录行的最小投影(GUI 目录封面操作的属主校验与缓存键用:
// uuid 是 covercache 的键,数字 id 多盘共库会撞)。
type FolderRow struct {
	ID        int64
	UUID      string
	Name      string
	DeletedAt sql.NullInt64
}

// GetFolder 取目录行;不存在时报错(含已软删,由调用方按 DeletedAt 分诊)。
func (db *DB) GetFolder(folderID int64) (FolderRow, error) {
	var f FolderRow
	err := db.QueryRow(
		`SELECT id, uuid, name, deleted_at FROM folders WHERE id = ?`, folderID).
		Scan(&f.ID, &f.UUID, &f.Name, &f.DeletedAt)
	if err != nil {
		return f, fmt.Errorf("index: 目录 %d 不存在: %w", folderID, err)
	}
	return f, nil
}

// GetFolderMeta 读目录元数据;目录不存在或已软删时报错。
func (db *DB) GetFolderMeta(folderID int64) (FolderMeta, error) {
	var m FolderMeta
	var note, userMeta sql.NullString
	err := db.QueryRow(
		`SELECT note, user_meta FROM folders WHERE id = ? AND deleted_at IS NULL`,
		folderID).Scan(&note, &userMeta)
	if err != nil {
		return m, fmt.Errorf("index: 目录 %d 不存在: %w", folderID, err)
	}
	m.Note, m.UserMeta = note.String, userMeta.String

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
// 删旧关联、插新关联,再把无任何挂点引用的死 tag 行清掉——
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
			// 清死 tag:没有任何挂点再引用的词不值得保留。v4 起词表被
			// folder_tags 与 file_tags 共享(TODO-17),存活判定必须 UNION
			// 两张挂点表——只查一张会误删另一形态仍在用的同名词。
			if _, err := tx.Exec(
				`DELETE FROM tags WHERE id NOT IN (
					SELECT tag_id FROM folder_tags UNION SELECT tag_id FROM file_tags)`); err != nil {
				return err
			}
		}
		return nil
	})
}
