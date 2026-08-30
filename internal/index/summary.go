package index

// 子树聚合视图(TODO-16):浏览的决策单元是作品(目录),不是话(文件)。
// FolderSummary 只报告结构涌现的计数,领域措辞(话/专辑/相册)属于 CLI/GUI 壳层;
// 元数据永不聚合(三不原则)——计数可以自下而上汇总,tag/作者属于目录自身。

import (
	"errors"
	"sort"
)

var (
	errNoRoot          = errors.New("index: 根目录缺失(库损坏)")
	errFolderInvisible = errors.New("index: 目录不存在或已删除")
)

// FolderSummary 是一个目录整棵子树的递归聚合,纯查询零维护:
// 任何写入只 bump revision,下次查询重新算(个人规模全量内存毫秒级)。
type FolderSummary struct {
	PackCount    int     // 子树内 pack 条目数(漫画语境 = 话数)
	FileCount    int     // 子树内活跃文件总数(含 pack)
	TotalSize    int64   // 子树内明文大小合计
	LatestAt     int64   // 子树内最新 modified_at,空子树为 0
	PendingCount int     // 子树内 state=uploading 的文件数(出站箱待推,呼应 TODO-13)
	CoverFileIDs []int64 // 封面三级回退链解析结果,≤4;0 = 该格渲染默认占位
}

// sumTree 是一次聚合查询的全部上下文:目录树、活跃文件索引、封面占有集。
type sumTree struct {
	root   *sumNode
	byID   map[int64]*sumNode  // 活跃目录索引(含根)
	files  map[int64]*fileLite // 活跃文件索引(封面引用悬空判定用)
	covers map[int64]bool      // 有封面的 fileID 集(ready covers ∪ legacy thumbnails)
}

// sumNode 是聚合用的内存树节点:只含活跃目录;祖先被软删的整支不参与
// (与 virtualPath 的"任一祖先软删即不可见"同语义)。
type sumNode struct {
	id       int64
	name     string
	cover    int64      // folders.cover_file_id,0 = 未设置
	children []*sumNode // 活跃子目录,名称序
	files    []fileLite // 活跃文件,名称序
}

type fileLite struct {
	id       int64
	name     string
	size     int64
	pack     bool
	state    string
	modified int64
}

