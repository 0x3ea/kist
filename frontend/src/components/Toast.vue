<script setup lang="ts">
// Toast.vue — notify/错误的浮层提示宿主,数据在 store.toasts。
import { store } from '../store'
</script>

<template>
  <div class="toasts">
    <div v-for="t in store.toasts" :key="t.id" class="toast" :class="t.level">
      {{ t.text }}
    </div>
  </div>
</template>

<style scoped>
.toasts {
  position: fixed;
  top: 14px;
  left: 50%;
  transform: translateX(-50%);
  display: flex;
  flex-direction: column;
  gap: 8px;
  z-index: 80;
  max-width: 70%;
}

.toast {
  background: var(--panel-2);
  border: 1px solid var(--line);
  border-left: 3px solid var(--accent);
  border-radius: var(--radius);
  padding: 9px 14px;
  box-shadow: 0 4px 16px rgba(0, 0, 0, 0.35);
  word-break: break-all;
}

.toast.error {
  border-left-color: var(--err);
}
</style>
