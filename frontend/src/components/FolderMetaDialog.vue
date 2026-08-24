<script setup lang="ts">
// FolderMetaDialog.vue — 目录元数据编辑(TODO-16 三不原则:不继承、不合并、
// 无告警)。Note/Tags/Cover 全部"改哪项发哪项":未动的字段传 nil(undefined),
// 空串/空数组/0 是显式清除——与后端 index.FolderMetaUpdate 指针语义一一对应。
import { onMounted, ref } from 'vue'
import { index } from '../../wailsjs/go/models'
import { getFolderMeta, saveFolderMeta, store } from '../store'

const props = defineProps<{ folderID: number; name: string }>()
const emit = defineEmits<{ close: [] }>()

const note = ref('')
const tags = ref('')
const cover = ref('')
const busy = ref(false)
// 记录初始值:保存时只把被改动的字段发出去(nil = 不动)
const orig = ref({ note: '', tags: '', cover: '' })

onMounted(async () => {
  const m = await getFolderMeta(props.folderID)
  if (!m) return
  note.value = m.Note
  tags.value = (m.Tags ?? []).join(', ')
  cover.value = m.CoverFileID ? String(m.CoverFileID) : ''
  orig.value = { note: note.value, tags: tags.value, cover: cover.value }
})

async function onSave() {
  const upd = new index.FolderMetaUpdate()
  if (note.value !== orig.value.note) {
    upd.Note = note.value // 空串 = 清除
  }
  if (tags.value !== orig.value.tags) {
    upd.Tags = tags.value
      .split(/[,，]/)
      .map((t) => t.trim())
      .filter(Boolean)
  }
  if (cover.value !== orig.value.cover) {
    const n = Number(cover.value.trim() || '0')
    if (Number.isNaN(n) || n < 0) {
      store.toasts.push({ id: Date.now(), level: 'error', text: '封面引用必须是文件 ID 数字(0 = 清除)' })
      return
    }
    upd.Cover = n // 0 = 清除回退拼贴
  }
  busy.value = true
  const ok = await saveFolderMeta(props.folderID, upd)
  busy.value = false
  if (ok) emit('close')
}
</script>

<template>
  <div class="mask" @click.self="emit('close')">
    <div class="dialog">
      <h3>目录元数据</h3>
      <p class="path">{{ name }}</p>
      <label>
        备注
        <textarea v-model="note" rows="3" placeholder="作品说明、作者等(可搜索)" />
      </label>
      <label>
        标签(逗号分隔)
        <input v-model="tags" type="text" placeholder="科幻, 已完结" />
      </label>
      <label>
        封面文件 ID(0 = 清除,回退子条目拼贴;ID 见文件详情)
        <input v-model="cover" type="text" placeholder="0" />
      </label>
      <p class="hint">留空不变;备注/标签清空即删除;封面引用需为有缩略图的文件。</p>
      <div class="row">
        <button @click="emit('close')">取消</button>
        <button class="primary" :disabled="busy" @click="onSave">保存</button>
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
  width: 420px;
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
}

label {
  display: flex;
  flex-direction: column;
  gap: 4px;
  font-size: 13px;
  color: var(--dim);
}

textarea {
  background: var(--bg);
  border: 1px solid var(--line);
  border-radius: var(--radius);
  color: var(--text);
  font: inherit;
  padding: 6px 8px;
  resize: vertical;
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