// buildTree 一次载入目录与文件建内存树(沿用 loadFolderInfos 的全量先例,
// 不用递归 CTE)。个人规模下目录与文件数都很小,毫秒级。
func (db *DB) buildTree() (*sumTree, error) {
	folders, err := db.loadFolderInfos()
	if err != nil {
		return nil, err
	}
	// 封面引用原值一并取回(NULL → 0 = 未设置)
	coverRows, err := db.Query(`SELECT id, COALESCE(cover_file_id, 0) FROM folders`)
	if err != nil {
		return nil, err
	}
	covers := map[int64]int64{}
	for coverRows.Next() {
		var id, c int64
		if err := coverRows.Scan(&id, &c); err != nil {
			coverRows.Close()
			return nil, err
		}
		covers[id] = c
	}
	if err := coverRows.Err(); err != nil {
		coverRows.Close()
		return nil, err
	}
	coverRows.Close()

	// 第一遍:为每个活跃目录建节点;父被软删的目录不挂树,整支不可见
	nodes := map[int64]*sumNode{}
	for id, fi := range folders {
		if fi.deleted {
			continue
		}
		nodes[id] = &sumNode{id: id, name: fi.name, cover: covers[id]}
	}
	root := nodes[rootFolderID]
	if root == nil {
		return nil, errNoRoot
	}
	for id, n := range nodes {
		if id == rootFolderID {
			continue // 根的 parent 为 NULL,不跳过会把自己挂成自己的孩子——聚合无限递归(测试先行抓出)
		}
		fi := folders[id]
		if p := nodes[fi.parent.Int64]; p != nil {
			p.children = append(p.children, n)
		}
	}

	// 第二遍:挂文件。目录不在活跃树上的文件(祖先软删)直接丢弃
	rows, err := db.Query(
		`SELECT id, folder_id, name, size, state, pack, modified_at FROM files WHERE deleted_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	fileIndex := map[int64]*fileLite{}
	for rows.Next() {
		var f fileLite
		var folderID int64
		if err := rows.Scan(&f.id, &folderID, &f.name, &f.size, &f.state, &f.pack, &f.modified); err != nil {
			return nil, err
		}
		if n := nodes[folderID]; n != nil {
			n.files = append(n.files, f)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 名称自然序:目录与文件各排各的,混合序在封面链里现场归并
	for _, n := range nodes {
		sort.Slice(n.files, func(i, j int) bool { return n.files[i].name < n.files[j].name })
		for i := range n.files {
			fileIndex[n.files[i].id] = &n.files[i]
		}
		sort.Slice(n.children, func(i, j int) bool { return n.children[i].name < n.children[j].name })
	}

	coverRows2, err := db.Query(
		`SELECT file_id FROM covers WHERE state = 'ready'
		 UNION
		 SELECT file_id FROM thumbnails`)
	if err != nil {
		return nil, err
	}
	defer coverRows2.Close()
	coverSet := map[int64]bool{}
	for coverRows2.Next() {
		var id int64
		if err := coverRows2.Scan(&id); err != nil {
			return nil, err
		}
		coverSet[id] = true
	}
	if err := coverRows2.Err(); err != nil {
		return nil, err
	}
	return &sumTree{root: root, byID: nodes, files: fileIndex, covers: coverSet}, nil
}

// agg 是自底向上的纯计数;封面链独立于计数单独解析。
type agg struct {
	pack, files, pending int
	size, latest         int64
}

func (a *agg) addFile(f fileLite) {
	a.files++
	if f.pack {
		a.pack++
	}
	a.size += f.size
	if f.modified > a.latest {
		a.latest = f.modified
	}
	if f.state == "uploading" {
		a.pending++
	}
}

// aggregate 后序聚合子树计数,memo 记忆化保证每节点只算一次。
func (t *sumTree) aggregate(n *sumNode, memo map[int64]*agg) *agg {
	if a, ok := memo[n.id]; ok {
		return a
	}
	a := &agg{}
	for i := range n.files {
		a.addFile(n.files[i])
	}
	for _, c := range n.children {
		ca := t.aggregate(c, memo)
		a.pack += ca.pack
		a.files += ca.files
		a.pending += ca.pending
		a.size += ca.size
		if ca.latest > a.latest {
			a.latest = ca.latest
		}
	}
	memo[n.id] = a
	return a
}

// FolderSummary 聚合 folderID 整棵子树;folderID=0 视为根。
// 目录不存在或任一祖先已软删时返回错误(不可见的东西没有摘要)。
func (db *DB) FolderSummary(folderID int64) (FolderSummary, error) {
	m, err := db.FolderSummaries([]int64{folderID})
	if err != nil {
		return FolderSummary{}, err
	}
	s, ok := m[normFolderID(folderID)]
	if !ok {
		return FolderSummary{}, errFolderInvisible
	}
	return s, nil
}

// FolderSummaries 批量聚合(ls 列目录、GUI 列卡片时一次建树逐个取,不重复载入)。
// 结果只含可见目录;输入里的不可见 id 直接缺席,调用方以缺失判定。
func (db *DB) FolderSummaries(ids []int64) (map[int64]FolderSummary, error) {
	t, err := db.buildTree()
	if err != nil {
		return nil, err
	}
	aggMemo := map[int64]*agg{}
	coverMemo := map[int64][]int64{}
	repMemo := map[int64]int64{}
	out := map[int64]FolderSummary{}
	for _, id := range ids {
		n := t.byID[normFolderID(id)]
		if n == nil {
			continue
		}
		a := t.aggregate(n, aggMemo)
		out[n.id] = FolderSummary{
			PackCount:    a.pack,
			FileCount:    a.files,
			TotalSize:    a.size,
			LatestAt:     a.latest,
			PendingCount: a.pending,
			CoverFileIDs: t.resolveCover(n, coverMemo, repMemo),
		}
	}
	return out, nil
}

// resolveCover 解析封面三级回退链,返回 ≤4 个 fileID:
//
//  1. 自定义封面:cover_file_id 指向的文件活跃且有缩略图 → 单值满铺;
//     引用悬空(文件已软删/不在树上/无缩略图)→ 落第 2 级;
//  2. 派生拼贴:直接子条目(目录与文件)名称自然序的前四个,每格取该子条目的
//     封面——子文件看缩略图,子目录递归取其自定义封面或子树内首个有缩略图的
//     文件;无封面的子条目不跳过,该格记 0(位置即信息,第几格空缺一目了然),
//     子条目不足四个时尾部留白(返回值变短)。派生只产"宫格":唯一子条目的
//     单槽一律回落空切片——满铺语义专属自定义封面,否则 A 只含子目录 B 时,
//     A 会顶着 B 里首个文件的封面,读起来像"该目录就是这个文件";
//  3. 默认四格:无任何子条目时返回空切片,由渲染端按目录名 hash 稳定挑内置占位图,
//     渲染期决定、零存储。
func (t *sumTree) resolveCover(n *sumNode, memo map[int64][]int64, repMemo map[int64]int64) []int64 {
	// 第 1 级:cover_file_id 有效(活跃 + 有封面)即单图满铺
	if n.cover > 0 {
		if f := t.files[n.cover]; f != nil && t.covers[f.id] {
			return []int64{n.cover}
		}
		// 引用悬空:继续走派生级
	}
	// 第 3 级边界:空作品交给渲染端
	if len(n.children)+len(n.files) == 0 {
		return []int64{}
	}
	// 第 2 级:混合名称序取前四个子条目
	if ids, ok := memo[n.id]; ok {
		return ids
	}
	type slot struct {
		name string
		id   int64
	}
	slots := make([]slot, 0, len(n.children)+len(n.files))
	for _, f := range n.files {
		id := int64(0)
		if t.covers[f.id] {
			id = f.id
		}
		slots = append(slots, slot{name: f.name, id: id})
	}
	for _, c := range n.children {
		slots = append(slots, slot{name: c.name, id: t.representative(c, repMemo)})
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].name < slots[j].name })
	if len(slots) > 4 {
		slots = slots[:4]
	}
	ids := make([]int64, len(slots))
	for i, s := range slots {
		ids[i] = s.id
	}
	// 单槽派生回落(含唯一子条目无缩略图的 [0]):前端对空切片/全空格都渲染
	// 默认文件夹图标,两种写法等效,统一归一成空切片
	if len(ids) == 1 {
		ids = []int64{}
	}
	memo[n.id] = ids
	return ids
}

// representative 找目录的"代表文件":自定义封面优先,否则先本目录文件、
// 再子目录递归(各自名称序,先文件后目录的先序),取第一个有封面的文件;
// 都没有则 0。纯名称序保证两次解析结果一致,与扫描定序同理。
func (t *sumTree) representative(n *sumNode, memo map[int64]int64) int64 {
	if id, ok := memo[n.id]; ok {
		return id
	}
	id := int64(0)
	if n.cover > 0 {
		if f := t.files[n.cover]; f != nil && t.covers[f.id] {
			id = n.cover
		}
	}
	if id == 0 {
		for _, f := range n.files {
			if t.covers[f.id] {
				id = f.id
				break
			}
		}
	}
	if id == 0 {
		for _, c := range n.children {
			if cid := t.representative(c, memo); cid != 0 {
				id = cid
				break
			}
		}
	}
	memo[n.id] = id
	return id
}

func normFolderID(id int64) int64 {
	if id == 0 {
		return rootFolderID
	}
	return id
}
