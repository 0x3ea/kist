# TODO-19 — 目录重命名(CLI rename + GUI 重命名)

> 状态:候选(2026-08-25;接 TODO-18:目录可建了,但传错的/想改的名字无法改)
> 来源:已上传的文件夹无法重命名——目录名是纯索引概念(blob 名与虚拟路径无关),改名本应零流量,却没有任何入口

## 现状核对

- `mv` 只动**文件**挂点(MoveFiles);目录既不能改名也不能移动;
- 目录名存 `folders.name`,`ux_folders_live` 保证同父下活跃目录不重名(软删后名字可复用);
- 改名 = `UPDATE folders SET name`,纯索引零流量,经 WithTx 计 revision 随 index.enc 同步。

## 方案

### 索引层:`RenameFolder(folderID, name)`

语义定案(与既有宽容/严格分界对齐):

- **同名 no-op 成功**:新名 == 当前名直接返回(mkdir 幂等同精神);
- **撞名报错,不自动 "(1)" 消解**:重命名是显式单发动作,撞名多半是选错了目标,自动后缀会制造意外名——与 mv 的批量消解场景不同(mv 是搬迁,碰撞是附带的);
- **单段校验**:新名必须是合法目录段(非空、非 "."/".."、不含 "/"),与 EnsureFolderPath 同规则;
- **根不可改**:根目录名约定为空串(路径拼接依赖),id=1 拒绝;
- 软删目录拒绝(不可见的东西没有名字可改)。

### CLI:`rename <目录路径> <新名>`

目录按 /路径 指认(与 meta 同款,ResolveFolderPath);成功打印新旧名。

### GUI:工具栏「重命名」

恰好选中一个目录时可用(与「元数据」按钮同款条件);弹输入对话框(预填当前名,全选);成功后 emitIndexChanged 刷新当前目录。

## 范围外

- **文件重命名**:同一机制(~20 行,`UPDATE files SET name` + 撞名报错),但本项按诉求只做目录;真需要时半天跟进;
- **目录移动到别的父目录**(mvdir):改 parent_id,子树 blob 依旧无关,同样纯索引——但涉及面包屑/摘要/移动语义,单独立项;
- 批量重命名。

## 涉及模块

- `internal/index/folders.go`:RenameFolder(~40 行)+ 单测;
- `cmd/kistctl/main.go`:cmdRename + usage + dispatch(~30 行);
- `app_browse.go`:RenameFolder 绑定(~15 行);
- 前端:Files.vue 按钮 + RenameDialog.vue(~100 行);
- 零 schema、零管线、零远端流量。

## 验收标准

- 改名后 `ls`/路径解析/搜索/摘要即时反映新名;backup/pull 恢复后仍为新名;
- 同名重跑 no-op;撞名报错人话;根/软删/非法段("a/b"、"..")拒绝;
- 全量验收命令全绿 + `npm run build`(vue-tsc)通过。

量级:Go ~90 行 + 前端 ~100 行,很小。
