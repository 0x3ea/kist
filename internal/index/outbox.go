package index

import (
	"database/sql"
	"fmt"
)

// 出站箱相关查询与状态翻转(TODO-13:运输与记账分离)。
// files.state 用 v1 schema 预留的 'uploading',blobs.state 追加 'pending'
// 值(该列是自由 TEXT),均零 schema 迁移。

// GetFileByBlobName 按 blob 名取文件行(outbox push/verify/discard 的入口)。
func (db *DB) GetFileByBlobName(blobName string) (FileRow, error) {
	return scanFile(db.QueryRow(`SELECT `+fileColumns+` FROM files WHERE blob_name = ?`, blobName))
}

// ListUploading 列出全部待上传文件(软删的不算——它们的产物由 verify 的
// 无主清理处置)。
func (db *DB) ListUploading() ([]FileRow, error) {
	rows, err := db.Query(`SELECT ` + fileColumns + ` FROM files
		WHERE state = 'uploading' AND deleted_at IS NULL ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FileRow{}
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// MarkUploaded 收账:files 转 ready 并补 uploaded_at,blobs pending 转 active。
// 经 WithTx(计入 revision,多设备同步能感知);幂等——对已 ready 的行无操作。
func (db *DB) MarkUploaded(blobName string, at int64) error {
	return db.WithTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(
			`UPDATE files SET state = 'ready', uploaded_at = ? WHERE blob_name = ? AND state = 'uploading'`,
			at, blobName); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE blobs SET state = 'active' WHERE name = ? AND state = 'pending'`, blobName)
		return err
	})
}

// DiscardPending 回滚一笔待上传记账:硬删 files 行(thumbnails 随外键级联)
// 与 blobs 行。仅允许放弃 uploading 状态——已就绪文件必须走 rm 软删流程,
// 绝不能经此路径无声消失。
func (db *DB) DiscardPending(blobName string) error {
	return db.WithTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM files WHERE blob_name = ? AND state = 'uploading'`, blobName)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("index: %q 不是待上传对象,拒绝放弃(已就绪文件请用 rm)", blobName)
		}
		_, err = tx.Exec(`DELETE FROM blobs WHERE name = ?`, blobName)
		return err
	})
}
