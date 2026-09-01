<script setup lang="ts">
// SyncConflictDialog.vue — 同步分叉裁决对话框(TODO-09):store.syncConflict
// 驱动,App.vue 挂载一次。远端是哑 WebDAV,分叉由客户端检出、人来裁决:
//   diverged     保留本机(覆盖远端)/ 保留云端(本机改动归档 backups/)/ 稍后
//   remote-ahead 只有远端动过,该拉不该推:只给"从远端恢复"与"稍后"
//   (此形态下"保留本机"等于用干净本机盖掉远端新改动,不提供)
// Esc / 点遮罩 = 稍后;keep-remote 走 store.resolveConflict(内含口令复核)。
import { syncConflict, dismissConflict, resolveConflict } from '../store'

function later() {
  dismissConflict()
}
</script>

<template>
  <div v-if="syncConflict.open" class="mask" @click.self="later" @keydown.esc="later">
    <div class="dialog">
      <h3>同步{{ syncConflict.kind === 'remote-ahead' ? '提示' : '冲突' }}</h3>
      <p class="message">
        <template v-if="syncConflict.kind === 'remote-ahead'">
          远端索引已更新到 revision {{ syncConflict.remoteRev }},本机自上次同步(revision
          {{ syncConflict.baselineRev }})后没有改动。直接从远端恢复即可,无需裁决。
        </template>
        <template v-else>
          本机(revision {{ syncConflict.localRev }})与远端(revision {{ syncConflict.remoteRev }}
          <template v-if="syncConflict.remoteDevice">,来自设备 {{ syncConflict.remoteDevice }}</template
          >)在基线 revision {{ syncConflict.baselineRev }} 之后都有改动。必须选择保留哪一方,
          被放弃的一方不会丢失:本机改动归档在 backups/,远端版本留在网盘上可再次恢复。
        </template>
      </p>
      <div class="row">
        <button @click="later">稍后</button>
        <button
          v-if="syncConflict.kind !== 'remote-ahead'"
          class="danger"
          @click="resolveConflict('keep-remote')"
        >
          保留云端(本机归档)
        </button>
        <button
          :class="syncConflict.kind === 'remote-ahead' ? 'primary' : 'danger'"
          @click="syncConflict.kind === 'remote-ahead' ? resolveConflict('keep-remote') : resolveConflict('keep-local')"
        >
          {{ syncConflict.kind === 'remote-ahead' ? '从远端恢复' : '保留本机(覆盖远端)' }}
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
  z-index: 60; /* 高于 ConfirmDialog(50):冲突裁决优先 */
}

.dialog {
  width: 420px;
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
  white-space: pre-line;
  word-break: break-all;
}

.row {
  display: flex;
  justify-content: flex-end;
  gap: 10px;
}
</style>
