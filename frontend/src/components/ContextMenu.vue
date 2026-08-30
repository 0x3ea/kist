<script lang="ts">
// 条目类型放普通 script 块导出(script setup 不允许 export 语句)
export interface CtxItem {
  label: string
  /** 禁用项置灰不可点,title 给原因 */
  disabled?: boolean
  /** 危险操作(删除)红色样式 */
  danger?: boolean
  title?: string
  action: () => void
}
</script>

<script setup lang="ts">
// ContextMenu.vue — 自绘右键菜单(TODO-20):webkit2gtk 原生菜单已全局抑制
// (main.ts),操作入口由此接管。定位在呼出坐标,挂载后按实际尺寸夹紧进视口
// 避免贴边溢出;Esc / 点外部 / 任意滚动关闭。
import { nextTick, onBeforeUnmount, onMounted, ref } from 'vue'

const props = defineProps<{ x: number; y: number; items: CtxItem[] }>()
const emit = defineEmits<{ close: [] }>()

const el = ref<HTMLElement | null>(null)
const pos = ref({ x: props.x, y: props.y })

function clamp() {
  const w = el.value?.offsetWidth ?? 0
  const h = el.value?.offsetHeight ?? 0
  pos.value = {
    x: Math.max(4, Math.min(props.x, window.innerWidth - w - 4)),
    y: Math.max(4, Math.min(props.y, window.innerHeight - h - 4)),
  }
}

function close() {
  emit('close')
}

function onDocMouseDown(e: MouseEvent) {
  // 菜单内的按下交给条目 click;外部按下(含右键换目标)一律先关,
  // 随后的 contextmenu 会在新目标上重新呼出
  if (el.value && e.target instanceof Node && !el.value.contains(e.target)) close()
}

function onKey(e: KeyboardEvent) {
  if (e.key === 'Escape') close()
}

function run(item: CtxItem) {
  if (item.disabled) return
  close()
  item.action()
}

onMounted(async () => {
  await nextTick()
  clamp()
  document.addEventListener('mousedown', onDocMouseDown, true)
  document.addEventListener('keydown', onKey, true)
  document.addEventListener('scroll', close, true)
})

onBeforeUnmount(() => {
  document.removeEventListener('mousedown', onDocMouseDown, true)
  document.removeEventListener('keydown', onKey, true)
  document.removeEventListener('scroll', close, true)
})
</script>

<template>
  <div
    ref="el"
    class="ctx"
    :style="{ left: pos.x + 'px', top: pos.y + 'px' }"
    @contextmenu.prevent.stop
  >
    <button
      v-for="it in items"
      :key="it.label"
      class="item"
      :class="{ danger: it.danger }"
      :disabled="it.disabled"
      :title="it.title || undefined"
      @click="run(it)"
    >
      {{ it.label }}
    </button>
  </div>
</template>

<style scoped>
.ctx {
  position: fixed;
  z-index: 55; /* 高于内容与对话框(50),低于 busy 遮罩(60)与 toast(80) */
  min-width: 160px;
  background: var(--panel-2);
  border: 1px solid var(--line);
  border-radius: var(--radius);
  box-shadow: 0 8px 24px rgba(0, 0, 0, 0.4);
  padding: 4px;
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.item {
  border: none;
  background: transparent;
  color: var(--text);
  text-align: left;
  padding: 7px 12px;
  border-radius: 6px;
  font-size: 13px;
}

.item:hover:not(:disabled) {
  background: var(--accent-dim);
}

.item.danger {
  color: var(--err);
}

.item.danger:hover:not(:disabled) {
  background: rgba(224, 82, 82, 0.15);
}

.item:disabled {
  color: var(--dim);
  opacity: 0.6;
  cursor: not-allowed;
}
</style>
