# TODO-17 — 文件元数据:tag 挂文件与手动封面

> 状态:候选(2026-08-25 讨论收敛:文件形态作品挂不上 tag/作者、无封面是真实缺口;方案全部骑现有机制,零新概念)
> 来源:作品两形态的元数据不对称——16 只给了目录 note/tags/cover,文件仅 note;epub/mp4 库无法按作者组织,卡片永远是占位图
> 与合集渲染的分工:本项只补元数据表面,不碰卡片渲染(渲染另案,见"留档");封面与 tag 都是纯索引状态,随 index.enc 跨设备同步——显式标记方案的同步卖点被白拿

## 问题

作品落到索引的三种形态与信息挂点:

| 形态 | 例子 | 信息挂点 | 现状 |
|---|---|---|---|
| 系列 | 父目录→子目录→图 | 父目录 | ✅ 16 已覆盖(note/tags/cover) |
| 单作品·目录 | 目录→图(put 成 pack) | 目录 | ✅ 同上 |
| 单作品·文件 | epub / mp4 / 直挂 pack | 文件 | ❌ 仅 note;tag 挂不上,无封面 |

缺口的具体表现:

- **tag**:folder_tags 只挂目录,文件作品的 tag 无处落——epub 库按作者/系列组织不可行;文件搜索(db.Search)也只覆盖 name/note;
- **封面**:缩略图管线只吃图片(makeThumbnail 对 epub/mp4 失败即 Warn 跳过),文件作品在卡片/列表/详情里永远是类型占位图。

## 设计原则

- **对称 16,孪生不另起炉灶**:目录有的元数据表面,文件对齐;写入语义(全量覆盖、指针增量、死词清理)照搬目录先例。
- **骑现有管线**:文件封面 = 它自己的 thumbnails 行(封面字节永远属于文件;目录走 cover_file_id 引用式是因为目录自身没有字节);tag = 共享 tags 词表 + 新挂点表,两种作品形态同一词典。
- **同步红利免费拿**:一切写经 WithTx 自动计 revision(db.go,索引云备份 LWW 的判定依据)——tag 与手动封面零同步代码即跨设备。

## 方案

### tag 挂文件(v4 迁移,加法式)

```sql
-- v4(TODO-17):文件 tag 挂点,镜像 folder_tags,共享 tags 词表
-- (目录/文件作品同一词典,"按 tag 捞作品"横跨两形态)
CREATE TABLE IF NOT EXISTS file_tags (
  file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  tag_id  INTEGER NOT NULL REFERENCES tags(id)   ON DELETE CASCADE,
  UNIQUE(file_id, tag_id));
CREATE INDEX IF NOT EXISTS ix_file_tags_tag ON file_tags(tag_id);
```

- 作者不设独立列,走 tag 约定(如 `作者:名`)——16 三不原则既定路线;
- `FileMeta/FileMetaUpdate` 孪生目录版:Note *string / Tags 非 nil 即全量覆盖(nil=不动,空切片=清空);写入四步照搬 UpdateFolderMeta——删旧关联 → 词表 get-or-create → 挂关联(先 trim 去重)→ 清死词;
- **清死词子查询必须 UNION 两张挂点表**:`DELETE FROM tags WHERE id NOT IN (SELECT tag_id FROM folder_tags UNION SELECT tag_id FROM file_tags)`——只查一张会把另一形态仍在用的词误删(测试先行:folder 与 file 同名 tag,清一侧不得伤另一侧);
- 现有 SetNote 保留兼容(CLI 在用),GUI 改走 GetFileMeta/UpdateFileMeta 新口。

### 手动封面(零 schema)

文件的封面就是它自己的 thumbnails 行,机制全部现成,缺三小段:

| 增量 | 位置 | 内容 |
|---|---|---|
| 导出缩略图生成器 | transfer/thumb.go | makeThumbnail(包内私有)→ MakeThumbnail;返回类型 thumbData 一并导出或摊平为多返回值 |
| 补删除 | index/thumbnails.go | DeleteThumbnail(tx, fileID)——现状只有 Put/Get,从无删除需求 |
| 新绑定 | app_browse.go | SetFileCover(fileID, localPath):MakeThumbnail → WithTx 内 PutThumbnail;localPath 空 = 清除(调 Delete) |

链路:GUI「导入封面」(wails runtime.OpenFileDialog 选本地图片)→ MakeThumbnail(与上传管线同规格:≤128KB,不透明→JPEG q80,含透明→PNG)→ PutThumbnail(INSERT OR REPLACE,覆盖语义)→ emitIndexChanged → 卡片 ensureThumb 即取新封面 → index.enc 备份同步到其他设备。

