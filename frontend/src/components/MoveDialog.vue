<script setup lang="ts">
// MoveDialog.vue — 移动所选文件到目标虚拟目录(纯索引,零远端流量)。
// 路径从根写起,不存在的层级会隐式创建(与 CLI put --dest 同款宽容)。
import { ref } from 'vue'
import { store, moveSelected } from '../store'

const emit = defineEmits<{ close: [] }>()
const path = ref('')
const busy = ref(false)

async function onMove() {
  if (!path.value.trim()) return
  busy.value = true
  const ok = await moveSelected(path.value.trim())
  busy.value = false
  if (ok) emit('close')
}
</script>

<template>
  <div class="mask" @click.self="emit('close')">
    <div class="dialog">
      <h3>移动 {{ store.selection.size }} 个文件</h3>
      <p class="hint">目标虚拟目录(从根写起,如 /合集B/续章;不存在会自动创建)</p>
      <input v-model="path" type="text" placeholder="/目标目录" @keyup.enter="onMove" />
      <div class="row">
        <button @click="emit('close')">取消</button>
        <button class="primary" :disabled="busy || !path.trim()" @click="onMove">移动</button>
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
  width: 400px;
  background: var(--panel);
  border: 1px solid var(--line);
  border-radius: 12px;
  padding: 20px;
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.hint {
  color: var(--dim);
  font-size: 12px;
}

.row {
  display: flex;
  justify-content: flex-end;
  gap: 10px;
}
</style>
