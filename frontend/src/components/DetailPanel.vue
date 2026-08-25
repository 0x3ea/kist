<script setup lang="ts">
// DetailPanel.vue — 右侧详情:缩略图、虚拟路径、大小/密文、加密与上传时间、
// sha256、备注编辑(保存调 SetNote)。目录不走此面板(元数据另走对话框)。
import { ref, watch } from 'vue'
import { store, saveNote } from '../store'
import { fullTime, humanSize } from '../format'

const noteDraft = ref('')
const saving = ref(false)

watch(
  () => store.detail?.ID,
  () => (noteDraft.value = store.detail?.Note ?? ''),
  { immediate: true },
)

async function onSaveNote() {
  if (!store.detail) return
  saving.value = true
  await saveNote(store.detail.ID, noteDraft.value)
  saving.value = false
}
</script>

<template>
  <aside v-if="store.detail" class="panel">
    <div class="thumb">
      <img v-if="store.thumbs.get(store.detail.ID)" :src="store.thumbs.get(store.detail.ID)" alt="" />
      <div v-else class="no-thumb">{{ store.detail.Pack ? '📦' : '📄' }}</div>
    </div>
    <h3 :title="store.detail.Name">{{ store.detail.Name }}</h3>
    <p class="path">{{ store.detail.Path }}</p>
    <div v-if="store.detail.Tags?.length" class="tags">
      <span v-for="t in store.detail.Tags" :key="t" class="tag">#{{ t }}</span>
    </div>
    <dl>
      <dt>大小</dt>
      <dd>{{ humanSize(store.detail.Size) }}(密文 {{ humanSize(store.detail.CipherSize) }})</dd>
      <dt>类型</dt>
      <dd>{{ store.detail.Pack ? '打包(pack,一话一对象)' : '普通文件' }}</dd>
      <dt>状态</dt>
      <dd>{{ store.detail.State === 'ready' ? '已上传' : store.detail.State === 'uploading' ? '待上传(出站箱)' : '缺失' }}</dd>
      <dt>加密时间</dt>
      <dd>{{ fullTime(store.detail.EncryptedAt) }}</dd>
      <dt>上传时间</dt>
      <dd>{{ fullTime(store.detail.UploadedAt) }}</dd>
      <dt>SHA-256</dt>
      <dd class="sha" :title="store.detail.SHA256">{{ store.detail.SHA256 }}</dd>
    </dl>
    <label class="note">
      备注(可搜索)
      <textarea v-model="noteDraft" rows="3" />
      <button :disabled="saving" @click="onSaveNote">保存备注</button>
    </label>
  </aside>
</template>

<style scoped>
.panel {
  width: 250px;
  border-left: 1px solid var(--line);
  background: var(--panel);
  padding: 14px;
  overflow-y: auto;
  display: flex;
  flex-direction: column;
  gap: 10px;
  flex-shrink: 0;
}

.thumb {
  aspect-ratio: 1;
  border-radius: 8px;
  overflow: hidden;
  background: var(--panel-2);
  display: flex;
  align-items: center;
  justify-content: center;
}

.thumb img {
  width: 100%;
  height: 100%;
  object-fit: cover;
}

.no-thumb {
  font-size: 48px;
  opacity: 0.5;
}

h3 {
  font-size: 14px;
  word-break: break-all;
}

.path {
  color: var(--dim);
  font-size: 12px;
  word-break: break-all;
}

.tags {
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
}

.tag {
  color: var(--accent);
  font-size: 12px;
}

dl {
  font-size: 12px;
  display: grid;
  grid-template-columns: 62px 1fr;
  gap: 4px 8px;
}

dt {
  color: var(--dim);
}

dd {
  word-break: break-all;
}

.sha {
  font-family: monospace;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.note {
  display: flex;
  flex-direction: column;
  gap: 6px;
  font-size: 12px;
  color: var(--dim);
}

.note textarea {
  background: var(--bg);
  border: 1px solid var(--line);
  border-radius: var(--radius);
  color: var(--text);
  font: inherit;
  padding: 6px 8px;
  resize: vertical;
}

.note button {
  align-self: flex-start;
}
</style>
