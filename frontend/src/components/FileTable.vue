<script setup lang="ts">
// FileTable.vue — 列表视图:目录行显示子树摘要(CLI ls 同款),文件行显示
// 大小/时间/状态;类型列用共享 fileKind 图标,uploading 标"待上传"。
// 条目点击(目录与文件同款约定):普通单击 = 唯一选中 + 右栏详情(目录双击
// = 进入,名称链接保留为即时进入的捷径);Ctrl/⌘+单击 = 多选切换,不清
// 其余、不动面板,目录侧立即生效免双击判定。
import { index } from '../../wailsjs/go/models'
import { store, summaryText, openDetail, openFolderDetail, selectOnly, ctrlToggleSelect, isMultiSelectClick } from '../store'
import { humanSize, shortTime } from '../format'
import { fileKind, folderKind } from '../fileIcon'

const emit = defineEmits<{ open: [id: number]; menu: [e: MouseEvent, entry: index.Entry] }>()

function toggleFile(id: number) {
  store.selection.has(id) ? store.selection.delete(id) : store.selection.add(id)
  if (store.selection.size === 0) store.detail = null
}

function toggleFolder(id: number) {
  store.folderSelection.has(id) ? store.folderSelection.delete(id) : store.folderSelection.add(id)
}

function onRowClick(ev: MouseEvent, e: index.Entry) {
  if (e.IsFolder) return
  if (isMultiSelectClick(ev)) {
    ctrlToggleSelect(e)
    return
  }
  selectOnly(e)
  openDetail(e.ID)
}

// 目录单击/双击:双击前必先落一次 click——第一击挂 250ms 判定,窗口内的
// 第二击取消判定并交由 dblclick 进入目录,否则每次双击都会先闪一次面板。
// 判定到点后目录已不在当前列表(切目录/已删)时 openFolderDetail 自会放弃。
let folderClickTimer: ReturnType<typeof setTimeout> | null = null

function folderClick(ev: MouseEvent, id: number) {
  // Ctrl/⌘+点击 = 多选切换:立即生效免判定;先撤挂起的普通单击判定,
  // 不撤的话它到点会清空选区,把这次多选悄悄抹掉
  if (isMultiSelectClick(ev)) {
    if (folderClickTimer) {
      clearTimeout(folderClickTimer)
      folderClickTimer = null
    }
    ctrlToggleSelect({ IsFolder: true, ID: id })
    return
  }
  if (folderClickTimer) {
    clearTimeout(folderClickTimer)
    folderClickTimer = null
    return // 双击的第二击:让位 dblclick
  }
  folderClickTimer = setTimeout(() => {
    folderClickTimer = null
    // 单击 = 唯一选中(多选走 Ctrl+单击/勾选框),右栏展示元数据
    store.selection.clear()
    store.folderSelection.clear()
    store.folderSelection.add(id)
    openFolderDetail(id)
  }, 250)
}

function folderDblClick(id: number) {
  if (folderClickTimer) {
    clearTimeout(folderClickTimer)
    folderClickTimer = null
  }
  emit('open', id)
}

function stateTag(e: index.Entry): string {
  if (e.State === 'uploading') return '待上传'
  if (e.State === 'missing') return '缺失'
  return ''
}
</script>

<template>
  <table class="table">
    <thead>
      <tr>
        <th class="c-check"></th>
        <th class="c-kind"></th>
        <th>名称</th>
        <th class="c-size">大小 / 摘要</th>
        <th class="c-time">修改时间</th>
      </tr>
    </thead>
    <tbody>
      <tr
        v-for="e in store.folder.entries"
        :key="e.ID"
        :class="{ sel: e.IsFolder ? store.folderSelection.has(e.ID) : store.selection.has(e.ID) }"
        @click="e.IsFolder ? folderClick($event, e.ID) : onRowClick($event, e)"
        @dblclick="e.IsFolder && folderDblClick(e.ID)"
        @contextmenu.prevent="$emit('menu', $event, e)"
      >
        <td class="c-check" @click.stop>
          <input
            type="checkbox"
            :checked="e.IsFolder ? store.folderSelection.has(e.ID) : store.selection.has(e.ID)"
            @change="e.IsFolder ? toggleFolder(e.ID) : toggleFile(e.ID)"
          />
        </td>
        <td class="c-kind">
          <component
            :is="e.IsFolder ? folderKind.icon : fileKind(e.Name, e.Pack).icon"
            :size="15"
            :color="e.IsFolder ? folderKind.color : fileKind(e.Name, e.Pack).color"
          />
        </td>
        <td class="c-name">
          <span v-if="e.IsFolder" class="link" @click.stop="$emit('open', e.ID)">{{ e.Name }}</span>
          <template v-else>{{ e.Name }}</template>
          <em v-if="stateTag(e)" class="tag">{{ stateTag(e) }}</em>
        </td>
        <td class="c-size dim">
          <template v-if="e.IsFolder">{{ summaryText(e.ID) }}</template>
          <template v-else>{{ humanSize(e.Size) }}</template>
        </td>
        <td class="c-time dim">{{ shortTime(e.ModifiedAt) }}</td>
      </tr>
      <tr v-if="store.folder.entries.length === 0">
        <td colspan="5" class="empty">空目录——用右上角按钮上传,或拖入文件夹</td>
      </tr>
    </tbody>
  </table>
</template>

<style scoped>
.table {
  width: 100%;
  border-collapse: collapse;
  font-size: 13px;
}

th {
  text-align: left;
  color: var(--dim);
  font-weight: normal;
  padding: 6px 8px;
  border-bottom: 1px solid var(--line);
  position: sticky;
  top: 0;
  background: var(--bg);
}

td {
  padding: 6px 8px;
  border-bottom: 1px solid var(--line);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

tr.sel td {
  background: rgba(79, 140, 255, 0.12);
}

tbody tr:hover td {
  background: var(--panel);
}

.c-check {
  width: 30px;
}

.c-kind {
  width: 28px;
  color: var(--dim);
}

.c-size {
  max-width: 260px;
}

.c-time {
  width: 90px;
}

.c-name .link {
  color: var(--accent);
  cursor: pointer;
}

.tag {
  font-style: normal;
  color: var(--warn);
  margin-left: 8px;
  font-size: 12px;
}

.dim {
  color: var(--dim);
}

.empty {
  text-align: center;
  color: var(--dim);
  padding: 40px 0;
}
</style>
