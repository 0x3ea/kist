package index

import (
	"crypto/rand"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	_ "modernc.org/sqlite" // 注册 "sqlite" 驱动(纯 Go,无 CGO)
)

const rootFolderID int64 = 1

// DB 是明文索引库句柄;方法并发安全(WAL + busy_timeout,写者由调用方语义串行)。
type DB struct {
	*sql.DB
	Path string
}

// Open 打开(必要时创建)索引库:
// PRAGMA → 按版本迁移 → 确保根目录(id=1)→ 确保 sync_state(含 device_id)。
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// PRAGMA 必须挂在 DSN 上:database/sql 连接池的每条连接都要生效,
	// Open 之后逐条 Exec 只会作用到当时那条连接。
	// _txlock=immediate 让写事务用 BEGIN IMMEDIATE:并发写者直接排队
	// 等 busy_timeout,而不是 deferred 事务升级写锁时撞出 SQLITE_BUSY。
	q := make(url.Values)
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_txlock", "immediate")
	d, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	db := &DB{DB: d, Path: path}
	if err := db.migrate(); err != nil {
		d.Close()
		return nil, err
	}
	if err := db.ensureRoot(); err != nil {
		d.Close()
		return nil, err
	}
	if err := db.ensureSyncState(); err != nil {
		d.Close()
		return nil, err
	}
	return db, nil
}

func (db *DB) migrate() error {
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return fmt.Errorf("index: 读取 user_version: %w", err)
	}
	for i := v; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("index: 应用迁移 v%d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) ensureRoot() error {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM folders WHERE id = ?`, rootFolderID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	u, err := newUUID()
	if err != nil {
		return err
	}
	// 根目录名留空:虚拟路径拼接时根贡献 "/",面包屑里由前端自行渲染
	_, err = db.Exec(
		`INSERT INTO folders (id, uuid, parent_id, name, created_at) VALUES (?, ?, NULL, '', ?)`,
		rootFolderID, u, now())
	return err
}

func (db *DB) ensureSyncState() error {
	for _, kv := range [][2]string{
		{"schema_version", strconv.Itoa(len(migrations))},
		{"revision", "0"},
		{"last_backup_at", "0"},
	} {
		if _, err := db.Exec(`INSERT OR IGNORE INTO sync_state (key, value) VALUES (?, ?)`, kv[0], kv[1]); err != nil {
			return err
		}
	}
	// device_id 一旦生成永久保留;模拟第二台设备应使用全新的库文件,而不是重生成它
	var dev string
	err := db.QueryRow(`SELECT value FROM sync_state WHERE key = 'device_id'`).Scan(&dev)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	u, err := newDeviceID()
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO sync_state (key, value) VALUES ('device_id', ?)`, u)
	return err
}

// WithTx 是唯一的写事务入口:fn 成功后在同一事务内 revision+1 再提交。
// revision 单调递增,是索引云备份 LWW 的判定依据(不信任系统时钟)。
func (db *DB) WithTx(fn func(tx *sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(
		`UPDATE sync_state SET value = CAST(value AS INTEGER) + 1 WHERE key = 'revision'`); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Revision 返回当前索引内容版本号。
func (db *DB) Revision() (uint64, error) {
	var v string
	if err := db.QueryRow(`SELECT value FROM sync_state WHERE key = 'revision'`).Scan(&v); err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// DeviceID 返回本机标识(8 随机字节 hex),随库持久。
func (db *DB) DeviceID() (string, error) {
	var v string
	err := db.QueryRow(`SELECT value FROM sync_state WHERE key = 'device_id'`).Scan(&v)
	return v, err
}

// SetLastBackupAt 记录最近一次备份时刻。直写而不经 WithTx:
// 它是本地簿记,不是索引内容变更——若参与 revision 会造成"备份→变更→再备份"循环。
func (db *DB) SetLastBackupAt(unix int64) error {
	_, err := db.Exec(`INSERT OR REPLACE INTO sync_state (key, value) VALUES ('last_backup_at', ?)`,
		strconv.FormatInt(unix, 10))
	return err
}

// SnapshotTo 生成一致性快照(VACUUM INTO),不必停写;产物是独立的完整库文件。
func (db *DB) SnapshotTo(path string) error {
	// VACUUM INTO 要求目标不存在
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if _, err := db.Exec(`VACUUM INTO ?`, path); err != nil {
		return fmt.Errorf("index: 生成快照: %w", err)
	}
	return nil
}

// ReplaceWith 用 path 指向的库文件原子替换当前库,被替换的旧库归档到
// 同目录 backups/ 下(index-<rev>-<ts>.db)。用于从远端拉取索引后落地。
func (db *DB) ReplaceWith(path string) (err error) {
	// 先把 WAL 落盘,保证归档与替换基于完整状态
	if _, err = db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return err
	}
	rev, _ := db.Revision()
	if err = db.DB.Close(); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			// 尽力恢复句柄:无论替换进行到哪一步,路径上总有一个可用的库文件
			if nd, oerr := Open(db.Path); oerr == nil {
				db.DB = nd.DB
			}
		}
	}()

	bdir := filepath.Join(filepath.Dir(db.Path), "backups")
	if err = os.MkdirAll(bdir, 0o700); err != nil {
		return err
	}
	archive := filepath.Join(bdir, fmt.Sprintf("index-%d-%d.db", rev, time.Now().Unix()))
	if err = copyFile(db.Path, archive); err != nil {
		return err
	}
	tmp := db.Path + ".tmp"
	if err = copyFile(path, tmp); err != nil {
		return err
	}
	if err = os.Rename(tmp, db.Path); err != nil {
		return err
	}
	// 清掉旧库的 WAL/SHM 残留,避免与新主文件错配
	for _, suffix := range []string{"-wal", "-shm"} {
		if e := os.Remove(db.Path + suffix); e != nil && !os.IsNotExist(e) {
			_ = e // 清理失败不致命:下次打开会按新库重建
		}
	}
	var nd *DB
	if nd, err = Open(db.Path); err != nil {
		return err
	}
	db.DB = nd.DB
	return nil
}

// ---- 小工具 ----

// newUUID 生成 16 随机字节的 hex(与 blob meta.fileID 的 hex 形态一致)。
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b[:]), nil
}

// newDeviceID 生成 8 随机字节的 hex 设备标识。
func newDeviceID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b[:]), nil
}

func now() int64 { return time.Now().Unix() }
