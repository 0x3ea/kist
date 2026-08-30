package index

// thumbnails 表是 legacy 只读回退(TODO-10 封面出库后):v5 起封面字节
// 不再入索引库,新封面一律走 covers 表 + blob 管线。本表仅在两种场景被读:
//   - covers migrate 尚未清空的旧库——GetLegacyThumbnail 兜底显示;
//   - 迁移命令逐行导出(transfer 层直查)。
// 写入口(PutThumbnail/DeleteThumbnail)已删除:任何新代码试图写缩略图
// 字节都会在编译期失败,这正是本文件存在的意义。

// GetLegacyThumbnail 取 legacy 缩略图;不存在时返回 sql.ErrNoRows。
func (db *DB) GetLegacyThumbnail(fileID int64) (data []byte, mime string, err error) {
	err = db.QueryRow(
		`SELECT data, mime FROM thumbnails WHERE file_id = ?`, fileID).Scan(&data, &mime)
	return
}
