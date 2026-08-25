package index

import "database/sql"

// PutThumbnail 与文件记录同事务写入(上传管线里紧随 InsertFile 调用)。
// data 为最终编码后的字节(JPEG/PNG),体积由生成方限制在 ≤128KB。
func (db *DB) PutThumbnail(tx *sql.Tx, fileID int64, data []byte, w, h int, mime string) error {
	_, err := tx.Exec(
		`INSERT OR REPLACE INTO thumbnails (file_id, data, width, height, mime) VALUES (?,?,?,?,?)`,
		fileID, data, w, h, mime)
	return err
}

// GetThumbnail 取缩略图;不存在时返回 sql.ErrNoRows。
func (db *DB) GetThumbnail(fileID int64) (data []byte, mime string, err error) {
	err = db.QueryRow(
		`SELECT data, mime FROM thumbnails WHERE file_id = ?`, fileID).Scan(&data, &mime)
	return
}

// DeleteThumbnail 删掉文件的缩略图行(GUI"清除封面",TODO-17)。
// 必须在 WithTx 内调用以计 revision——缩略图随 index.enc 同步,删除也是
// 索引内容变更。文件行自身不动:清除后该文件回落类型占位图。
// 注意与目录封面清除的语义差别:目录是 cover_file_id 置 NULL(引用式),
// 文件是删 thumbnails 行(持有式)。
func (db *DB) DeleteThumbnail(tx *sql.Tx, fileID int64) error {
	_, err := tx.Exec(`DELETE FROM thumbnails WHERE file_id = ?`, fileID)
	return err
}
