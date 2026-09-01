<script setup lang="ts">
// App.vue — 外壳:未解锁时 Lock 全屏覆盖;解锁后左侧栏导航三页切换。
// 底部 tab 是移动端形态,桌面窗口改侧栏(品牌 + 导航 + 网盘名/锁定)。
// 页面状态驻 store,KeepAlive 保证切页不丢(Files 的浏览位置/选择集)。
import { onMounted, watch } from 'vue'
import { store, init, loadFolder, lock } from './store'
import { ArrowLeftRight, Box, FolderOpen, Lock, Settings as SettingsIcon } from 'lucide-vue-next'
import LockPage from './pages/Lock.vue'
import Files from './pages/Files.vue'
import Transfers from './pages/Transfers.vue'
import Settings from './pages/Settings.vue'
import Toast from './components/Toast.vue'
import ConfirmDialog from './components/ConfirmDialog.vue'
import SyncConflictDialog from './components/SyncConflictDialog.vue'

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

const navItems = [
  { page: 'files', label: '文件', icon: FolderOpen },
  { page: 'transfers', label: '传输', icon: ArrowLeftRight },
  { page: 'settings', label: '设置', icon: SettingsIcon },
] as const
</script>

<template>
  <div class="shell">
    <template v-if="store.state.Unlocked">
      <aside class="sidebar">
        <div class="brand">
          <Box :size="18" :stroke-width="2" />
          <span>kist</span>
        </div>
        <nav class="nav">
          <button
            v-for="p in navItems"
            :key="p.page"
            :class="{ active: store.page === p.page }"
            @click="store.page = p.page"
          >
            <component :is="p.icon" :size="16" />
            <span>{{ p.label }}</span>
            <em v-if="p.page === 'files' && store.state.FileCount" class="badge">{{ store.state.FileCount }}</em>
          </button>
        </nav>
        <div class="foot">
          <p v-if="store.state.DriveName" class="drive" :title="store.state.DriveName">{{ store.state.DriveName }}</p>
          <button class="lockbtn" title="擦除内存中的密钥" @click="lock">
            <Lock :size="14" />
            <span>锁定</span>
          </button>
        </div>
      </aside>
      <main class="content">
        <KeepAlive>
          <Files v-if="store.page === 'files'" />
          <Transfers v-else-if="store.page === 'transfers'" />
          <Settings v-else />
        </KeepAlive>
      </main>
    </template>
    <main v-else class="cover">
      <LockPage />
    </main>
    <Toast />
    <ConfirmDialog />
    <SyncConflictDialog />
    <div v-if="store.busy" class="busy-overlay">{{ store.busy }}</div>
  </div>
</template>

<style scoped>
.shell {
  height: 100%;
  display: flex;
}

/* 左侧栏:品牌 + 导航 + 网盘状态。主内容与其并排(桌面形态,
   替代原底部 tab——移动端布局在桌面窗口上比例失衡) */
.sidebar {
  width: 168px;
  flex-shrink: 0;
  background: var(--panel);
  border-right: 1px solid var(--line);
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 14px 10px 12px;
}

.brand {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 2px 10px 10px;
  font-size: 16px;
  font-weight: 700;
  letter-spacing: 0.4px;
}

.brand svg {
  color: var(--accent);
}

.nav {
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.nav button {
  display: flex;
  align-items: center;
  gap: 10px;
  background: transparent;
  border: none;
  border-radius: var(--radius);
  padding: 9px 10px;
  color: var(--dim);
  text-align: left;
}

.nav button:hover {
  background: var(--panel-2);
  color: var(--text);
}

.nav button.active {
  background: rgba(79, 140, 255, 0.12);
  color: var(--accent);
}

.badge {
  margin-left: auto;
  background: var(--panel-2);
  border-radius: 9px;
  padding: 0 7px;
  font-size: 12px;
  font-style: normal;
  color: var(--dim);
}

.foot {
  margin-top: auto;
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 0 2px;
}

.drive {
  color: var(--dim);
  font-size: 12px;
  padding: 0 8px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

/* 类名避讳:不能叫 .lock——scoped 样式会命中子组件(Lock 页)的根元素 */
.lockbtn {
  display: flex;
  align-items: center;
  gap: 8px;
  background: transparent;
  border: 1px solid var(--line);
  border-radius: var(--radius);
  color: var(--dim);
  padding: 7px 10px;
  font: inherit;
}

.lockbtn:hover {
  border-color: var(--err);
  color: var(--err);
}

.content {
  flex: 1;
  overflow: hidden;
  display: flex;
  flex-direction: column;
  min-width: 0;
}

/* 解锁前 Lock 全屏铺满:row 布局下页面根元素只声明了 height:100%,
   不接 flex:1 会按内容宽度缩成一列 */
.cover {
  flex: 1;
  min-width: 0;
  overflow: hidden;
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
