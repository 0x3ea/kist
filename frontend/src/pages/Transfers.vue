<script setup lang="ts">
// Transfers.vue — 传输页:进度列表 + 取消。数据源 store.transfers
// (transfers:changed 全量 / transfer:update 增量),初始 Transfers() 拉快照。
import { computed, ref } from 'vue'
import { store, cancelTransfer } from '../store'
import { humanSize, phaseText } from '../format'

const hideDone = ref(true)

function pct(t: { bytesDone: number; bytesTotal: number }): number {
  if (t.bytesTotal <= 0) return 0
  return Math.min(100, Math.round((t.bytesDone / t.bytesTotal) * 100))
}

function running(p: string): boolean {
  return ['queued', 'encrypting', 'uploading', 'downloading', 'decrypting'].includes(p)
}

const list = computed(() =>
  hideDone.value ? store.transfers.filter((t) => running(t.phase)) : store.transfers,
)
</script>

<template>
  <div class="transfers">
    <div class="head">
      <span>传输队列({{ store.transfers.filter((t) => running(t.phase)).length }} 个进行中)</span>
      <label class="check">
        <input v-model="hideDone" type="checkbox" />
        只看进行中
      </label>
    </div>
    <div class="list">
      <div v-for="t in list" :key="t.id" class="row" :class="t.phase">
        <div class="info">
          <span class="kind">{{ t.kind === 'upload' ? '↑' : '↓' }}</span>
          <span class="name" :title="t.name">{{ t.name }}</span>
          <span class="phase" :class="t.phase">{{ phaseText(t.phase) }}</span>
          <span class="err" v-if="t.err" :title="t.err">{{ t.err }}</span>
        </div>
        <div class="bar-wrap">
          <div class="bar">
            <div class="fill" :class="{ indet: running(t.phase) && t.bytesTotal <= 0 }" :style="`width:${pct(t)}%`" />
          </div>
          <span class="bytes">
            {{ humanSize(t.bytesDone) }}<template v-if="t.bytesTotal > 0"> / {{ humanSize(t.bytesTotal) }}</template>
          </span>
          <button v-if="running(t.phase)" class="cancel" @click="cancelTransfer(t.id)">取消</button>
        </div>
      </div>
      <div v-if="list.length === 0" class="empty">没有{{ hideDone ? '进行中的' : '' }}传输</div>
    </div>
  </div>
</template>

<style scoped>
.transfers {
  height: 100%;
  display: flex;
  flex-direction: column;
}

.head {
  display: flex;
  justify-content: space-between;
  align-items: center;
  padding: 10px 14px;
  color: var(--dim);
  border-bottom: 1px solid var(--line);
}

.check {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 13px;
  color: var(--dim);
  cursor: pointer;
}

.check input {
  width: auto;
}

.list {
  flex: 1;
  overflow: auto;
  padding: 6px 14px;
}

.row {
  padding: 10px 0;
  border-bottom: 1px solid var(--line);
}

.info {
  display: flex;
  gap: 10px;
  align-items: baseline;
  margin-bottom: 6px;
  font-size: 13px;
}

.kind {
  color: var(--dim);
}

.name {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  max-width: 50%;
}

.phase {
  color: var(--dim);
  flex-shrink: 0;
}

.phase.done {
  color: var(--ok);
}

.phase.error,
.phase.canceled {
  color: var(--err);
}

.err {
  color: var(--err);
  font-size: 12px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.bar-wrap {
  display: flex;
  align-items: center;
  gap: 10px;
}

.bar {
  flex: 1;
  height: 6px;
  border-radius: 3px;
  background: var(--panel-2);
  overflow: hidden;
}

.fill {
  height: 100%;
  background: var(--accent);
  transition: width 0.2s;
}

.row.error .fill,
.row.canceled .fill {
  background: var(--err);
}

.row.done .fill {
  background: var(--ok);
}

/* 总量未知(如排队/加密早期)的流动效果 */
.fill.indet {
  width: 30% !important;
  animation: indet 1.2s infinite alternate linear;
}

@keyframes indet {
  from {
    transform: translateX(-40%);
  }
  to {
    transform: translateX(230%);
  }
}

.bytes {
  color: var(--dim);
  font-size: 12px;
  min-width: 110px;
  text-align: right;
  flex-shrink: 0;
}

.cancel {
  padding: 3px 10px;
  font-size: 12px;
  flex-shrink: 0;
}

.empty {
  text-align: center;
  color: var(--dim);
  padding: 40px 0;
}
</style>
