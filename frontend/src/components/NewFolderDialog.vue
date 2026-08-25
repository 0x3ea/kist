<script setup lang="ts">
// NewFolderDialog.vue — 在当前目录下新建虚拟文件夹(TODO-18)。
// 纯索引操作零远端流量;输入允许 "a/b" 多级(EnsureFolder 逐级建);
// 重名幂等返回已有目录,不报错——与 CLI mkdir 的 mkdir -p 语义一致。
import { ref } from 'vue'
import { createFolder, loadFolder, store } from '../store'

const emit = defineEmits<{ close: [] }>()

const name = ref('')
const busy = ref(false)

// 面包屑拼当前位置(根目录名约定为空串,跳过)
const here = store.folder.crumbs
  .slice(1)
  .map((c) => c.Name)
  .join('/')

async function onCreate() {
  const n = name.value.trim().replace(/^\/+|\/+$/g, '')
  if (!n) return
  busy.value = true
  const ok = await createFolder(n)
  busy.value = false
  if (ok) {
    await loadFolder(store.folder.id)
    emit('close')
  }
}
</script>

<template>
  <div class="mask" @click.self="emit('close')">
    <div class="dialog">
      <h3>新建文件夹</h3>
      <p class="path">位置:{{ '/' + (here || '') }}</p>
      <input
        v-model="name"
        type="text"
        placeholder="名称(可含 / 建多级,如 作者A/书名)"
        @keydown.enter="onCreate"
      />
      <p class="hint">纯索引操作,零远端流量;已存在则直接复用(幂等)。</p>
      <div class="row">
        <button @click="emit('close')">取消</button>
        <button class="primary" :disabled="busy || !name.trim()" @click="onCreate">创建</button>
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
