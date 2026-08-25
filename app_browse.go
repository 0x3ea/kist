package main

// app_browse.go — 索引浏览与元数据类绑定:目录导航(面包屑+条目+子树摘要一次
// 合成返回,避免前端多次往返)、搜索(文件+目录)、文件详情、缩略图、备注、
// 删除、移动与 TODO-16 的目录元数据。全部纯索引操作,不需解锁。
//
// 注意:GUI 新 DTO 一律不加 json tag(与 index 包的 PascalCase 出参风格一致);
// transfer/config 自带 tag 的类型维持原样——前端以 wailsjs 生成的 models.ts 为准。

import (
	"database/sql"
	"errors"
	"strings"

	"kist/internal/errs"
	"kist/internal/index"
	"kist/internal/transfer"
)

// FolderView 一次目录导航所需的全部数据:面包屑、条目(目录在前)、
// 每个子目录的子树摘要(FolderSummaries 批量,一次建树不重复载入——CLI ls 同款)。
type FolderView struct {
	Crumbs    []index.Crumb
	Entries   []index.Entry
	Summaries map[int64]index.FolderSummary
}

// ListFolder 列目录;folderID 0 归一化为根。空结果归一为空 slice
// (JSON 里 nil 会变 null,前端统一按数组处理)。
func (a *App) ListFolder(folderID int64) (v FolderView, err error) {
	defer a.panicGuard(&err)
	db, err := a.requireDB()
	if err != nil {
		return v, a.wrap(err)
	}
	if folderID <= 0 {
		folderID = 1 // 根固定 id=1
	}
	crumbs, err := db.FolderPath(folderID)
	if err != nil {
		return v, a.wrap(errs.From(err))
	}
	entries, err := db.ListFolder(folderID)
	if err != nil {
		return v, a.wrap(errs.From(err))
	}
	summaries := map[int64]index.FolderSummary{}
	if ids := folderIDsOf(entries); len(ids) > 0 {
		if summaries, err = db.FolderSummaries(ids); err != nil {
			return v, a.wrap(errs.From(err))
		}
	}
	if crumbs == nil {
		crumbs = []index.Crumb{}
	}
	if entries == nil {
		entries = []index.Entry{}
	}
	return FolderView{Crumbs: crumbs, Entries: entries, Summaries: summaries}, nil
}

func folderIDsOf(entries []index.Entry) []int64 {
	var ids []int64
	for _, e := range entries {
		if e.IsFolder {
			ids = append(ids, e.ID)
		}
	}
	return ids
}

// SearchView 搜索结果:目录命中在前(CLI search 先例),各自带虚拟路径。
type SearchView struct {
	Folders []index.FolderHit
	Files   []index.FileHit
}

