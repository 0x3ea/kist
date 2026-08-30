<script setup lang="ts">
// CardGrid.vue — 网格视图:目录卡 = 封面宫格 + 名称 + 摘要;文件卡 = 缩略图
// 或类型占位 + 名称 + 大小。封面链已由索引层解析为 CoverFileIDs(≤4),
// 自定义封面 = 单值满铺,由 CoverMosaic 按格数自适应。
// TODO-10 出库后封面可能走网络:全量预取改为 IntersectionObserver 可见优先
// (rootMargin 提前 200px ≈ 预取一屏),实际并发由 store 的有界队列限制。
import { index } from '../../wailsjs/go/models'
import { store, ensureThumb, openDetail, summaryText } from '../store'
import { humanSize } from '../format'
import { fileKind } from '../fileIcon'
import CoverMosaic from './CoverMosaic.vue'
import { onBeforeUnmount, onMounted } from 'vue'

defineEmits<{ open: [id: number]; menu: [e: MouseEvent, entry: index.Entry] }>()

let io: IntersectionObserver | null = null
const pendingFetch = new Map<Element, () => void>()

// ref 回调工厂:登记卡片元素与"该卡可见时要取哪些封面"
function registerCard(e: index.Entry) {
  return (el: unknown) => {
    const elc = el as Element | null
    if (!elc || !io || pendingFetch.has(elc)) return
    const fetch = () => {
      if (e.IsFolder) {
        for (const id of store.folder.summaries[String(e.ID)]?.CoverFileIDs ?? []) {
          if (id) ensureThumb(id)
        }
      } else {
        ensureThumb(e.ID)
      }
    }
    pendingFetch.set(elc, fetch)
    io.observe(elc)
  }
}

onMounted(() => {
  io = new IntersectionObserver(
    (ents) => {
      for (const en of ents) {
        if (!en.isIntersecting) continue
        pendingFetch.get(en.target)?.()
        pendingFetch.delete(en.target)
        io?.unobserve(en.target)
      }
    },
    { rootMargin: '200px' }
  )
})

onBeforeUnmount(() => {
  io?.disconnect()
  io = null
  pendingFetch.clear()
})

function toggleFile(id: number) {
  store.selection.has(id) ? store.selection.delete(id) : store.selection.add(id)
}

function cardClick(e: index.Entry) {
  if (e.IsFolder) return
  toggleFile(e.ID)
  openDetail(e.ID)
}
</script>

<template>
  <div class="grid">
    <div
      v-for="e in store.folder.entries"
      :key="e.ID"
      :ref="registerCard(e)"
      class="card"
      :class="{ sel: e.IsFolder ? store.folderSelection.has(e.ID) : store.selection.has(e.ID) }"
      @click="e.IsFolder ? $emit('open', e.ID) : cardClick(e)"
      @contextmenu.prevent="$emit('menu', $event, e)"
    >
      <span
        class="check"
        :class="{ on: e.IsFolder ? store.folderSelection.has(e.ID) : store.selection.has(e.ID) }"
        @click.stop="e.IsFolder ? (store.folderSelection.has(e.ID) ? store.folderSelection.delete(e.ID) : store.folderSelection.add(e.ID)) : toggleFile(e.ID)"
      >
      </span>
      <CoverMosaic v-if="e.IsFolder" :ids="store.folder.summaries[String(e.ID)]?.CoverFileIDs ?? []" />
      <div v-else class="thumb">
        <img v-if="store.thumbs.get(e.ID)" :src="store.thumbs.get(e.ID)" alt="" />
        <!-- 无缩略图:按扩展名给类型图标(pack = 目录打包物,独立于普通文件) -->
        <component
          :is="fileKind(e.Name, e.Pack).icon"
          v-else
          class="file-type"
          :size="44"
          :stroke-width="1.5"
          :color="fileKind(e.Name, e.Pack).color"
        />
      </div>
      <div class="name" :title="e.Name">
        {{ e.Name }}
        <em v-if="e.State === 'uploading'" class="tag">待上传</em>
      </div>
      <div class="meta dim">{{ e.IsFolder ? summaryText(e.ID) : humanSize(e.Size) }}</div>
    </div>
    <div v-if="store.folder.entries.length === 0" class="empty">空目录</div>
  </div>
</template>

<style scoped>
.grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(150px, 1fr));
  gap: 14px;
  padding: 4px;
  align-content: start;
}

.card {
  position: relative;
  cursor: pointer;
  border: 2px solid transparent;
  border-radius: 10px;
  padding: 6px;
}

.card:hover {
  background: var(--panel);
}

.card.sel {
  border-color: var(--accent);
  background: rgba(79, 140, 255, 0.08);
}

.check {
  position: absolute;
  top: 10px;
  left: 10px;
  width: 16px;
  height: 16px;
  border-radius: 4px;
  border: 1.5px solid var(--line);
  background: rgba(27, 38, 54, 0.7);
  z-index: 2;
}

.check.on {
  background: var(--accent);
  border-color: var(--accent);
}

.check.on::after {
  content: '✓';
  color: #fff;
  font-size: 12px;
  position: absolute;
  left: 2px;
  top: -2px;
}

.thumb {
  aspect-ratio: 2 / 3;
  border-radius: 6px;
  overflow: hidden;
  background: var(--panel-2);
  display: flex;
  align-items: center;
  justify-content: center;
}

.thumb img {
  width: 100%;
  height: 100%;
  object-fit: cover;
}

.file-type {
  opacity: 0.85;
}

.name {
  margin-top: 6px;
  font-size: 13px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.meta {
  font-size: 12px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.tag {
  font-style: normal;
  color: var(--warn);
  font-size: 12px;
}

.dim {
  color: var(--dim);
}

.empty {
  grid-column: 1 / -1;
  text-align: center;
  color: var(--dim);
  padding: 40px 0;
}
</style>
