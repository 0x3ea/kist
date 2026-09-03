package index

import (
	"database/sql"
	"net/url"
	"path"
	"sort"

	_ "modernc.org/sqlite" // SnapshotPaths 只读打开快照库
)

// PathEntry 是一条"活的"虚拟路径(未软删),供同步 diff 等全树比对场景。
type PathEntry struct {
	Path string // 完整虚拟路径(如 "/话术/a.mp4");目录与文件同形,以 Kind 区分
	Kind string // file|folder
	Blob string // 文件的远端对象名(内容同一性比较用);目录为空
}

// pathQueryable 是路径清单查询所需的最小接口:活库句柄与只读快照句柄同构。
type pathQueryable interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// ListLivePaths 列出本库全部活路径(目录+文件,路径字典序)。
// 祖先被软删的文件不计入(与 Search 的可见性口径一致)。
func (db *DB) ListLivePaths() ([]PathEntry, error) {
	return listLivePaths(db.DB)
}

// SnapshotPaths 打开快照库文件(只读)做同构查询,供 backup 包把远端快照
// 与本地清单做文件级比对。快照是 SnapshotTo 的产物,schema 与本代码同代,
// 只查询不迁移;mode=ro 防止比对过程意外改写快照字节。
func SnapshotPaths(dbFile string) ([]PathEntry, error) {
	q := make(url.Values)
	q.Add("mode", "ro")
	q.Add("_pragma", "busy_timeout(5000)")
	d, err := sql.Open("sqlite", "file:"+dbFile+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return listLivePaths(d)
}

// listLivePaths 一次载入目录表拼虚拟路径(沿用 loadFolderInfos 的全量先例,
// 个人规模下目录数很小),再扫活文件。目录只比存在性;文件带 blob 名,
// 重上传必换对象名,故 blob 名即内容同一性。
func listLivePaths(q pathQueryable) ([]PathEntry, error) {
	rows, err := q.Query(`SELECT id, parent_id, name, deleted_at IS NOT NULL FROM folders`)
	if err != nil {
		return nil, err
	}
	folders := map[int64]folderInfo{}
	for rows.Next() {
		var id int64
		var fi folderInfo
		if err := rows.Scan(&id, &fi.parent, &fi.name, &fi.deleted); err != nil {
			rows.Close()
			return nil, err
		}
		folders[id] = fi
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	out := []PathEntry{}
	// 活目录逐个拼路径;根(id=1,名为空,路径 "/")不作为条目
	for id, fi := range folders {
		if fi.deleted {
			continue
		}
		if p, ok := virtualPath(folders, id); ok && p != "/" {
			out = append(out, PathEntry{Path: p, Kind: "folder"})
		}
	}

	frows, err := q.Query(`SELECT folder_id, name, blob_name FROM files WHERE deleted_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer frows.Close()
	for frows.Next() {
		var folderID int64
		var name, blob string
		if err := frows.Scan(&folderID, &name, &blob); err != nil {
			return nil, err
		}
		p, ok := virtualPath(folders, folderID)
		if !ok {
			continue // 祖先软删:不可见,不参与比对
		}
		out = append(out, PathEntry{Path: path.Join(p, name), Kind: "file", Blob: blob})
	}
	if err := frows.Err(); err != nil {
		return nil, err
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Kind < out[j].Kind
	})
	return out, nil
}
