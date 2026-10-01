<script setup lang="ts">
// MetaDialog.vue — 元数据编辑(TODO-16 目录 + TODO-17 文件泛化)。三不原则:
// 不继承、不合并、无告警;Note/Tags "改哪项发哪项",未动字段传 nil(undefined),
// 空串/空数组是显式清除——与后端指针语义一一对应。
// 封面不走表单保存:目录与文件一样是即时导入/清除操作(v6 起目录封面
// 持有式,从本地图片导入而非引用库内文件);pack 的自动首页封面被顶掉后
// 清除不恢复,确认框明示。
import { onMounted, ref } from 'vue'
import { index } from '../../wailsjs/go/models'
import {
  askConfirm,
  ensureFolderCover,
  ensureThumb,
  getFileMeta,
  getFolderMeta,
  openFolderDetail,
  pickImageFile,
  saveFileMeta,
  saveFolderMeta,
  setFileCover,
  setFolderCover,
  store,
} from '../store'
import { fileKind, folderKind } from '../fileIcon'

const props = defineProps<{
  mode: 'folder' | 'file'
  id: number
  name: string
  pack?: boolean // 文件形态:pack 的封面覆盖警示用
}>()
const emit = defineEmits<{ close: [] }>()

const note = ref('')
const tags = ref('')
const coverURL = ref('') // 当前封面预览(目录与文件各自按属主 id 取)
const busy = ref(false)
// 记录初始值:保存时只把被改动的字段发出去(nil = 不动)
const orig = ref({ note: '', tags: '' })

async function refreshCover() {
  coverURL.value = props.mode === 'folder' ? await ensureFolderCover(props.id) : await ensureThumb(props.id)
}

onMounted(async () => {
  if (props.mode === 'folder') {
    const m = await getFolderMeta(props.id)
    if (!m) return
    note.value = m.Note
    tags.value = (m.Tags ?? []).join(', ')
  } else {
    const m = await getFileMeta(props.id)
    if (!m) return
    note.value = m.Note
    tags.value = (m.Tags ?? []).join(', ')
  }
  refreshCover()
  orig.value = { note: note.value, tags: tags.value }
})

async function onSave() {
  const changed = { note: note.value !== orig.value.note, tags: tags.value !== orig.value.tags }
  if (!changed.note && !changed.tags) {
    emit('close') // 封面是即时操作,元数据无改动即关
    return
  }
  busy.value = true
  const ok =
    props.mode === 'folder'
      ? await saveFolderMeta(props.id, folderUpdate(changed))
      : await saveFileMeta(props.id, fileUpdate(changed))
  busy.value = false
  if (ok) {
    // 右栏目录详情若正展示该目录,重拉快照,面板与保存结果保持一致
    if (props.mode === 'folder' && store.folderDetail?.id === props.id) openFolderDetail(props.id)
    emit('close')
  }
}

// 拆分构造更新请求:Note 空串 = 清除;Tags 全量覆盖(逗号/全角逗号分隔)
function folderUpdate(changed: { note: boolean; tags: boolean }) {
  const upd = new index.FolderMetaUpdate()
  if (changed.note) upd.Note = note.value
  if (changed.tags) upd.Tags = parseTags(tags.value)
  return upd
}

function fileUpdate(changed: { note: boolean; tags: boolean }) {
  const upd = new index.FileMetaUpdate()
  if (changed.note) upd.Note = note.value
  if (changed.tags) upd.Tags = parseTags(tags.value)
  return upd
}

function parseTags(s: string): string[] {
  return s
    .split(/[,，]/)
    .map((t) => t.trim())
    .filter(Boolean)
}

async function onImportCover() {
  if (
    props.pack &&
    coverURL.value &&
    !(await askConfirm({
      title: '替换封面',
      message: '将替换 pack 自动生成的首页封面(清除后不恢复),继续?',
      danger: true,
      confirmText: '替换',
    }))
  )
    return
  const path = await pickImageFile()
  if (!path) return
  if (props.mode === 'folder') {
    if (await setFolderCover(props.id, path)) refreshCover()
  } else if (await setFileCover(props.id, path)) refreshCover()
}

async function onClearCover() {
  if (!coverURL.value) return
  const extra = props.pack ? 'pack 的自动首页封面不会恢复,' : ''
  if (
    !(await askConfirm({
      title: '清除封面',
      message:
        props.mode === 'folder'
          ? `清除封面?${extra}该目录将回退子条目拼贴。`
          : `清除封面?${extra}该文件将回落类型占位图。`,
      danger: props.pack, // 普通封面可随时重导;pack 自动首页顶掉后不可恢复
      confirmText: '清除',
    }))
  )
    return
  const ok = props.mode === 'folder' ? await setFolderCover(props.id, '') : await setFileCover(props.id, '')
  if (ok) refreshCover()
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
      <div class="cover-row">
        <div class="cover-box">
          <img v-if="coverURL" :src="coverURL" alt="" />
          <component
            :is="mode === 'folder' ? folderKind.icon : fileKind(name, pack).icon"
            v-else
            class="cover-empty"
            :size="32"
            :stroke-width="1.5"
            :color="mode === 'folder' ? folderKind.color : fileKind(name, pack).color"
          />
        </div>
        <div class="cover-ops">
          <button :disabled="busy" @click="onImportCover">导入封面…</button>
          <button :disabled="busy || !coverURL" @click="onClearCover">清除封面</button>
          <p class="hint">
            {{
              mode === 'folder'
                ? '本地图片导入,随索引备份同步;已设封面会被替换,清除后回退子条目拼贴。'
                : '本地图片导入,随索引备份同步;mp4 等无自动缩略图的内容由此获得封面(epub 上传时已自动抽包内封面)。'
            }}
          </p>
        </div>
      </div>
      <p class="hint">留空不变;备注/标签清空即删除。</p>
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

/* 封面预览 + 即时操作(目录与文件共用) */
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
