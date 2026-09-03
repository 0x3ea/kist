<script setup lang="ts">
// SyncConflictDialog.vue — 同步分叉裁决对话框(TODO-09,TODO-22 增强):
// store.syncConflict 驱动,App.vue 挂载一次。远端是哑 WebDAV,分叉由客户端
// 检出、人来裁决:
//   diverged     保留本机(覆盖远端)/ 保留云端(本机改动归档 backups/)/ 稍后
//   remote-ahead 只有远端动过,该拉不该推:只给"从远端恢复"与"稍后"
//                (此形态下"保留本机"等于用干净本机盖掉远端新改动,不提供)
// Esc / 点遮罩 = 稍后;keep-remote 走 store.resolveConflict(内含口令复核)。
// 打开时按需拉详情(SyncConflictDetail):文件级 diff 三栏只是帮判断的信息,
// 裁决粒度仍是整库二选一——不做行级合并;裁决时后端会先重检远端 revision,
// 已变化则刷新详情回到本框(stale 分支)。
import { computed, watch } from 'vue'
import { syncConflict, dismissConflict, resolveConflict, loadConflictDetail } from '../store'

// 每栏最多展开的路径数,超出折叠为"共 N 条"
const MAX_ROWS = 8

// diff 区只对分叉/远端领先有意义(local-ahead/in-sync 的零值 diff 不渲染,
// 免得"两侧一致"的文案误导);字段名对齐生成器口径(DiffResult camelCase)
const showDiff = computed(() => {
  const k = syncConflict.detail?.Kind ?? syncConflict.kind
  return k === 'diverged' || k === 'remote-ahead'
})
const diff = computed(() => syncConflict.detail?.Diff)
const columns = computed(() => {
  const d = diff.value
  if (!d) return []
  return [
    { title: '仅本机有', items: d.localOnly?.items ?? [], total: d.localOnly?.total ?? 0, tone: 'local' },
    { title: '仅云端有', items: d.remoteOnly?.items ?? [], total: d.remoteOnly?.total ?? 0, tone: 'remote' },
    { title: '两侧都有但内容不同', items: d.changed?.items ?? [], total: d.changed?.total ?? 0, tone: 'changed' },
  ].filter((c) => c.total > 0)
})
const diffEmpty = computed(() => !!diff.value && columns.value.length === 0)

watch(
  () => syncConflict.open,
  (open) => {
    if (open) loadConflictDetail()
  },
)

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

      <!-- 文件级 diff(TODO-22):三栏参考信息;按需加载,失败不阻塞裁决 -->
      <div v-if="showDiff" class="diff" :class="{ loading: syncConflict.detailLoading }">
        <template v-if="syncConflict.detailLoading">正在比对两侧文件清单…</template>
        <template v-else-if="!diff">差异清单拉取失败(仍可按上方说明裁决)。</template>
        <template v-else-if="diffEmpty">两侧文件清单一致(差异可能只在备注等元数据层)。</template>
        <template v-else>
          <div v-for="col in columns" :key="col.title" class="col" :class="col.tone">
            <div class="col-title">{{ col.title }}({{ col.total }})</div>
            <ul>
              <li v-for="p in col.items.slice(0, MAX_ROWS)" :key="p">{{ p }}</li>
            </ul>
            <div v-if="col.total > MAX_ROWS" class="more">…共 {{ col.total }} 条</div>
          </div>
        </template>
      </div>

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
  width: 520px;
  max-height: 80vh;
  overflow: auto;
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

.diff {
  display: flex;
  flex-direction: column;
  gap: 10px;
  font-size: 12px;
  color: var(--dim);
}

.diff.loading {
  opacity: 0.7;
}

.col {
  border: 1px solid var(--line);
  border-radius: var(--radius);
  padding: 8px 10px;
  display: flex;
  flex-direction: column;
  gap: 4px;
  min-width: 0;
}

.col-title {
  font-weight: 600;
  color: var(--text);
}

.col.local .col-title {
  color: var(--accent);
}

.col.remote .col-title {
  color: var(--ok);
}

.col.changed .col-title {
  color: var(--err);
}

.col ul {
  margin: 0;
  padding-left: 18px;
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.col li {
  word-break: break-all;
}

.more {
  color: var(--dim);
}

.row {
  display: flex;
  justify-content: flex-end;
  gap: 10px;
}
</style>
