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

// MarkUploaded 收账:files 转 ready 并补 uploaded_at,封面引用(文件侧
// covers 与目录侧 folder_covers)翻转 uploading→ready(TODO-10,v6 补目录),
// blobs pending 转 active。经 WithTx(计入 revision,多设备同步能感知);
// 幂等——对已 ready 的行无操作。一个 blob 名只会命中 files/covers/
// folder_covers 之一(blobs.name 全局唯一),各 UPDATE 天然各管各的。
func (db *DB) MarkUploaded(blobName string, at int64) error {
	return db.WithTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(
			`UPDATE files SET state = 'ready', uploaded_at = ? WHERE blob_name = ? AND state = 'uploading'`,
			at, blobName); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`UPDATE covers SET state = ? WHERE blob_name = ? AND state = ?`,
			CoverReady, blobName, CoverUploading); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`UPDATE folder_covers SET state = ? WHERE blob_name = ? AND state = ?`,
			CoverReady, blobName, CoverUploading); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE blobs SET state = 'active' WHERE name = ? AND state = 'pending'`, blobName)
		return err
	})
}

// DiscardPending 回滚一笔待上传记账:"一笔"= 文件及其封面(TODO-10)。
// 硬删 files 行(封面引用随外键级联)与对应 blobs 行;返回被一并放弃的
// 封面 blob 名(调用方据此清出站箱产物)。仅允许放弃 uploading 状态——
// 已就绪文件必须走 rm 软删流程,绝不能经此路径无声消失。
// 单独放弃一笔封面挂账(GUI 导入回退、文件已 ready)也走这里:按 blob 名
// 删 covers 行,文件行不受影响。
func (db *DB) DiscardPending(blobName string) (coverBlobs []string, err error) {
	err = db.WithTx(func(tx *sql.Tx) error {
		f, ferr := scanFile(tx.QueryRow(
			`SELECT `+fileColumns+` FROM files WHERE blob_name = ?`, blobName))
		isFile := ferr == nil
		if ferr != nil && ferr != sql.ErrNoRows {
			return ferr
		}
		if isFile && f.State != "uploading" {
			return fmt.Errorf("index: %q 不是待上传对象,拒绝放弃(已就绪文件请用 rm)", blobName)
		}

		if isFile {
			// 文件账:收齐该文件的封面 blob 名后一并放弃(引用行随下面的
			// files 行删除级联消失,blobs 行要显式删)
			rows, err := tx.Query(`SELECT blob_name FROM covers WHERE file_id = ?`, f.ID)
			if err != nil {
				return err
			}
			for rows.Next() {
				var name string
				if err := rows.Scan(&name); err != nil {
					rows.Close()
					return err
				}
				coverBlobs = append(coverBlobs, name)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return err
			}
			rows.Close()
			res, err := tx.Exec(`DELETE FROM files WHERE id = ? AND state = 'uploading'`, f.ID)
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil {
				return err
			} else if n == 0 {
				return fmt.Errorf("index: %q 不是待上传对象,拒绝放弃(已就绪文件请用 rm)", blobName)
			}
			for _, name := range coverBlobs {
				if _, err := tx.Exec(`DELETE FROM blobs WHERE name = ?`, name); err != nil {
					return err
				}
			}
		} else {
			// 单独的封面账:按 blob 名删引用行(文件封面或目录封面),
			// 属主行(文件/目录)不动
			res, err := tx.Exec(
				`DELETE FROM covers WHERE blob_name = ? AND state = ?`, blobName, CoverUploading)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if n == 0 {
				res, err = tx.Exec(
					`DELETE FROM folder_covers WHERE blob_name = ? AND state = ?`, blobName, CoverUploading)
				if err != nil {
					return err
				}
				if n, err = res.RowsAffected(); err != nil {
					return err
				} else if n == 0 {
					return fmt.Errorf("index: %q 不是待上传对象,拒绝放弃(已就绪文件请用 rm)", blobName)
				}
			}
		}
		_, err := tx.Exec(`DELETE FROM blobs WHERE name = ?`, blobName)
		return err
	})
	return coverBlobs, err
}
