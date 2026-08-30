package transfer

// 存量封面出库迁移(TODO-10):把 v4 时代存在 thumbnails.data 里的封面
// 字节一次性导出为独立 blob(/kist/covers/ 命名空间)。
//
// revision 规则(拆放大器的关键):逐行用 raw 事务(db.Begin)记账,
// **不经 WithTx、不计 revision**——万张图迁移顶出万次 revision 正是本项
// 要消灭的放大器;结束时一次空 WithTx +1,触发一次只含轻引用的备份,
// 其他设备经 LWW 即可看到封面。逐行记账让中断随时可续跑:已迁移的行
// (covers 已有引用)自动跳过。
//
// 迁移行的 source 一律记 custom:legacy 行无法区分是上传自动生成还是
// GUI 导入,按"不可再生的用户内容"保守保护(gc 孤儿永不自动删)。

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"time"

	"kist/internal/crypto"
	"kist/internal/index"
	"kist/internal/remote"
)

// CoverMigrateResult 是迁移汇总;Failed>0 时重跑可续,已成功的行不会重传。
type CoverMigrateResult struct {
	Total    int      // 需迁移的 legacy 行数(活跃文件)
	Migrated int      // 本次成功出库
	Skipped  int      // covers 已有引用,跳过(断点续跑)
	Deleted  int      // 软删文件的 legacy 行直接清除(不迁移)
	Failed   int      // 本次失败(可重跑续传)
	Bytes    int64    // 待迁移明文字节合计(dryRun 亦返回)
	Failures []string // 失败明细(fileID:err)
}

const coverMigrateBatch = 50 // 每批 ≤50 行(≤6.4MB):不持有读游标跨写事务

