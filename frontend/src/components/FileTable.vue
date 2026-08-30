<script setup lang="ts">
// FileTable.vue — 列表视图:目录行显示子树摘要(CLI ls 同款),文件行显示
// 大小/时间/状态;类型列用共享 fileKind 图标,uploading 标"待上传"。
import { index } from '../../wailsjs/go/models'
import { store, summaryText, openDetail } from '../store'
import { humanSize, shortTime } from '../format'
import { fileKind, folderKind } from '../fileIcon'

defineEmits<{ open: [id: number]; menu: [e: MouseEvent, entry: index.Entry] }>()

function toggleFile(id: number) {
  store.selection.has(id) ? store.selection.delete(id) : store.selection.add(id)
  if (store.selection.size === 0) store.detail = null
}

function toggleFolder(id: number) {
  store.folderSelection.has(id) ? store.folderSelection.delete(id) : store.folderSelection.add(id)
}

function onRowClick(e: index.Entry) {
  if (e.IsFolder) return
  toggleFile(e.ID)
  openDetail(e.ID)
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
        @click="e.IsFolder ? null : onRowClick(e)"
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
