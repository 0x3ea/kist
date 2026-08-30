<script setup lang="ts">
// MetaDialog.vue — 元数据编辑(TODO-16 目录 + TODO-17 文件泛化)。三不原则:
// 不继承、不合并、无告警;Note/Tags "改哪项发哪项",未动字段传 nil(undefined),
// 空串/空数组是显式清除——与后端指针语义一一对应。
// 文件形态的封面不走表单保存:导入/清除是即时操作(SetFileCover),pack 的
// 自动首页封面被顶掉后清除不恢复,两处确认框都明示。
import { onMounted, ref } from 'vue'
import { index } from '../../wailsjs/go/models'
import {
  ensureThumb,
  getFileMeta,
  getFolderMeta,
  pickImageFile,
  saveFileMeta,
  saveFolderMeta,
  setFileCover,
  store,
} from '../store'
import { fileKind } from '../fileIcon'

const props = defineProps<{
  mode: 'folder' | 'file'
  id: number
  name: string
  pack?: boolean // 文件形态:pack 的封面覆盖警示用
}>()
const emit = defineEmits<{ close: [] }>()

const note = ref('')
const tags = ref('')
const cover = ref('') // 目录形态:封面文件 ID 输入
const coverURL = ref('') // 文件形态:当前封面预览
const busy = ref(false)
// 记录初始值:保存时只把被改动的字段发出去(nil = 不动)
const orig = ref({ note: '', tags: '', cover: '' })

async function refreshCover() {
  coverURL.value = await ensureThumb(props.id)
}

onMounted(async () => {
  if (props.mode === 'folder') {
    const m = await getFolderMeta(props.id)
    if (!m) return
    note.value = m.Note
    tags.value = (m.Tags ?? []).join(', ')
    cover.value = m.CoverFileID ? String(m.CoverFileID) : ''
  } else {
    const m = await getFileMeta(props.id)
    if (!m) return
    note.value = m.Note
    tags.value = (m.Tags ?? []).join(', ')
    refreshCover()
  }
  orig.value = { note: note.value, tags: tags.value, cover: cover.value }
})

async function onSave() {
  const changed = { note: note.value !== orig.value.note, tags: tags.value !== orig.value.tags }
  if (props.mode === 'folder') {
    const upd = new index.FolderMetaUpdate()
    if (changed.note) upd.Note = note.value // 空串 = 清除
    if (changed.tags) {
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
    const ok = await saveFolderMeta(props.id, upd)
    busy.value = false
    if (ok) emit('close')
    return
  }
  const upd = new index.FileMetaUpdate()
  if (changed.note) upd.Note = note.value
  if (changed.tags) {
    upd.Tags = tags.value
      .split(/[,，]/)
      .map((t) => t.trim())
      .filter(Boolean)
  }
  if (!changed.note && !changed.tags) {
    emit('close') // 文件形态封面是即时操作,无改动即关
    return
  }
  busy.value = true
  const ok = await saveFileMeta(props.id, upd)
  busy.value = false
  if (ok) emit('close')
}

async function onImportCover() {
  if (props.pack && coverURL.value && !confirm('将替换 pack 自动生成的首页封面(清除后不恢复),继续?')) return
  const path = await pickImageFile()
  if (!path) return
  if (await setFileCover(props.id, path)) refreshCover()
}

async function onClearCover() {
  if (!coverURL.value) return
  const extra = props.pack ? 'pack 的自动首页封面不会恢复,' : ''
  if (!confirm(`清除封面?${extra}该文件将回落类型占位图。`)) return
  if (await setFileCover(props.id, '')) refreshCover()
}
</script>

<template>
  <div class="mask" @click.self="emit('close')">
    <div class="dialog">
      <h3>{{ mode === 'folder' ? '目录元数据' : '文件元数据' }}</h3>
      <p class="path">{{ name }}</p>
      <label>
        备注
        <textarea v-model="note" rows="3" placeholder="作品说明、作者等(可搜索)" />
      </label>
      <label>
        标签(逗号分隔)
        <input v-model="tags" type="text" placeholder="科幻, 已完结, 作者:某人" />
      </label>
      <label v-if="mode === 'folder'">
        封面文件 ID(0 = 清除,回退子条目拼贴;ID 见文件详情)
        <input v-model="cover" type="text" placeholder="0" />
      </label>
      <div v-else class="cover-row">
        <div class="cover-box">
          <img v-if="coverURL" :src="coverURL" alt="" />
          <component
            :is="fileKind(name, pack).icon"
            v-else
            class="cover-empty"
            :size="32"
            :stroke-width="1.5"
            :color="fileKind(name, pack).color"
          />
        </div>
        <div class="cover-ops">
          <button :disabled="busy" @click="onImportCover">导入封面…</button>
          <button :disabled="busy || !coverURL" @click="onClearCover">清除封面</button>
          <p class="hint">本地图片导入,随索引备份同步;mp4 等无自动缩略图的内容由此获得封面(epub 上传时已自动抽包内封面)。</p>
        </div>
      </div>
      <p class="hint">留空不变;备注/标签清空即删除;目录封面引用需为有缩略图的文件。</p>
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

/* 文件形态:封面预览 + 即时操作 */
.cover-row {
  display: flex;
  gap: 12px;
  align-items: stretch;
}

.cover-box {
  width: 96px;
  aspect-ratio: 2 / 3;
  border-radius: 8px;
  overflow: hidden;
  background: var(--panel-2);
  display: flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
}

.cover-box img {
  width: 100%;
  height: 100%;
  object-fit: cover;
}

.cover-empty {
  opacity: 0.7;
}

.cover-ops {
  display: flex;
  flex-direction: column;
  gap: 6px;
  justify-content: center;
}

.cover-ops .hint {
  margin-top: 4px;
}
</style>
