package index

import "database/sql"

// RegisterBlob 在事务内登记一个远端对象(与 InsertFile 同事务,
// 保证"索引有记录 ⟺ 对象已登记")。
func (db *DB) RegisterBlob(tx *sql.Tx, name, kind string, size int64) error {
	_, err := tx.Exec(
		`INSERT OR REPLACE INTO blobs (name, size, kind, state, created_at) VALUES (?,?,?,'active',?)`,
		name, size, kind, now())
	return err
}

// RegisterBlobPending 在事务内登记一个"尚未上传"的对象(TODO-13 defer 路径,
// 与 InsertFile(state=uploading) 同事务);push/verify 成功后由 MarkUploaded 转 active。
func (db *DB) RegisterBlobPending(tx *sql.Tx, name, kind string, size int64) error {
	_, err := tx.Exec(
		`INSERT OR REPLACE INTO blobs (name, size, kind, state, created_at) VALUES (?,?,?,'pending',?)`,
		name, size, kind, now())
	return err
}

// MarkBlobTrash 把对象标记为待清理;物理删除由孤儿清理流程确认远端状态后执行。
func (db *DB) MarkBlobTrash(names []string) error {
	if len(names) == 0 {
		return nil
	}
	return db.WithTx(func(tx *sql.Tx) error {
		return db.MarkBlobTrashTx(tx, names)
	})
}

// MarkBlobTrashTx 是 MarkBlobTrash 的事务内版本:封面引用换手/清除时,
// 旧 blob 的 trash 必须与引用写入同一事务(PutCover/ClearCover 的 prevBlob
// 闭环),否则旧字节永久悬空。
func (db *DB) MarkBlobTrashTx(tx *sql.Tx, names []string) error {
	if len(names) == 0 {
		return nil
	}
	_, err := tx.Exec(
		`UPDATE blobs SET state = 'trash' WHERE name IN (`+placeholders(len(names))+`)`,
		toAny(names)...)
	return err
}

// BlobRecord 是 blobs 表一行的投影:gc 分命名空间删除要用 kind
// (file → /kist/,cover → /kist/covers/),kind 是唯一权威,不看名字。
type BlobRecord struct {
	Name  string
	Kind  string
	State string
}

// ListBlobRecords 返回全部已登记对象(名称、类别、状态)。
func (db *DB) ListBlobRecords() ([]BlobRecord, error) {
	rows, err := db.Query(`SELECT name, kind, state FROM blobs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BlobRecord{}
	for rows.Next() {
		var r BlobRecord
		if err := rows.Scan(&r.Name, &r.Kind, &r.State); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListBlobStates 返回全部已登记对象的 name → state 映射,
// 与远端 PROPFIND 结果做 diff 即可得到孤儿与待删清单。
func (db *DB) ListBlobStates() (map[string]string, error) {
	records, err := db.ListBlobRecords()
	if err != nil {
		return nil, err
	}
	m := make(map[string]string, len(records))
	for _, r := range records {
		m[r.Name] = r.State
	}
	return m, nil
}