// SearchAll 同时搜目录(名/note/tag)与文件(名/备注);limit<=0 或过大时取 100。
func (a *App) SearchAll(query string, limit int) (v SearchView, err error) {
	defer a.panicGuard(&err)
	db, err := a.requireDB()
	if err != nil {
		return v, a.wrap(err)
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	folders, err := db.SearchFolders(query, limit)
	if err != nil {
		return v, a.wrap(errs.From(err))
	}
	files, err := db.Search(query, limit)
	if err != nil {
		return v, a.wrap(errs.From(err))
	}
	if folders == nil {
		folders = []index.FolderHit{}
	}
	if files == nil {
		files = []index.FileHit{}
	}
	return SearchView{Folders: folders, Files: files}, nil
}

// FileDetail 是 FileRow 的 GUI 投影:摊平 sql.NullInt64(其 JSON 形态是
// {Int64,Valid} 对象,不可直接透传),并补全虚拟路径、缩略图有无与 tag。
type FileDetail struct {
	ID          int64
	UUID        string
	Name        string
	Path        string // 完整虚拟路径,如 /合集/第一话.zip
	Size        int64
	CipherSize  int64
	SHA256      string
	ChunkSize   int64
	BlobName    string
	State       string // ready | uploading | missing
	Pack        bool
	CreatedAt   int64 // Unix 秒
	ModifiedAt  int64
	EncryptedAt *int64 // nil = 无
	UploadedAt  *int64
	Note        string
	Tags        []string // TODO-17:详情面板展示
	HasThumb    bool
}

// FileInfo 取文件详情(详情面板数据源)。
func (a *App) FileInfo(fileID int64) (d FileDetail, err error) {
	defer a.panicGuard(&err)
	db, err := a.requireDB()
	if err != nil {
		return d, a.wrap(err)
	}
	f, err := db.GetFile(fileID)
	if err != nil {
		return d, a.wrap(errs.From(err)) // 不存在 → NOT_FOUND
	}
	path := f.Name
	if crumbs, cerr := db.FolderPath(f.FolderID); cerr == nil {
		path = virtualPath(crumbs, f.Name)
	}
	d = FileDetail{
		ID:          f.ID,
		UUID:        f.UUID,
		Name:        f.Name,
		Path:        path,
		Size:        f.Size,
		CipherSize:  f.CipherSize,
		SHA256:      f.SHA256,
		ChunkSize:   f.ChunkSize,
		BlobName:    f.BlobName,
		State:       f.State,
		Pack:        f.Pack,
		CreatedAt:   f.CreatedAt,
		ModifiedAt:  f.ModifiedAt,
		EncryptedAt: nullInt64Ptr(f.EncryptedAt),
		UploadedAt:  nullInt64Ptr(f.UploadedAt),
	}
	if f.Note.Valid {
		d.Note = f.Note.String
	}
	if m, merr := db.GetFileMeta(fileID); merr == nil {
		d.Tags = m.Tags
	} else {
		d.Tags = []string{} // 读不到不致命:详情以文件行为主
	}
	if _, _, terr := db.GetThumbnail(fileID); terr == nil {
		d.HasThumb = true
	}
	return d, nil
}

// ThumbData 缩略图字节(Wails 把 []byte 序列化为 base64,前端拼 data URL)。
type ThumbData struct {
	Data []byte
	Mime string
}

// GetThumbnail 取缩略图;无缩略图返回 NOT_FOUND(前端据此显示占位图)。
func (a *App) GetThumbnail(fileID int64) (t ThumbData, err error) {
	defer a.panicGuard(&err)
	db, err := a.requireDB()
	if err != nil {
		return t, a.wrap(err)
	}
	data, mime, err := db.GetThumbnail(fileID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return t, a.wrap(errs.New(errs.NotFound, "该文件没有缩略图"))
		}
		return t, a.wrap(errs.From(err))
	}
	return ThumbData{Data: data, Mime: mime}, nil
}

// SetNote 设置/清空文件备注(空串即清空)。
func (a *App) SetNote(fileID int64, note string) (err error) {
	defer a.panicGuard(&err)
	db, err := a.requireDB()
	if err != nil {
		return a.wrap(err)
	}
	if err := db.SetNote(fileID, note); err != nil {
		return a.wrap(errs.From(err))
	}
	a.emitIndexChanged("note")
	return nil
}

// SetUserMeta 存自定义 JSON 扩展位;合法性归上层(此处只存非空串或清空)。
func (a *App) SetUserMeta(fileID int64, metaJSON string) (err error) {
	defer a.panicGuard(&err)
	db, err := a.requireDB()
	if err != nil {
		return a.wrap(err)
	}
	if err := db.SetUserMeta(fileID, metaJSON); err != nil {
		return a.wrap(errs.From(err))
	}
	a.emitIndexChanged("meta")
	return nil
}

