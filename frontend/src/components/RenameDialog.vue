<script setup lang="ts">
// RenameDialog.vue — 目录重命名(TODO-19):纯索引零流量。预填当前名并全选;
// 同名提交是幂等 no-op;撞名后端报错走 toast(不自动消解——显式单发动作)。
import { nextTick, ref } from 'vue'
import { renameFolder, store } from '../store'

const props = defineProps<{ folderID: number; currentName: string }>()
const emit = defineEmits<{ close: [] }>()

const name = ref(props.currentName)
const busy = ref(false)
const inputEl = ref<HTMLInputElement | null>(null)

// 打开即聚焦全选,直接输入即覆盖
nextTick(() => inputEl.value?.select())

async function onRename() {
  const n = name.value.trim()
  if (!n || n === props.currentName) {
    emit('close') // 空名拦截;未改动直接关
    return
  }
  if (n.includes('/')) {
    store.toasts.push({ id: Date.now(), level: 'error', text: '目录名不能含 "/"(多级请用新建文件夹或分次改名)' })
    return
  }
  busy.value = true
  const ok = await renameFolder(props.folderID, n)
  busy.value = false
  if (ok) emit('close')
}
</script>

<template>
  <div class="mask" @click.self="emit('close')">
    <div class="dialog">
      <h3>重命名目录</h3>
      <p class="path">{{ currentName }}</p>
      <input
        ref="inputEl"
        v-model="name"
        type="text"
        placeholder="新名称"
        @keydown.enter="onRename"
      />
      <p class="hint">纯索引操作,零远端流量;同级已有同名目录会报错(不自动加后缀)。</p>
      <div class="row">
        <button @click="emit('close')">取消</button>
        <button class="primary" :disabled="busy || !name.trim()" @click="onRename">重命名</button>
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
  gap: 10px;
}

.path {
  color: var(--dim);
  font-size: 13px;
  margin-top: -6px;
  word-break: break-all;
}

input {
  background: var(--bg);
  border: 1px solid var(--line);
  border-radius: var(--radius);
  color: var(--text);
  font: inherit;
  padding: 6px 8px;
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
