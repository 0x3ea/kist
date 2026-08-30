<script setup lang="ts">
// Files.vue — 主浏览页:搜索框(300ms 防抖,文件名/备注/目录名/标签)、工具栏
// (上传文件/文件夹、新建文件夹、下载、视图切换;操作类入口已收进右键菜单,
// 见 TODO-20)、面包屑、列表/网格双视图、右侧详情面板。
// 目录进入 = loadFolder;搜索态点击目录 = 跳进该目录。
import { computed, ref, watch } from 'vue'
import { index } from '../../wailsjs/go/models'
import {
  store,
  loadFolder,
  searchDebounced,
  exitSearch,
  upload,
  downloadSelected,
  deleteEntries,
  openDetail,
} from '../store'
import FileTable from '../components/FileTable.vue'
import CardGrid from '../components/CardGrid.vue'
import DetailPanel from '../components/DetailPanel.vue'
import MoveDialog from '../components/MoveDialog.vue'
import MetaDialog from '../components/MetaDialog.vue'
import NewFolderDialog from '../components/NewFolderDialog.vue'
import RenameDialog from '../components/RenameDialog.vue'
import ContextMenu, { type CtxItem } from '../components/ContextMenu.vue'
import { humanSize } from '../format'
import { fileKind, folderKind } from '../fileIcon'

const query = ref('')
const showMove = ref(false)
const showNewFolder = ref(false)
// 换库(TODO-21):store 侧统一回根目录清缓存,这里只清页面本地的搜索词
watch(
  () => store.driveSwitchSeq,
  () => {
    query.value = ''
  },
)
// 重命名目标(TODO-19,仅目录):恰好选中一个目录时可用
const renameTarget = ref<{ id: number; name: string } | null>(null)
// 元数据对话框目标:目录(TODO-16)或文件(TODO-17),恰好选中一个时可用
const metaTarget = ref<{ mode: 'folder' | 'file'; id: number; name: string; pack: boolean } | null>(null)

const selTotal = computed(() => store.selection.size + store.folderSelection.size)

function onSearch(q: string) {
  query.value = q
  searchDebounced(q)
}

function openFolder(id: number) {
  exitSearch()
  query.value = ''
  loadFolder(id)
}

function openSearchFolder(id: number) {
  openFolder(id)
}

function onUpload(kind: 'files' | 'folder') {
  upload(kind)
}

async function onDelete() {
  const n = selTotal.value
  if (n === 0) return
  const extra = store.folderSelection.size > 0 ? '\n注意:目录只从列表隐藏,其内文件需逐个删除后由「孤儿清理」回收远端空间。' : ''
  if (!confirm(`删除 ${n} 个条目?${extra}`)) return
  await deleteEntries()
}

function onRename() {
  const id = [...store.folderSelection][0]
  if (!id) return
  renameTarget.value = { id, name: store.folder.entries.find((e) => e.ID === id)?.Name ?? '' }
}

function onMeta() {
  const fid = [...store.folderSelection][0]
  if (fid) {
    metaTarget.value = {
      mode: 'folder',
      id: fid,
      name: store.folder.entries.find((e) => e.ID === fid)?.Name ?? '',
      pack: false,
    }
    return
  }
  const id = [...store.selection][0]
  const e = store.folder.entries.find((x) => x.ID === id)
  if (e) metaTarget.value = { mode: 'file', id, name: e.Name, pack: e.Pack }
}

// ---- 右键菜单(TODO-20):右键即选(目标不在选区时变为唯一选中,已在
// 选区则保持多选,配合勾选框可先多选再右键批量操作),条目按选区算禁用态;
// 四个动作与被移除的工具栏按钮同函数,行为逐项一致。
const ctxPos = ref<{ x: number; y: number } | null>(null)

function onEntryMenu(e: MouseEvent, entry: index.Entry) {
  const sel = entry.IsFolder ? store.folderSelection : store.selection
  if (!sel.has(entry.ID)) {
    store.selection.clear()
    store.folderSelection.clear()
    sel.add(entry.ID)
  }
  ctxPos.value = { x: e.clientX, y: e.clientY }
}

const ctxItems = computed<CtxItem[]>(() => {
  const nFiles = store.selection.size
  const nFolders = store.folderSelection.size
  const single = nFiles + nFolders === 1
  return [
    {
      label: '重命名',
      // 后端 RenameFolder 只支持目录(TODO-19);文件重命名落地前保持禁用明示
      disabled: !(nFolders === 1 && nFiles === 0),
      title: nFolders === 1 && nFiles === 0 ? '' : '仅支持重命名单个目录(文件重命名尚未支持)',
      action: onRename,
    },
    {
      label: '移动',
      // MoveFiles 只支持文件;目录移动是 TODO-19 立项时明示的范围外
      disabled: nFiles === 0,
      title: nFiles === 0 ? '仅支持移动文件(目录移动尚未支持)' : '',
      action: () => (showMove.value = true),
    },
    {
      label: '元数据',
      disabled: !single,
      title: single ? '' : '恰好选中一个条目时可用',
      action: onMeta,
    },
    {
      label: single ? '删除' : `删除 ${nFiles + nFolders} 项`,
      disabled: nFiles + nFolders === 0,
      danger: true,
      action: onDelete,
    },
  ]
})
</script>

