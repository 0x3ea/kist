package index

// 子树枚举(文件夹下载的前置):把 folderID 整棵活跃子树平铺成"相对路径
// 已算好"的投影。树结构知识(软删语义、路径拼装)只在这里实现一次,下载侧
// 只做路径映射与入队,不必再懂目录树。

import "sort"

// SubtreeFolder 是子树内一个活跃目录(不含子树根本身——根由 RootID/RootName
// 承载,消费方无需再过滤)。
type SubtreeFolder struct {
	ID      int64
	RelPath string // 相对子树根的路径,段间 "/",如 "作品A/第01话"
}

// SubtreeFile 是子树内一个活跃文件。File 为全行:BlobName/CipherSize/UUID
// 下载要用,State(含 uploading)照常返回——跳不跳过是上层的业务取舍,
// 索引保持"结构真实"。
type SubtreeFile struct {
	RelDir string  // 所属目录相对子树根的路径,根本身为 ""
	File   FileRow // 全行
}

// Subtree 是整棵子树的平铺投影:Folders/Files 均为父前序、同级名称序,
// 与 ListFolder 的浏览顺序同口径。
type Subtree struct {
	RootID   int64
	RootName string          // 根目录名;子树根为根目录(id=1)时为空串
	Folders  []SubtreeFolder // 不含根本身
	Files    []SubtreeFile
}

// FolderSubtree 枚举 folderID(0 视为根)整棵活跃子树。纯查询,不进
// WithTx、不计 revision。实现沿用 loadFolderInfos 全量载入 + 内存遍历的
// 先例(summary/MoveEntries/ListLivePaths,个人规模毫秒级,不用递归 CTE):
// 软删目录整支剪除(其子孙与文件一并不出现);同名"软删目录+活跃目录"
// 共存时只取活跃者(ux_folders_live 允许共存)。
func (db *DB) FolderSubtree(folderID int64) (Subtree, error) {
	folderID = normFolderID(folderID)
	infos, err := db.loadFolderInfos()
	if err != nil {
		return Subtree{}, err
	}
	rootInfo, ok := infos[folderID]
	if !ok || rootInfo.deleted {
		if folderID == rootFolderID {
			return Subtree{}, errNoRoot
		}
		return Subtree{}, errFolderInvisible
	}

	// 活文件一把全查、按挂点分桶:子树外的桶不消费即弃,
	// 软删目录挂的文件因目录不可达自然落网外。
	byFolder := map[int64][]FileRow{}
	rows, err := db.Query(`SELECT ` + fileColumns + ` FROM files WHERE deleted_at IS NULL`)
	if err != nil {
		return Subtree{}, err
	}
	defer rows.Close()
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return Subtree{}, err
		}
		byFolder[f.FolderID] = append(byFolder[f.FolderID], f)
	}
	if err := rows.Err(); err != nil {
		return Subtree{}, err
	}

	// 活跃子目录按父分桶、同级名称序;软删目录不入桶
	children := map[int64][]int64{}
	for id, fi := range infos {
		if fi.deleted || !fi.parent.Valid {
			continue
		}
		children[fi.parent.Int64] = append(children[fi.parent.Int64], id)
	}
	for p := range children {
		c := children[p]
		sort.Slice(c, func(i, j int) bool { return infos[c[i]].name < infos[c[j]].name })
	}

	st := Subtree{
		RootID:   folderID,
		RootName: rootInfo.name,
		Folders:  []SubtreeFolder{},
		Files:    []SubtreeFile{},
	}
	seen := map[int64]bool{folderID: true} // 环防御:正常库自根不可达成环,脏数据兜底
	var visit func(id int64, rel string)
	visit = func(id int64, rel string) {
		files := byFolder[id]
		sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
		for _, f := range files {
			st.Files = append(st.Files, SubtreeFile{RelDir: rel, File: f})
		}
		for _, c := range children[id] {
			if seen[c] {
				continue
			}
			seen[c] = true
			crel := infos[c].name
			if rel != "" {
				crel = rel + "/" + crel
			}
			st.Folders = append(st.Folders, SubtreeFolder{ID: c, RelPath: crel})
			visit(c, crel)
		}
	}
	visit(folderID, "")
	return st, nil
}
