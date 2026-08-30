<script setup lang="ts">
// CoverMosaic.vue — 目录封面宫格(TODO-16 三级回退链的渲染端):
// 索引层已把封面解析成 ≤4 个 fileID(自定义封面=单值满铺,派生=前 4 子条目,
// 子条目无缩略图记 0),这里只管渲染:1 格满铺 / 2 横排 / 3-4 2×2 均分,
// 0 与取不到字节的格子留白(与 quickstart「空位留白」一致);所有格都取不到
// 字节时整卡退文件夹图标。取图由 CardGrid 的可见优先预取触发(TODO-10:
// 封面可能走网络,不再组件内自发全量取)。
import { computed } from 'vue'
import { store } from '../store'
import { folderKind } from '../fileIcon'

const props = defineProps<{ ids: number[] }>()

const cells = computed(() => {
  const list = props.ids?.filter((v) => v !== undefined) ?? []
  return list.slice(0, 4).map((id) => ({ id, url: store.thumbs.get(id) ?? '' }))
})

// 一个格子都取不到字节(子条目全无缩略图,ensureThumb 已负缓存)时退文件夹
// 图标——否则整卡是一片真空,连"这是目录"都读不出来。有图格仍按原位渲染,
// 空位留白不变(位置即信息);封面在途时也会先显示图标再换图,顺带当加载态。
const empty = computed(() => cells.value.every((c) => !c.url))
</script>

<template>
  <div class="mosaic" :class="`n${empty ? 'one' : cells.length}`">
    <template v-if="!empty">
      <div v-for="c in cells" :key="c.id" class="cell">
        <img v-if="c.url" :src="c.url" alt="" />
      </div>
    </template>
    <!-- 零封面(无子条目,或子条目全无缩略图):整卡占位 -->
    <div v-else class="empty" :style="{ color: folderKind.color }">
      <component :is="folderKind.icon" :size="44" :stroke-width="1.5" />
    </div>
  </div>
</template>

<style scoped>
.mosaic {
  width: 100%;
  aspect-ratio: 2 / 3;
  display: grid;
  gap: 2px;
  overflow: hidden;
  border-radius: 6px;
  background: var(--panel-2);
}

/* 单值满铺(自定义封面);2 横排 / 3-4 2×2 网格按数量均分。
   容器为 2:3 竖版:2 格若左右排会切成 1:3 细条,故改上下两行(每格≈4:3) */
.mosaic.n1,
.mosaic.none {
  grid-template-columns: 1fr;
}

.mosaic.n2 {
  grid-template-rows: 1fr 1fr;
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

.empty {
  width: 100%;
  height: 100%;
  display: flex;
  align-items: center;
  justify-content: center;
}
</style>
