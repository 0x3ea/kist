<script setup lang="ts">
// DetailPanel.vue — 右侧详情,文件与目录共用一个面板(交互约定:单击条目 =
// 详情、双击目录 = 进入;目录的标签/封面等完整编辑仍走右键「元数据」
// 对话框,面板只做速览 + 备注编辑):
//   文件:缩略图、虚拟路径、大小/密文、加密与上传时间、sha256、备注编辑
//   目录:封面(自有封面满铺 > 子条目宫格 > 图标)、tag、子树摘要、备注编辑
// 面板常驻占位(v-if 在内容而非面板上):若选中才挂载,网格会因右栏突然
// 出现而重排列数,卡片在点击瞬间变宽——点击目标漂移,观感突兀。
import { computed, ref, watch } from 'vue'
import { ensureFolderCover, ensureThumb, saveFolderNote, saveNote, store } from '../store'
import { fullTime, humanSize } from '../format'
import { fileKind, folderKind } from '../fileIcon'
import CoverMosaic from './CoverMosaic.vue'
import { MousePointerClick } from 'lucide-vue-next'

const noteDraft = ref('')
const saving = ref(false)

const fd = computed(() => store.folderDetail)

// 文件与目录详情共用一份备注草稿:详情主体(文件 ID / 目录 ID,两侧互斥)
// 切换时重置;Note 值也在监听列里——元数据对话框与面板备注保存都会回写
// 详情快照,草稿要跟上,免得面板停留旧值、一保存又把新值顶回去
watch(
  () => [store.detail?.ID, store.detail?.Note, fd.value?.id, fd.value?.meta.Note] as const,
  () => {
    noteDraft.value = store.detail ? store.detail.Note : (fd.value?.meta.Note ?? '')
  },
  { immediate: true },
)

// 目录封面:自有封面(summary.CustomCover)取字节满铺,字节按 folderID 走
// GetFolderCover(v6,与文件缩略图两条路);派生宫格补预取——列表视图目录行
// 不经过 CardGrid 的可见优先预取,面板里第一次看必须自己取;两个 ensure
// 都自带缓存/负缓存/限流,重复触发无代价
const folderCoverURL = ref('')
watch(
  () => [fd.value?.id, fd.value?.summary?.CustomCover] as const,
  async ([id, custom]) => {
    folderCoverURL.value = ''
    if (!id) return
    for (const cid of fd.value?.summary?.CoverFileIDs ?? []) if (cid) ensureThumb(cid)
    if (!custom) return
    const url = await ensureFolderCover(id)
    // 取图在途时详情可能已切走
    if (fd.value?.id === id && fd.value.summary?.CustomCover) folderCoverURL.value = url
  },
  { immediate: true },
)

// 目录摘要行:话数/文件数与 CLI summaryLine 同措辞,pack 目录两者都给
function scaleText(s: { PackCount: number; FileCount: number }): string {
  if (s.PackCount > 0) return `${s.PackCount} 话 / ${s.FileCount} 文件`
  if (s.FileCount > 0) return `${s.FileCount} 文件`
  return '空目录'
}

const folderRows = computed(() => {
  const rows: Array<[string, string]> = [['类型', '目录']]
  const s = fd.value?.summary
  if (s) {
    rows.push(['规模', scaleText(s)])
    if (s.TotalSize > 0) rows.push(['总大小', humanSize(s.TotalSize)])
    if (s.LatestAt > 0) rows.push(['最近更新', fullTime(s.LatestAt)])
    if (s.PendingCount > 0) rows.push(['待上传', `${s.PendingCount} 个(出站箱)`])
  }
  return rows
})

async function onSaveNote() {
  if (fd.value) {
    saving.value = true
    await saveFolderNote(fd.value.id, noteDraft.value)
    saving.value = false
    return
  }
  if (!store.detail) return
  saving.value = true
  await saveNote(store.detail.ID, noteDraft.value)
  saving.value = false
}
</script>

<template>
  <aside class="panel">
    <div v-if="!store.detail && !fd" class="empty">
      <MousePointerClick :size="28" :stroke-width="1.5" />
      <p>点击条目查看详情</p>
    </div>
    <template v-else-if="fd">
      <div class="thumb">
        <img v-if="folderCoverURL" :src="folderCoverURL" alt="" />
        <CoverMosaic v-else-if="fd.summary?.CoverFileIDs?.length" :ids="fd.summary.CoverFileIDs" />
        <component :is="folderKind.icon" v-else class="no-thumb" :size="56" :stroke-width="1.25" :color="folderKind.color" />
      </div>
      <h3 :title="fd.name">{{ fd.name }}</h3>
      <p class="path">{{ fd.path }}</p>
      <div v-if="fd.meta.Tags?.length" class="tags">
        <span v-for="t in fd.meta.Tags" :key="t" class="tag">#{{ t }}</span>
      </div>
      <dl>
        <template v-for="[k, v] in folderRows" :key="k">
          <dt>{{ k }}</dt>
          <dd>{{ v }}</dd>
        </template>
      </dl>
      <label class="note">
        备注(可搜索)
        <textarea v-model="noteDraft" rows="3" />
        <button :disabled="saving" @click="onSaveNote">保存备注</button>
      </label>
      <p class="hint">标签与封面:右键目录 →「元数据」</p>
    </template>
    <template v-else-if="store.detail">
    <div class="thumb">
      <img v-if="store.thumbs.get(store.detail.ID)" :src="store.thumbs.get(store.detail.ID)" alt="" />
      <component
        :is="fileKind(store.detail.Name, store.detail.Pack).icon"
        v-else
        class="no-thumb"
        :size="56"
        :stroke-width="1.25"
        :color="fileKind(store.detail.Name, store.detail.Pack).color"
      />
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
    </template>
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

/* 空态:居中提示,面板不因无选中而塌缩 */
.empty {
  margin: auto;
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 8px;
  color: var(--dim);
  font-size: 12px;
  opacity: 0.7;
}

.thumb {
  aspect-ratio: 2 / 3;
  /* 禁止参与 flex 压缩:面板内容超高时缩略图盒会被沿主轴压扁,
     cover 裁出扁条;宁可靠面板滚动保住 2:3 */
  flex-shrink: 0;
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
  opacity: 0.75;
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

.hint {
  color: var(--dim);
  font-size: 12px;
}
</style>
