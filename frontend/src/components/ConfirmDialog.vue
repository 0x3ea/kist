<script setup lang="ts">
// ConfirmDialog.vue — 全局自绘确认框:store.confirmState 驱动,App.vue 挂载
// 一次,各处用 askConfirm() await 出布尔值,替代原生 confirm()。
// 键盘与原生对齐:Enter = 确定(打开时自动聚焦确认键),Esc = 取消;
// 点遮罩 = 取消。message 支持 \n(white-space: pre-line)。
import { nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { confirmState, settleConfirm } from '../store'

const confirmBtn = ref<HTMLButtonElement | null>(null)

watch(
  () => confirmState.open,
  (open) => {
    if (open) nextTick(() => confirmBtn.value?.focus())
  },
)

function onKey(e: KeyboardEvent) {
  if (!confirmState.open) return
  if (e.key === 'Escape') settleConfirm(false)
  else if (e.key === 'Enter') settleConfirm(true)
}
window.addEventListener('keydown', onKey)
onBeforeUnmount(() => window.removeEventListener('keydown', onKey))
</script>

<template>
  <div v-if="confirmState.open" class="mask" @click.self="settleConfirm(false)">
    <div class="dialog">
      <h3>{{ confirmState.title }}</h3>
      <p class="message">{{ confirmState.message }}</p>
      <div class="row">
        <button @click="settleConfirm(false)">取消</button>
        <button
          ref="confirmBtn"
          :class="confirmState.danger ? 'danger' : 'primary'"
          @click="settleConfirm(true)"
        >
          {{ confirmState.confirmText }}
        </button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.mask {
  position: fixed;
  inset: 0;
  background: rgba(0, 0, 0, 0.45);
  display: flex;
  align-items: center;
  justify-content: center;
  z-index: 50;
}

.dialog {
  width: 380px;
  background: var(--panel);
  border: 1px solid var(--line);
  border-radius: 12px;
  padding: 20px;
  display: flex;
  flex-direction: column;
  gap: 12px;
}

h3 {
  font-size: 14px;
}

.message {
  color: var(--dim);
  font-size: 13px;
  white-space: pre-line; /* 多行文案(如删除目录的提示)按 \n 换行 */
  word-break: break-all;
}

.row {
  display: flex;
  justify-content: flex-end;
  gap: 10px;
}
</style>
