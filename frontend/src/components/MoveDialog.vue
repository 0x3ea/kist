<script setup lang="ts">
// MoveDialog.vue — 移动所选文件/目录:内嵌迷你目录选择器(纯索引,零远端流量)。
// 点条目进入、点面包屑返回、"移动到这里"落定;可就地新建文件夹并直接进入。
// 待移的文件夹本身不可进入(移进自身子树后端也会拒,前端挡掉最常见的一步);
// 更深的环(如移进待移目录的孙级)由索引层 MoveEntries 拒绝并经 toast 报错。
import { computed, onMounted, ref } from 'vue'
import { store, moveSelected, peekFolders, ensureFolderAt } from '../store'
import { folderKind } from '../fileIcon'

const emit = defineEmits<{ close: [] }>()

// 选择器当前位置:id + 从根到该处的面包屑(name 空串 = 根)
const curID = ref(1)
const crumbs = ref<{ id: number; name: string }[]>([])
const folders = ref<{ id: number; name: string }[]>([])
const loading = ref(true)
const busy = ref(false)
const newName = ref('')

// 落点默认 Files 页当前目录——移动多发生在就近目录之间;
// ListFolder 自带 Crumbs,面包屑零成本派生
async function goto(id: number) {
  loading.value = true
  const v = await peekFolders(id)
  loading.value = false
  if (!v) return
  curID.value = id
  crumbs.value = v.crumbs.map((c) => ({ id: c.ID, name: c.Name }))
  folders.value = v.folders.map((f) => ({ id: f.ID, name: f.Name }))
}
onMounted(() => goto(store.folder.id))

function enter(f: { id: number; name: string }) {
  if (store.folderSelection.has(f.id)) return // 待移目录不可进入
  goto(f.id)
}

const summary = computed(() => {
  const parts: string[] = []
  if (store.selection.size > 0) parts.push(`${store.selection.size} 个文件`)
  if (store.folderSelection.size > 0) parts.push(`${store.folderSelection.size} 个目录`)
  return `移动 ${parts.join('、')}`
})

// 当前位置的虚拟路径(根段为空串名,拼出来就是 "/"),建目录用
const here = computed(() => '/' + crumbs.value.map((c) => c.name).filter(Boolean).join('/'))

async function onCreate() {
  const name = newName.value.trim()
  if (!name) return
  const id = await ensureFolderAt(`${here.value}/${name}`)
  newName.value = ''
  if (id) goto(id) // 建好直接进入,贴合"移进新文件夹"的动线
}

async function onMove() {
  busy.value = true
  const ok = await moveSelected(curID.value)
  busy.value = false
  if (ok) emit('close')
}
</script>

<template>
  <div class="mask" @click.self="emit('close')">
    <div class="dialog">
      <h3>{{ summary }}</h3>
      <div class="crumbs">
        <template v-for="(c, i) in crumbs" :key="c.id">
          <span class="crumb" :class="{ last: i === crumbs.length - 1 }" @click="goto(c.id)">
            {{ c.name || '根' }}
          </span>
          <span v-if="i < crumbs.length - 1" class="sep">/</span>
        </template>
      </div>
      <div class="list">
        <div v-if="loading" class="empty">读取中…</div>
        <template v-else>
          <div v-if="folders.length === 0" class="empty">空目录——可直接「移动到这里」</div>
          <div
            v-for="f in folders"
            :key="f.id"
            class="row"
            :class="{ disabled: store.folderSelection.has(f.id) }"
            :title="store.folderSelection.has(f.id) ? '待移目录不可移进自身' : ''"
            @click="enter(f)"
          >
            <span class="kind"><component :is="folderKind.icon" :size="15" :color="folderKind.color" /></span>
            <span class="name">{{ f.name }}</span>
          </div>
        </template>
      </div>
      <div class="newrow">
        <input v-model="newName" type="text" placeholder="在新位置新建文件夹(可多级 a/b)" @keyup.enter="onCreate" />
        <button :disabled="!newName.trim()" @click="onCreate">新建并进入</button>
      </div>
      <div class="actions">
        <button @click="emit('close')">取消</button>
        <button class="primary" :disabled="busy || loading" @click="onMove">移动到这里</button>
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

.crumbs {
  color: var(--dim);
  font-size: 13px;
  display: flex;
  gap: 4px;
  flex-wrap: wrap;
}

.crumb {
  cursor: pointer;
}

.crumb:hover {
  color: var(--accent);
}

.crumb.last {
  color: var(--text);
  cursor: default;
}

.sep {
  opacity: 0.5;
}

.list {
  max-height: 240px;
  overflow: auto;
  border: 1px solid var(--line);
  border-radius: 8px;
  min-height: 64px;
}

.row {
  display: flex;
  gap: 8px;
  align-items: center;
  padding: 7px 10px;
  cursor: pointer;
  font-size: 13px;
}

.row:hover {
  background: var(--panel-2);
}

.row.disabled {
  opacity: 0.45;
  cursor: not-allowed;
}

.kind {
  flex: 0 0 18px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
}

.name {
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.empty {
  text-align: center;
  color: var(--dim);
  font-size: 13px;
  padding: 20px 0;
}

.newrow {
  display: flex;
  gap: 8px;
}

.newrow input {
  flex: 1;
}

.actions {
  display: flex;
  justify-content: flex-end;
  gap: 10px;
}
</style>
