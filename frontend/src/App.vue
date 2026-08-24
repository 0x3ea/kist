<script setup lang="ts">
// App.vue — 外壳:未解锁时 Lock 全屏覆盖;解锁后底部导航三页切换。
// 页面状态驻 store,KeepAlive 保证切页不丢(Files 的浏览位置/选择集)。
import { onMounted } from 'vue'
import { store, init, loadFolder } from './store'
import Lock from './pages/Lock.vue'
import Files from './pages/Files.vue'
import Transfers from './pages/Transfers.vue'
import Settings from './pages/Settings.vue'
import Toast from './components/Toast.vue'
import { watch } from 'vue'

onMounted(() => {
  init()
})

// 首次解锁成功后进入根目录
watch(
  () => store.state.Unlocked,
  (now, was) => {
    if (now && !was) loadFolder(1)
  },
)
</script>

<template>
  <div class="shell">
    <template v-if="store.state.Unlocked">
      <main class="content">
        <KeepAlive>
          <Files v-if="store.page === 'files'" />
          <Transfers v-else-if="store.page === 'transfers'" />
          <Settings v-else />
        </KeepAlive>
      </main>
      <nav class="bottom-nav">
        <button
          v-for="p in (['files', 'transfers', 'settings'] as const)"
          :key="p"
          :class="{ active: store.page === p }"
          @click="store.page = p"
        >
          {{ p === 'files' ? '文件' : p === 'transfers' ? '传输' : '设置' }}
          <span v-if="p === 'files' && store.state.FileCount" class="badge">{{ store.state.FileCount }}</span>
        </button>
      </nav>
    </template>
    <Lock v-else />
    <Toast />
    <div v-if="store.busy" class="busy-overlay">{{ store.busy }}</div>
  </div>
</template>

<style scoped>
.shell {
  height: 100%;
  display: flex;
  flex-direction: column;
}

.content {
  flex: 1;
  overflow: hidden;
  display: flex;
  flex-direction: column;
}

.bottom-nav {
  display: flex;
  border-top: 1px solid var(--line);
  background: var(--panel);
}

.bottom-nav button {
  flex: 1;
  border: none;
  border-radius: 0;
  background: transparent;
  color: var(--dim);
  padding: 10px 0;
}

.bottom-nav button.active {
  color: var(--accent);
  box-shadow: inset 0 2px 0 var(--accent);
}

.badge {
  margin-left: 6px;
  background: var(--panel-2);
  border-radius: 9px;
  padding: 0 7px;
  font-size: 12px;
  color: var(--dim);
}

.busy-overlay {
  position: fixed;
  inset: 0;
  background: rgba(27, 38, 54, 0.55);
  display: flex;
  align-items: center;
  justify-content: center;
  color: var(--text);
  font-size: 15px;
  z-index: 60;
}
</style>