// MigrateLegacyCovers 把 legacy thumbnails 逐行出库。max>0 时本次最多迁移
// max 行(分批,配合网盘限速);dryRun 只统计不动任何数据。VACUUM 在全部
// 完成后执行(事务外),回收 thumbnails.data 释放的库空间。
func MigrateLegacyCovers(ctx context.Context, store *remote.Store, db *index.DB, mk crypto.MasterKey,
	dryRun bool, max int, prog func(done, total int)) (CoverMigrateResult, error) {

	var res CoverMigrateResult
	// 需迁移 = legacy 行 且 covers 无引用 且文件未软删
	err := db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(LENGTH(t.data)), 0)
		 FROM thumbnails t
		 JOIN files f ON f.id = t.file_id AND f.deleted_at IS NULL
		 LEFT JOIN covers c ON c.file_id = t.file_id
		 WHERE c.file_id IS NULL`).Scan(&res.Total, &res.Bytes)
	if err != nil {
		return res, err
	}
	if dryRun {
		return res, nil
	}
	if res.Total > 0 {
		if err := store.EnsureCoversRoot(ctx); err != nil {
			return res, err
		}
	}

	done := 0
	lastID := int64(0)
	for res.Migrated < res.Total {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		if max > 0 && done >= max {
			break
		}
		// 按 file_id 键集分页取一批,整批读进内存后即关游标
		rows, err := db.Query(
			`SELECT t.file_id, t.data, t.width, t.height, t.mime
			 FROM thumbnails t
			 JOIN files f ON f.id = t.file_id AND f.deleted_at IS NULL
			 LEFT JOIN covers c ON c.file_id = t.file_id
			 WHERE c.file_id IS NULL AND t.file_id > ?
			 ORDER BY t.file_id LIMIT ?`, lastID, coverMigrateBatch)
		if err != nil {
			return res, err
		}
		type legacyRow struct {
			fileID int64
			data   []byte
			w, h   int
			mime   string
		}
		var batch []legacyRow
		for rows.Next() {
			var r legacyRow
			if err := rows.Scan(&r.fileID, &r.data, &r.w, &r.h, &r.mime); err != nil {
				rows.Close()
				return res, err
			}
			batch = append(batch, r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return res, err
		}
		rows.Close()
		if len(batch) == 0 {
			break
		}

		for _, r := range batch {
			lastID = r.fileID
			// 逐行重查 covers:同批内不会重复;断点续跑时此行已在 covers → 跳过
			var exists int
			if err := db.QueryRow(
				`SELECT COUNT(*) FROM covers WHERE file_id = ?`, r.fileID).Scan(&exists); err != nil {
				return res, err
			}
			if exists > 0 {
				res.Skipped++
				continue
			}
			if err := migrateOneCover(ctx, store, db, mk, r.fileID, r.data, r.w, r.h, r.mime); err != nil {
				res.Failed++
				res.Failures = append(res.Failures, fmt.Sprintf("%d:%v", r.fileID, err))
				slog.Warn("封面迁移失败(重跑可续)", "fileID", r.fileID, "err", err)
				continue
			}
			res.Migrated++
			done++
			if prog != nil {
				prog(done, res.Total)
			}
			if max > 0 && done >= max {
				break
			}
		}
	}

	// 软删文件的遗留行直接清(随文件删除语义走,不占远端对象)
	if res.Deleted, err = purgeDeletedLegacy(db); err != nil {
		return res, err
	}

	// 结束一次空 WithTx:+1 revision 触发备份传播;无变更则不动
	if res.Migrated > 0 || res.Deleted > 0 {
		if err := db.WithTx(func(tx *sql.Tx) error { return nil }); err != nil {
			return res, err
		}
		if err := vacuum(db); err != nil {
			slog.Warn("VACUUM 失败(空间未回收,不影响正确性)", "err", err)
		}
	}
	return res, nil
}

// migrateOneCover 迁移单行:加密 → PUT covers 命名空间 → raw 事务记账
// (covers 引用 custom/ready + blobs 登记 + 删 legacy 行)。记账不经
// WithTx:迁移是数据搬运,不是索引内容编辑,不逐行计 revision。
func migrateOneCover(ctx context.Context, store *remote.Store, db *index.DB, mk crypto.MasterKey,
	fileID int64, data []byte, w, h int, mime string) error {

	name := newHexID()
	tmp, err := os.CreateTemp("", "kist-covmig-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	cw, err := crypto.NewBlobWriter(tmp, mk, crypto.EncryptOptions{})
	if err != nil {
		tmp.Close()
		return err
	}
	if _, err := cw.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := cw.Close(); err != nil {
		tmp.Close()
		return err
	}
	// covers.size 存密文总长(读侧 NewBlobReader 的长度校验参数,与上传
	// 路径的 stat 口径一致);meta.OrigSize 是明文长,二者在 v2 下不等
	st, err := tmp.Stat()
	if err != nil {
		tmp.Close()
		return err
	}
	cipherSize := st.Size()
	if _, err := tmp.Seek(0, 0); err != nil {
		tmp.Close()
		return err
	}
	if err := store.PutCoverBlob(ctx, name, tmp, nil); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := db.PutCover(tx, index.CoverRow{
		FileID: fileID, BlobName: name, Size: cipherSize,
		Width: w, Height: h, Mime: mime,
		Source: index.CoverCustom, State: index.CoverReady, CreatedAt: time.Now().Unix(),
	}); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO blobs (name, size, kind, state, created_at) VALUES (?,?,'cover','active',?)`,
		name, cipherSize, time.Now().Unix()); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM thumbnails WHERE file_id = ?`, fileID); err != nil {
		return err
	}
	return tx.Commit()
}

// purgeDeletedLegacy 清掉无需迁移的 legacy 缩略图行:软删(或悬空)文件的
// ——文件本体已按删除语义处置,封面字节随之消亡;已有 covers 引用的
// ——迁移完成后 legacy 表应清空,只留历史 schema 占位。
func purgeDeletedLegacy(db *index.DB) (int, error) {
	res, err := db.Exec(
		`DELETE FROM thumbnails WHERE file_id IN (
		   SELECT t.file_id FROM thumbnails t
		   LEFT JOIN files f ON f.id = t.file_id
		   WHERE f.id IS NULL OR f.deleted_at IS NOT NULL)
		 OR file_id IN (SELECT file_id FROM covers)`)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// vacuum 回收库空间;WAL 下主文件收缩还需 checkpoint 截断。
// 二者都必须在事务外执行;GUI 同开一库时可能 SQLITE_BUSY,只 Warn 不致命。
func vacuum(db *index.DB) error {
	if _, err := db.Exec(`VACUUM`); err != nil {
		return err
	}
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return err
	}
	return nil
}
