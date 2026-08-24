# TODO-16 — 作品级元数据与聚合视图(目录元数据 + tag + 移动)

> 状态:立项实施中(2026-08-24;封面三级回退链与元数据三不原则随评审定稿,见对应小节)
> 来源:漫画库浏览需求——浏览的决策单元是作品,不是话

## 问题

- `folders` 表没有任何用户数据位:作者/tag 无处挂,作品级属性只能重复打在每话的 note 上;
- `Search` 只查 `files` 的 name/note(`internal/index/files.go`),**目录名不在检索面**——按作品名找,除非文件名恰好含作品名,否则搜不到;
- 浏览投影缺失:`ls`/`ListFolder` 是账本视图(一话一行),决策信息(话数/总大小/最近更新/封面/tag)要人工翻目录拼出来;
- 归属只能一次性给对:话传错了作品目录,现状唯一纠错是 rm + 重传——一话 100–200MB 在 ~100 请求/分钟限速下代价真实。

## 设计原则(贯穿全项)

**结构承载身份,元数据承载属性。** 作品 = 目录 + 子树内 pack 条目(结构涌现,put 即成立,无需 `--series` 标志或"标记为作品"按钮);作者/tag/封面指定是元数据(缺席不降级,写错只影响检索与徽章)。索引层领域无关:`FolderSummary` 只说 PackCount/TotalSize,不说"话数"——漫画措辞属于 CLI/GUI 壳层,同一聚合对相册/专辑原样适用。

## 方案

### 目录元数据(schema 迁移,15 之后)

- `folders` 加 `note`、`user_meta`(与 files 对齐);
- tag 走独立表——**过滤是 tag 的全部意义**,LIKE-over-JSON 撑不起:

```sql
CREATE TABLE tags (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE);
CREATE TABLE folder_tags (
  folder_id INTEGER NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
  tag_id   INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
  UNIQUE(folder_id, tag_id));
```

- CLI:`meta set <目录> --note/--tag …`、`meta list`;`Search` 扩展:目录名与目录 tag 进检索面。
- **元数据三不原则**:不继承、不合并、无告警——tag/作者/note 是文件夹自身属性,父子各自持有、互不感知;聚合面(FolderSummary)只聚合计数,永不聚合元数据,父子"冲突"场景结构性不存在。哑远端约束同款:写在哪个目录就属于哪个目录,kist 不做解释。

### 聚合视图(纯查询,零 schema)

`FolderSummary(folderID)` 递归子树聚合:`PackCount / FileCount / TotalSize / LatestAt / PendingCount(state=uploading)/ CoverFileIDs`(三级链见下节)。

- 实现:**不用递归 CTE**,沿用 `loadFolderInfos` 先例(全量载入内存建树,个人规模毫秒级,与 Search 的内存拼路径同风格);
- CLI:`ls` 目录条目附带子树摘要(`作品A/  12 话 · 8.2GB · ← 08-01`,待传话数单列)——对任何目录普遍有用,不限漫画;
- GUI(Phase 7 设计输入):`ListCards(parentID) → []FolderCard{Title, PackCount, FileCount, TotalSize, LatestAt, PendingCount, CoverFileIDs, Author, Tags}`。Files.vue 增**卡片视图(用户切换,而非逐目录判断身份)**:`PackCount > 0` 措辞"N 话",否则"N 个文件";封面按三级链渲染(自定义单图满铺 → 派生四宫格,空位留白 → 默认四格);进作品才是紧凑话列表(虚拟滚动);缓存按 revision 失效(`index:changed` 事件现成)。

### 封面引用(零新增机制)

**三级回退链,单值改多值**——`FolderSummary`/`FolderCard` 返回 `CoverFileIDs []int64`(≤4),纯查询零 schema:

1. **自定义封面**:`cover_file_id` 指向的文件单图满铺。cover.jpg 作为**普通文件** put 进作品目录 + 目录元信息指向——复用全部文件管线(加密/出站箱/备份/gc),不发明"封面 blob"类别;换封面 = 改引用,零流量;引用悬空 → 落第 2 级;
2. **派生拼贴**:2×2 宫格**按位置对应**子条目(目录与文件)名称自然序的前四个——每格显示该子条目的封面:子目录取其自定义封面或递归解析的首张缩略图,子文件取其缩略图。**不跳过**无封面的子条目,该格渲染默认占位——位置即信息,第几格空缺一目了然;子条目不足四个时尾部**留白**。`CoverFileIDs` 长度 = min(4, 子条目数),0 值即"此格默认占位";
3. **默认四格**:无任何子条目(空作品)时,按目录名 hash 从内置占位图集稳定挑 4 张——渲染期决定、零存储,与扫描定序同理保证两次打开一致。

- 派生扫描一次遍历子树顺带收集,与原单封面方案同量级;每卡片 `GetThumbnail` 至多 4 次,缩略图在索引内,毫秒级;
- 与 TODO-10 前向兼容:缩略图字节将来出库,引用不变。

### MoveEntries(纯索引,零远端流量)

`mv <源> <目标目录>`:`UPDATE files SET folder_id`(含逐级建目录与 UniqueFileName 消解),blob 不动、字节不重传——归属给错时的便宜纠错通道,与 15 的"归属是流程问题"闭环。

### 可选增强(记口子,不立项)

- 最近下载时间(get 时记一条时间戳):卡片上"上次看到第 N 话"的弱进度代理——"已读"属阅读器域,kist 不碰;
- 显示名覆盖(目录名 ≠ 展示标题)、FTS5。

## 代价与缓解

- 索引多几列 + tag 行:纯状态,状态库该背的,极小;
- `ListCards` 聚合成本:个人规模全量内存聚合毫秒级,GUI 按 revision 失效,无增量维护复杂度。

## 迁移

folders 加列 + tags 两表(`user_version` 顺序迁移,开发阶段无数据负担)。依赖 15 先行(pack 列、首页缩略图)。

## 范围外

阅读进度(阅读器域);zip 内容穿透浏览;文件级 tag(需要时加 `file_tags` 同构表)。

## 涉及模块

`internal/index`(folders 元数据、tags 表、Search 扩展、FolderSummary)、`cmd/kistctl`(meta/mv、ls 摘要)、`app.go` + Files.vue(ListCards、卡片视图)。

## 验收标准

- meta set 作者/tag → index.enc 备份/拉回后仍在;search 按作者、tag、目录名均命中
- ls 附子树摘要:多话作品一行摘要;出站箱未推的话数单列可见(呼应 13)
- mv 移动一话到另一作品:远端对象零变化(对账验证),聚合即时更新
- 自定义封面:put cover.jpg + meta set cover → 摘要/卡片引用之;rm 该文件 → 回退派生默认
- 收尾验收命令全绿:`go build ./... && go vet ./... && gofmt -l . && go test ./... -race -count=1`