// DeleteEntries 软删条目:文件与 CLI rm 同款(软删+blob 标 trash,远端清理由 gc);
// 目录仅隐藏子树——内部文件的 blob 不自动标 trash,彻底清理需逐文件删除后 gc
// (GUI 不做子树展开删除的业务逻辑)。
func (a *App) DeleteEntries(fileIDs, folderIDs []int64) (err error) {
	defer a.panicGuard(&err)
	db, err := a.requireDB()
	if err != nil {
		return a.wrap(err)
	}
	if len(fileIDs) == 0 && len(folderIDs) == 0 {
		return a.wrap(errs.New(errs.BadConfig, "未选择任何条目"))
	}
	var blobNames []string
	for _, id := range fileIDs {
		f, err := db.GetFile(id)
		if err != nil {
			return a.wrap(errs.From(err))
		}
		blobNames = append(blobNames, f.BlobName)
	}
	if len(fileIDs) > 0 {
		if err := db.SoftDeleteFiles(fileIDs); err != nil {
			return a.wrap(errs.From(err))
		}
		if err := db.MarkBlobTrash(blobNames); err != nil {
			return a.wrap(errs.From(err))
		}
	}
	if len(folderIDs) > 0 {
		if err := db.SoftDeleteFolders(folderIDs); err != nil {
			return a.wrap(errs.From(err))
		}
	}
	a.emitIndexChanged("delete")
	return nil
}

// EnsureFolder 按虚拟路径逐级建目录(put --dest 同款宽容语义),返回最深 id;
// 供移动对话框解析用户输入的目标路径。
func (a *App) EnsureFolder(path string) (id int64, err error) {
	defer a.panicGuard(&err)
	db, err := a.requireDB()
	if err != nil {
		return 0, a.wrap(err)
	}
	segs, err := splitVirtualPath(path)
	if err != nil {
		return 0, a.wrap(err)
	}
	err = db.WithTx(func(tx *sql.Tx) error {
		var ierr error
		id, ierr = db.EnsureFolderPath(tx, 1, segs)
		return ierr
	})
	if err != nil {
		return 0, a.wrap(errs.From(err))
	}
	return id, nil
}

// MoveFiles 纯索引移动文件(零远端流量,TODO-16);重名由索引层 "(1)" 消解。
func (a *App) MoveFiles(fileIDs []int64, destFolderID int64) (err error) {
	defer a.panicGuard(&err)
	db, err := a.requireDB()
	if err != nil {
		return a.wrap(err)
	}
	if len(fileIDs) == 0 {
		return a.wrap(errs.New(errs.BadConfig, "未选择任何文件"))
	}
	if err := db.MoveFiles(fileIDs, destFolderID); err != nil {
		return a.wrap(errs.From(err))
	}
	a.emitIndexChanged("move")
	return nil
}

// GetFolderMeta 读目录元数据(note/tags/cover);三不原则:不继承、不合并、无告警。
func (a *App) GetFolderMeta(folderID int64) (m index.FolderMeta, err error) {
	defer a.panicGuard(&err)
	db, err := a.requireDB()
	if err != nil {
		return m, a.wrap(err)
	}
	m, err = db.GetFolderMeta(folderID)
	if err != nil {
		return m, a.wrap(errs.From(err))
	}
	if m.Tags == nil {
		m.Tags = []string{}
	}
	return m, nil
}

// UpdateFolderMeta 增量写目录元数据:Note/Cover 为 nil=不动、指向零值=清除;
// Tags 非 nil 即全量覆盖(index.FolderMetaUpdate 的指针语义原生穿透前端)。
func (a *App) UpdateFolderMeta(folderID int64, u index.FolderMetaUpdate) (err error) {
	defer a.panicGuard(&err)
	db, err := a.requireDB()
	if err != nil {
		return a.wrap(err)
	}
	if err := db.UpdateFolderMeta(folderID, u); err != nil {
		return a.wrap(errs.From(err))
	}
	a.emitIndexChanged("meta")
	return nil
}

// ---- 文件元数据(TODO-17):tag 挂文件 + 手动封面 ----

