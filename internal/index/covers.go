package index

import "database/sql"

// 封面出库(TODO-10):封面字节不再持有于 thumbnails.data,改为「一封面
// 一 blob」+ 轻引用。本表只存引用与标志,字节走加密 blob 管线
// (/kist/covers/ 命名空间);thumbnails 表是 legacy 只读回退,禁止新写入。
//
// 写入约定(与 files+blobs 同款不变量):
//   - PutCover/ClearCover 返回被顶掉/被删行的 blob 名(prevBlob),调用方
//     必须在同一事务里 MarkBlobTrashTx(prevBlob)——否则旧 blob 成"账上无、
//     远端有"的封面孤儿,custom 语义下 gc 永不自动删,永久悬空。
//   - 自动封面(derived)与文件记录同事务写,不额外计 revision;自定义
//     封面(custom)由调用方经 WithTx 计 revision(与 SetNote 同级)。
//   - state=uploading 的引用对外不可见:产物还在出站箱,先让 GUI 去远端
//     拉只会得到 404。

// 封面来源:derived=上传管线自动生成(可再生的派生缓存);
// custom=用户导入(不可再生的用户内容)。迁移回填行一律记 custom。
const (
	CoverDerived = "derived"
	CoverCustom  = "custom"
)

// 封面上传状态:uploading=产物在出站箱待运(引用不可见);ready=远端可取。
const (
	CoverUploading = "uploading"
	CoverReady     = "ready"
)

// CoverRow 是 covers 表一行:封面 blob 的轻引用。
type CoverRow struct {
	FileID    int64
	BlobName  string
	Size      int64 // 密文大小
	Width     int
	Height    int
	Mime      string
	Source    string // CoverDerived | CoverCustom
	State     string // CoverUploading | CoverReady
	CreatedAt int64
}

func scanCover(row interface{ Scan(dest ...any) error }) (CoverRow, error) {
	var c CoverRow
	err := row.Scan(&c.FileID, &c.BlobName, &c.Size, &c.Width, &c.Height, &c.Mime, &c.Source, &c.State, &c.CreatedAt)
	return c, err
}

const coverColumns = `file_id, blob_name, size, width, height, mime, source, state, created_at`

// PutCover 写入(或替换)文件的封面引用,返回被顶掉的旧 blob 名
// (空串 = 原本无封面)。必须在事务内调用;调用方负责对返回值同事务
// MarkBlobTrashTx,完成"引用换手、旧字节进 trash"的闭环。
func (db *DB) PutCover(tx *sql.Tx, c CoverRow) (prevBlob string, err error) {
	if c.State == "" {
		c.State = CoverReady
	}
	err = tx.QueryRow(`SELECT blob_name FROM covers WHERE file_id = ?`, c.FileID).Scan(&prevBlob)
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	_, err = tx.Exec(
		`INSERT OR REPLACE INTO covers (`+coverColumns+`) VALUES (?,?,?,?,?,?,?,?,?)`,
		c.FileID, c.BlobName, c.Size, c.Width, c.Height, c.Mime, c.Source, c.State, now())
	return prevBlob, err
}

// ClearCover 删除文件的封面引用,返回被删行的 blob 名(空串 = 原本无)。
// 语义与目录侧 cover_file_id 置 NULL 不同:文件封面是持有式引用,清引用
// 的同时旧 blob 进 trash,物理删除交给 gc。必须在事务内调用。
func (db *DB) ClearCover(tx *sql.Tx, fileID int64) (prevBlob string, err error) {
	err = tx.QueryRow(`SELECT blob_name FROM covers WHERE file_id = ?`, fileID).Scan(&prevBlob)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(`DELETE FROM covers WHERE file_id = ?`, fileID)
	return prevBlob, err
}

// GetReadyCover 取文件已就绪的封面引用;无封面或仍在出站箱挂账
// (uploading)时返回 sql.ErrNoRows——uploading 的引用不可见。
func (db *DB) GetReadyCover(fileID int64) (CoverRow, error) {
	return scanCover(db.QueryRow(
		`SELECT `+coverColumns+` FROM covers WHERE file_id = ? AND state = ?`, fileID, CoverReady))
}

// HasCover 报告文件"有没有封面":covers(ready) 或 legacy thumbnails 行
// 任一存在即为有(CLI info 展示、meta set --cover 引用校验用)。
func (db *DB) HasCover(fileID int64) (bool, error) {
	var has bool
	err := db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM covers WHERE file_id = ? AND state = ?)
		 OR EXISTS(SELECT 1 FROM thumbnails WHERE file_id = ?)`,
		fileID, CoverReady, fileID).Scan(&has)
	return has, err
}

// CoverBlobNamesOf 取一批文件的封面 blob 名(rm 软删时与文件 blob 一并
// MarkBlobTrash)。无封面的文件自然不出现在结果里。
func (db *DB) CoverBlobNamesOf(fileIDs []int64) ([]string, error) {
	out := []string{}
	if len(fileIDs) == 0 {
		return out, nil
	}
	rows, err := db.Query(
		`SELECT blob_name FROM covers WHERE file_id IN (`+placeholders(len(fileIDs))+`)`,
		toAny(fileIDs)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// ListUploadingCovers 列出全部待上传封面(outbox push/verify 的挂账来源,
// 与 ListUploading 对称)。verify 的"无主产物清理"必须把本结果并入挂账集,
// 否则封面产物会被当作无主文件误删。
func (db *DB) ListUploadingCovers() ([]CoverRow, error) {
	rows, err := db.Query(
		`SELECT `+coverColumns+` FROM covers WHERE state = ? ORDER BY created_at`, CoverUploading)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CoverRow{}
	for rows.Next() {
		c, err := scanCover(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
