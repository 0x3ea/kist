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
