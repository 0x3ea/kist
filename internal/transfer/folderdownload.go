package transfer

// 文件夹下载:虚拟目录整棵子树按结构还原到本地(嵌套目录与空目录都建,
// pack 条目沿用解压还原)。分两步走——
//  1. planFolderTree 纯规划:枚举子树(index.FolderSubtree)、消解本地根名、
//     算每文件的落盘目录与任务显示名,零 FS 写入;
//  2. DownloadEntriesTo 统一 mkdir 后逐文件入队,复用 runDownload 单文件
//     管线(它只认 job.destDir 与文件名,解压/取消清理自动继承)。
//
// 任务显示名用子树内相对路径:索引内名字一律单段(拒 "/"),含 "/" 的
// Name 不可能与真实文件名混淆;几百个同 basename 的传输列表才分得清。
// 入队顺序铁律与空目录语义见 DownloadEntriesTo。

import (
	"context"
	"path/filepath"
	"time"

	"kist/internal/errs"
	"kist/internal/index"
)

// downloadSpec 是一次下载任务的入队描述(单文件与子树文件共用)。
type downloadSpec struct {
	file    index.FileRow
	destDir string // 映射后的本地目录(子树内 = rootLocal + RelDir)
	name    string // Transfer.Name:单文件 = 文件名;子树内 = "<根名>/<相对路径>"
}

// folderTree 是一个待下载目录的规划产物。
type folderTree struct {
	rootName  string   // 消解后的本地根目录名(与虚拟名差 " (n)" 后缀时以此为准)
	rootLocal string   // 本地根目录路径
	dirs      []string // 相对 rootLocal 的全部子目录(含空目录、含只剩 uploading 的目录)
	specs     []downloadSpec
	skipped   int // state=uploading 被跳过的文件数(其所在目录照建)
}

// planFolderTree 规划 folderID 子树的下载落盘。taken 跨调用累积:同批的
// 兄弟根目录尚未落盘、文件系统看不见,规划期用集合占位消解撞名(命中后
// 由此函数记入)。destDir 需已存在(DownloadEntriesTo 先建)。
func (m *Manager) planFolderTree(folderID int64, destDir string, taken map[string]bool) (folderTree, error) {
	st, err := m.deps.DB.FolderSubtree(folderID)
	if err != nil {
		return folderTree{}, errs.From(err)
	}
	if st.RootName == "" {
		return folderTree{}, errs.New(errs.BadConfig, "根目录不可整树下载(整树导出待做);请指定子目录")
	}
	rootName := uniqueLocalNameTaken(destDir, st.RootName, taken)
	taken[rootName] = true
	ft := folderTree{
		rootName:  rootName,
		rootLocal: filepath.Join(destDir, rootName),
	}
	for _, sf := range st.Folders {
		ft.dirs = append(ft.dirs, filepath.FromSlash(sf.RelPath))
	}
	for _, sf := range st.Files {
		// 待上传对象远端必然 404,规划期跳过并计数(所在目录照建)
		if sf.File.State == "uploading" {
			ft.skipped++
			continue
		}
		dir, disp := ft.rootLocal, rootName
		if sf.RelDir != "" {
			dir = filepath.Join(ft.rootLocal, filepath.FromSlash(sf.RelDir))
			disp = rootName + "/" + sf.RelDir
		}
		ft.specs = append(ft.specs, downloadSpec{
			file:    sf.File,
			destDir: dir,
			name:    disp + "/" + sf.File.Name,
		})
	}
	return ft, nil
}

// enqueueDownloadSpec 入队一次下载任务。name 允许是子树相对路径(纯显示用)。
func (m *Manager) enqueueDownloadSpec(ctx context.Context, s downloadSpec, keepZip bool) {
	jctx, cancel := context.WithCancel(ctx)
	id := newHexID()
	j := &job{ctx: jctx, cancel: cancel, file: s.file, destDir: s.destDir, keepZip: keepZip}
	j.tr = &Transfer{ID: id, Kind: "download", Name: s.name, UUID: s.file.UUID, Phase: PhaseQueued,
		BytesTotal: s.file.Size + s.file.CipherSize, StartedAt: time.Now().Unix()}
	m.add(j)
}