坑与边界:

- **pack 覆盖语义**:pack 上传时自动存了首页缩略图,手动封面直接顶掉它,清除后不恢复自动封面(无副本)——漫画库是主流形态,清除确认框里明示"将失去封面";
- **清除语义与目录不同**:文件清除 = 删 thumbnails 行;目录清除 = cover_file_id 置 NULL 而非 0(FK 坑,folders.go 已记录);
- 非图片路径/坏图:报错原样上抛(AppError),不静默——这是显式用户动作,与上传管线"失败即跳过"的语义不同。

### 搜索

db.Search(files)加 `EXISTS (… file_tags JOIN tags …)` 子查询,镜像 SearchFolders 走 ix_file_tags_tag——文件 tag 进搜索面。加成:词表共享 + 双侧可搜 ⇒ **作品级 tag 过滤不立项即得大半**(works 收藏表留档的触发条件之一被吸收)。

### GUI / CLI

- FolderMetaDialog 泛化为 MetaDialog:目录形态照旧(note/tags/cover fileID);文件形态(note/tags +「导入封面…」,已有缩略图时预览 +「清除封面」);
- 渲染零改动:卡片/列表的 ensureThumb 直取新封面;tag 不进网格,在搜索结果与详情面板展示;
- CLI:`meta` 支持文件路径(--tag/--note 同语义);封面导入 GUI-only(本地文件对话框是 GUI 的天然能力,CLI 真有需要再补 --cover-file)。

## 留档:合集渲染与 works 收藏表

合集卡片视图曾完整评估过一轮(原 17 稿,未入库即删),定案要点浓缩于此备查:

- **渲染读类型与构成,不读身份**:目录/pack 天然携带聚合信息(子树摘要、entries/orig_size、首页缩略图),逐层浏览只需回答"子条目怎么渲染",从不需要"这个节点是不是作品"——同构异义(`漫画库/{作品.pack}` 与 `作品/{话.pack}` 签名相同、语义相反)在渲染层不再咬人;
- **密度默认按当前层构成推断**(全 pack→紧凑话列表;散文件为主→普通列表;其余→卡片网格+尾部文件行),用户切换按目录记忆(localStorage)——切换+记忆即懒执行的手动指定;
- **pack 卡片需要 Entry 读侧暴露 entries/orig_size**(user_meta 自 15 落库后无读者;Entry.Size 是量化后明文总长,不得当原始大小用)——此增量属渲染案,不并入本项;
- **works 收藏表**(folder XOR file 引用 + put 自动锚定 + work mark/unmark)因渲染不需要身份而搁置;本项落地后其剩余独特价值收窄为**标题覆盖**与**全库扁平合集视图**(同步已被纯索引状态吸收,tag 过滤已被共享词表吸收)——真需要跨层"所有作品"视图时再立项。

## 范围外

卡片视图渲染规则(密度推断/尾部文件行/紧凑话列表,另案);pack entries/orig_size 的 Entry 暴露(属渲染案);epub 封面自动提取(容器解析,量级另评,手动封面先顶);TODO-10 封面出库(手动封面继续住索引库,个人规模无碍,10 的"引用不变"结论不受影响)。

## 涉及模块

- `internal/index`:schema.go 追加 v4 迁移;files.go 增 GetFileMeta/UpdateFileMeta;thumbnails.go 增 DeleteThumbnail
- `internal/transfer`:导出 MakeThumbnail
- `app_browse.go`:GetFileMeta / UpdateFileMeta / SetFileCover 三个绑定
- 前端:FolderMetaDialog 泛化、详情面板与搜索结果展示 tag
- `cmd/kistctl`:meta 支持文件路径
- put 与远端零改动,全程零远端流量

## 验收标准

- v4 迁移:老库升级无损、新库直建同构;15/16 现有测试全绿;
- 文件 tag:设置/全量覆盖/清除/trim 去重;**同名 tag 双形态共存时清一侧不伤另一侧**(专项测试);
- 手动封面:设置后卡片即显;备份→恢复后仍在(孪生 TestMetaSurvivesSnapshot);清除回落占位且 pack 有确认提示;非图片路径报错可读;
- 搜索:文件按 tag 命中,与目录 tag 同入口;
- CLI:meta 对文件设/清 tag 与 note;
- 前端 `npm run build`(含 vue-tsc)通过;收尾验收命令全绿:`go build ./... && go vet ./... && gofmt -l . && go test ./... -race -count=1`

量级:Go 约 150–200 行 + 前端约 100 行,中偏小。
