<script setup lang="ts">
// CoverMosaic.vue — 目录封面宫格(TODO-16 三级回退链的渲染端):
// 索引层已把封面解析成 ≤4 个 fileID(自定义封面=单值满铺,派生=前 4 子条目,
// 空=占位),这里只管渲染:1 格满铺 / 2-4 格均分,0 与取不到封面的格显示
// 按目录名 hash 挑色的占位块。取图由 CardGrid 的可见优先预取触发
// (TODO-10:封面可能走网络,不再组件内自发全量取)。
import { computed } from 'vue'
import { store } from '../store'

const props = defineProps<{ ids: number[]; name: string }>()

const cells = computed(() => {
  const list = props.ids?.filter((v) => v !== undefined) ?? []
  return list.slice(0, 4).map((id) => ({ id, url: store.thumbs.get(id) ?? '' }))
})

// 占位色:目录名 hash → 色相,同目录稳定着色
const hue = computed(() => {
  let h = 0
  for (const ch of props.name ?? '') h = (h * 31 + ch.charCodeAt(0)) % 360
  return h
})
</script>

<template>
  <div class="mosaic" :class="`n${cells.length || 'one'}`" :style="`--h:${hue}`">
    <template v-if="cells.length">
      <div v-for="c in cells" :key="c.id" class="cell">
        <img v-if="c.url" :src="c.url" alt="" />
        <div v-else class="placeholder" />
      </div>
    </template>
    <!-- 无子条目:整卡占位 -->
    <div v-else class="cell"><div class="placeholder" /></div>
  </div>
</template>

<style scoped>
.mosaic {
  width: 100%;
  aspect-ratio: 1;
  display: grid;
  gap: 2px;
  overflow: hidden;
  border-radius: 6px;
  background: var(--panel-2);
}

/* 单值满铺(自定义封面);2×2 / 1×2 网格按数量均分 */
.mosaic.n1,
.mosaic.none {
  grid-template-columns: 1fr;
}

.mosaic.n2 {
  grid-template-columns: 1fr 1fr;
}

.mosaic.n3,
.mosaic.n4 {
  grid-template-columns: 1fr 1fr;
  grid-template-rows: 1fr 1fr;
}

.cell {
  overflow: hidden;
  min-height: 0;
  min-width: 0;
}

.cell img {
  width: 100%;
  height: 100%;
  object-fit: cover;
  display: block;
}

.placeholder {
  width: 100%;
  height: 100%;
  background: linear-gradient(135deg, hsl(var(--h), 30%, 32%), hsl(calc(var(--h) + 40), 28%, 24%));
}
</style>