<template>
  <div class="files">
    <!-- 工具栏 -->
    <div class="toolbar">
      <input
        class="search"
        type="text"
        placeholder="搜索文件名 / 备注 / 目录 / 标签…"
        :value="query"
        @input="onSearch(($event.target as HTMLInputElement).value)"
      />
      <span class="spacer" />
      <button @click="onUpload('files')">上传文件</button>
      <button @click="onUpload('folder')">上传文件夹</button>
      <button @click="showNewFolder = true">新建文件夹</button>
      <button :disabled="store.selection.size === 0" @click="downloadSelected">下载</button>
      <button class="view" :title="store.view === 'grid' ? '切到列表' : '切到网格'" @click="store.view = store.view === 'grid' ? 'list' : 'grid'">
        {{ store.view === 'grid' ? '☰' : '▦' }}
      </button>
    </div>

    <!-- 搜索结果态 -->
    <div v-if="store.search.active" class="search-results">
      <div class="search-head">
        <span>“{{ store.search.query }}” 的结果:{{ store.search.folders.length }} 个目录 · {{ store.search.files.length }} 个文件</span>
        <button @click="((query = ''), exitSearch())">退出搜索</button>
      </div>
      <div class="search-list">
        <div v-for="f in store.search.folders" :key="'f' + f.ID" class="hit folder" @click="openSearchFolder(f.ID)">
          <span class="kind"><component :is="folderKind.icon" :size="15" :color="folderKind.color" /></span>
          <span class="name">{{ f.Name }}</span>
          <span class="dim">{{ f.Path }}</span>
          <span v-if="f.Note" class="dim note">{{ f.Note }}</span>
          <span v-for="t in f.Tags" :key="t" class="tag">#{{ t }}</span>
        </div>
        <div v-for="h in store.search.files" :key="h.ID" class="hit" @click="openDetail(h.ID)">
          <span class="kind"><component :is="fileKind(h.Name).icon" :size="15" :color="fileKind(h.Name).color" /></span>
          <span class="name">{{ h.Name }}</span>
          <span class="dim">{{ h.Path }}</span>
          <span class="dim size">{{ humanSize(h.Size) }}</span>
          <span v-if="h.Note" class="dim note">{{ h.Note }}</span>
          <span v-for="t in h.Tags" :key="t" class="tag">#{{ t }}</span>
        </div>
        <div v-if="store.search.folders.length === 0 && store.search.files.length === 0" class="no-hit">没有命中</div>
      </div>
    </div>

    <!-- 正常浏览态 -->
    <template v-else>
      <div class="breadcrumb">
        <template v-for="(c, i) in store.folder.crumbs" :key="c.ID">
          <span class="crumb" :class="{ last: i === store.folder.crumbs.length - 1 }" @click="loadFolder(c.ID)">
            {{ c.Name || '根' }}
          </span>
          <span v-if="i < store.folder.crumbs.length - 1" class="sep">/</span>
        </template>
      </div>
      <div class="body">
        <div class="list-wrap">
          <FileTable v-if="store.view === 'list'" @open="openFolder" @menu="onEntryMenu" />
          <CardGrid v-else @open="openFolder" @menu="onEntryMenu" />
        </div>
        <DetailPanel />
      </div>
    </template>

    <MoveDialog v-if="showMove" @close="showMove = false" />
    <NewFolderDialog v-if="showNewFolder" @close="showNewFolder = false" />
    <RenameDialog
      v-if="renameTarget"
      :folder-i-d="renameTarget.id"
      :current-name="renameTarget.name"
      @close="renameTarget = null"
    />
    <MetaDialog
      v-if="metaTarget"
      :mode="metaTarget.mode"
      :id="metaTarget.id"
      :name="metaTarget.name"
      :pack="metaTarget.pack"
      @close="metaTarget = null"
    />
    <ContextMenu v-if="ctxPos" :x="ctxPos.x" :y="ctxPos.y" :items="ctxItems" @close="ctxPos = null" />
  </div>
</template>

<style scoped>
.files {
  height: 100%;
  display: flex;
  flex-direction: column;
}

.toolbar {
  display: flex;
  gap: 8px;
  padding: 10px 12px;
  align-items: center;
  border-bottom: 1px solid var(--line);
  flex-wrap: wrap;
}

.search {
  width: 260px;
}

.spacer {
  flex: 1;
}

.view {
  width: 40px;
  padding: 7px 0;
}

.breadcrumb {
  padding: 8px 12px;
  color: var(--dim);
  font-size: 13px;
  border-bottom: 1px solid var(--line);
  display: flex;
  gap: 4px;
  flex-wrap: wrap;
}

.crumb {
  cursor: pointer;
}

.crumb:hover {
  color: var(--accent);
}

.crumb.last {
  color: var(--text);
  cursor: default;
}

.sep {
  opacity: 0.5;
}

.body {
  flex: 1;
  display: flex;
  min-height: 0;
}

.list-wrap {
  flex: 1;
  overflow: auto;
  min-width: 0;
}

/* 搜索结果 */
.search-results {
  flex: 1;
  display: flex;
  flex-direction: column;
  min-height: 0;
}

.search-head {
  display: flex;
  justify-content: space-between;
  align-items: center;
  padding: 8px 12px;
  color: var(--dim);
  border-bottom: 1px solid var(--line);
}

.search-list {
  flex: 1;
  overflow: auto;
  padding: 4px 0;
}

.hit {
  display: flex;
  gap: 10px;
  align-items: baseline;
  padding: 6px 14px;
  cursor: pointer;
  font-size: 13px;
}

.hit:hover {
  background: var(--panel);
}

/* 类型图标列:SVG 与文字基线对齐会偏低,固定列宽 + 居中更稳 */
.kind {
  flex: 0 0 18px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  align-self: center;
}

.hit.folder .name {
  color: var(--accent);
}

.name {
  white-space: nowrap;
}

.dim {
  color: var(--dim);
  font-size: 12px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.note {
  max-width: 260px;
}

.size {
  flex-shrink: 0;
}

.tag {
  color: var(--accent);
  font-size: 12px;
}

.no-hit {
  text-align: center;
  color: var(--dim);
  padding: 40px 0;
}
</style>