// GetFileMeta 读文件元数据(note/tags)。文件封面不经此口——封面就是文件
// 自己的缩略图,读走 GetThumbnail,写走 SetFileCover(与目录的引用式不同)。
func (a *App) GetFileMeta(fileID int64) (m index.FileMeta, err error) {
	defer a.panicGuard(&err)
	db, err := a.requireDB()
	if err != nil {
		return m, a.wrap(err)
	}
	m, err = db.GetFileMeta(fileID)
	if err != nil {
		return m, a.wrap(errs.From(err))
	}
	if m.Tags == nil {
		m.Tags = []string{}
	}
	return m, nil
}

// UpdateFileMeta 增量写文件元数据,指针语义与 UpdateFolderMeta 一致
// (Note nil=不动、空串=清除;Tags 非 nil 即全量覆盖)。
func (a *App) UpdateFileMeta(fileID int64, u index.FileMetaUpdate) (err error) {
	defer a.panicGuard(&err)
	db, err := a.requireDB()
	if err != nil {
		return a.wrap(err)
	}
	if err := db.UpdateFileMeta(fileID, u); err != nil {
		return a.wrap(errs.From(err))
	}
	a.emitIndexChanged("meta")
	return nil
}

// SetFileCover 导入本地图片作为文件封面(TODO-17):与上传缩略图同一管线
// 同一规格(transfer.MakeThumbnail)。localPath 空串 = 清除封面——文件侧
// 的清除是删 thumbnails 行,与目录侧的 cover_file_id 置 NULL 不同。
// 非图片/坏图在这里是显式用户动作:错误上抛不静默(上传管线才会忽略)。
// pack 注意:覆盖会顶掉上传时自动保存的首页缩略图,清除后不恢复。
func (a *App) SetFileCover(fileID int64, localPath string) (err error) {
	defer a.panicGuard(&err)
	db, err := a.requireDB()
	if err != nil {
		return a.wrap(err)
	}
	f, err := db.GetFile(fileID)
	if err != nil {
		return a.wrap(errs.From(err))
	}
	if f.DeletedAt.Valid {
		return a.wrap(errs.New(errs.NotFound, "文件已删除:"+f.Name))
	}
	if localPath == "" {
		if err := db.WithTx(func(tx *sql.Tx) error {
			return db.DeleteThumbnail(tx, fileID)
		}); err != nil {
			return a.wrap(errs.From(err))
		}
	} else {
		td, err := transfer.MakeThumbnail(localPath)
		if err != nil {
			return a.wrap(errs.New(errs.BadConfig, "封面导入失败:"+err.Error()))
		}
		if err := db.WithTx(func(tx *sql.Tx) error {
			return db.PutThumbnail(tx, fileID, td.Data, td.W, td.H, td.Mime)
		}); err != nil {
			return a.wrap(errs.From(err))
		}
	}
	a.emitIndexChanged("meta")
	return nil
}

// ---- 小工具 ----

// splitVirtualPath 与 cmd/kistctl 的同名函数孪生:把 "/a/b" 拆成 ["a","b"],
// 容忍空段与 "./" 前缀;".." 直接拒绝——虚拟路径从根写起,不存在向上逃逸。
func splitVirtualPath(p string) ([]string, error) {
	var segs []string
	for _, s := range strings.Split(strings.TrimSpace(p), "/") {
		switch s {
		case "", ".":
			continue
		case "..":
			return nil, errs.New(errs.BadConfig,
				"虚拟目录路径不支持 \"..\":请从根写起,如 /合集/子目录")
		default:
			segs = append(segs, s)
		}
	}
	return segs, nil
}

// virtualPath 由面包屑拼完整虚拟路径;根目录名约定为空串(首页显示"根")。
func virtualPath(crumbs []index.Crumb, name string) string {
	var b strings.Builder
	for i, c := range crumbs {
		if i > 0 { // 跳过根的空名
			b.WriteByte('/')
			b.WriteString(c.Name)
		}
	}
	b.WriteByte('/')
	b.WriteString(name)
	return b.String()
}

// nullInt64Ptr sql.NullInt64 → *int64(nil = 无值)。
func nullInt64Ptr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}
