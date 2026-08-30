<script setup lang="ts">
// Settings.vue — 设置页:网盘档案列表(一盘一库,TODO-21)、行为偏好
// (并发/块大小/自动备份/大小混淆)、立即备份、孤儿清理两步(预览→确认)、
// 改口令、锁定。改并发/块大小对进行中传输的下一任务生效(管线闭包每轮重读)。
import { onMounted, reactive, ref } from 'vue'
import { config, main } from '../../wailsjs/go/models'
import {
  store,
  backupNow,
  changePassphrase,
  deleteDrive,
  activateDrive,
  loadSettings,
  lock,
  previewGC,
  runGC,
  saveDrive,
  saveSettings,
  testConnection,
  askConfirm,
  type DriveForm,
} from '../store'
import { fullTime } from '../format'

// ---- 网盘档案(TODO-21):列表 + 增/改/删/切换/测试 ----
const showForm = ref(false)
const form = reactive<DriveForm>({
  id: '',
  name: '',
  url: '',
  username: '',
  password: '',
  rootPath: '/kist',
  rememberPassword: false,
})
const formMsg = ref('')
const formOk = ref(false)

function onAddDrive() {
  Object.assign(form, { id: '', name: '', url: '', username: '', password: '', rootPath: '/kist', rememberPassword: false })
  formMsg.value = ''
  showForm.value = true
}

function onEditDrive(d: main.DriveInfo) {
  // 密码仅"记住密码"的档案有回显(未记的本就没存)
  Object.assign(form, {
    id: d.ID,
    name: d.Name,
    url: d.URL,
    username: d.Username,
    password: d.Password ?? '',
    rootPath: d.RootPath,
    rememberPassword: d.RememberPassword,
  })
  formMsg.value = ''
  showForm.value = true
}

async function onTestDrive() {
  formMsg.value = ''
  const r = await testConnection(
    new main.WebDAVConfig({
      URL: form.url,
      Username: form.username,
      Password: form.password,
      RootPath: form.rootPath,
      RememberPassword: form.rememberPassword,
    }),
  )
  formOk.value = r.Ok
  formMsg.value = r.Detail
}

async function onSaveDrive() {
  if (await saveDrive({ ...form })) showForm.value = false
}

async function onDeleteDrive(d: main.DriveInfo) {
  if (
    !(await askConfirm({
      title: '删除档案',
      message: `删除档案「${d.Name}」?\n远端数据不动;本地索引文件保留在 KIST_HOME 下(index-${d.ID}.db)。`,
      danger: true,
      confirmText: '删除',
    }))
  )
    return
  await deleteDrive(d.ID)
}

async function onActivateDrive(d: main.DriveInfo) {
  if (
    !(await askConfirm({
      title: '切换网盘',
      message: `切换到「${d.Name}」?\n当前盘落后的索引会先补一次备份;有在途传输时切换会被拒绝;切换后文件列表换成本盘的库(口令不变,无需重新解锁)。`,
      confirmText: '切换',
    }))
  )
    return
  await activateDrive(d.ID)
}

const settings = reactive({
  concurrency: 2,
  chunk_mib: 4,
  auto_backup: true,
  size_padding: 'on',
  cover_cache_mb: 512,
})

onMounted(async () => {
  await loadSettings()
  if (store.settings) {
    settings.concurrency = store.settings.concurrency
    settings.chunk_mib = store.settings.chunk_mib
    settings.auto_backup = store.settings.auto_backup
    settings.size_padding = store.settings.size_padding
    settings.cover_cache_mb = store.settings.cover_cache_mb
  }
})

async function onSaveSettings() {
  // 展开 store.settings 保留表单未覆盖的字段(remember_password/outbox_push_fail)
  const s = new config.Settings(store.settings ?? {})
  s.concurrency = settings.concurrency
  s.chunk_mib = settings.chunk_mib
  s.auto_backup = settings.auto_backup
  s.size_padding = settings.size_padding
  s.cover_cache_mb = settings.cover_cache_mb
  await saveSettings(s)
}

// ---- 备份 ----
async function onBackup() {
  await backupNow() // 成功文案由 notify 事件出 toast
}

// ---- 孤儿清理:预览 → 确认 ----
const gc = ref<main.GCReport | null>(null)

async function onPreview() {
  gc.value = await previewGC()
}

async function onRunGC() {
  if (!gc.value) return
  const n = gc.value.TrashOrDeleted?.length ?? 0
  if (n === 0) return
  if (
    !(await askConfirm({
      title: '孤儿清理',
      message: `确认删除远端 ${n} 个已标记的 blob?此操作不可撤销。`,
      danger: true,
      confirmText: '删除',
    }))
  )
    return
  if (await runGC()) gc.value = null
}

// ---- 改口令 ----
const passOld = ref('')
const passNew = ref('')
const passNew2 = ref('')
const passMsg = ref('')

async function onChangePass() {
  if (passNew.value.length < 4) {
    passMsg.value = '新口令至少 4 个字符'
    return
  }
  if (passNew.value !== passNew2.value) {
    passMsg.value = '两次输入的新口令不一致'
    return
  }
  passMsg.value = ''
  if (await changePassphrase(passOld.value, passNew.value)) {
    passOld.value = passNew.value = passNew2.value = ''
    passMsg.value = ''
  }
}
</script>

<template>
  <div class="settings">
    <section>
      <h2>WebDAV 网盘</h2>
      <p class="dim">
        一盘一库:每个档案是独立的库(各自的文件列表与远端备份),共用同一把加密口令——切盘不重新解锁。
      </p>
      <div class="drives">
        <div v-for="d in store.drives" :key="d.ID" class="drive" :class="{ active: d.Active }">
          <div class="d-head">
            <span class="d-name">
              {{ d.Name }}
              <em v-if="d.Active" class="cur">当前</em>
            </span>
            <span class="d-actions">
              <button v-if="!d.Active" @click="onActivateDrive(d)">切换</button>
              <button @click="onEditDrive(d)">编辑</button>
              <button v-if="!d.Active" class="danger" @click="onDeleteDrive(d)">删除</button>
            </span>
          </div>
          <div class="dim">
            {{ d.URL }} · {{ d.Username }} · 根 {{ d.RootPath }}<template v-if="d.RememberPassword"> · 已记密码</template>
          </div>
        </div>
        <div v-if="store.drives.length === 0" class="dim">还没有网盘档案,添加一个开始使用。</div>
      </div>
      <div class="row">
        <button @click="onAddDrive">添加网盘</button>
      </div>

      <!-- 增/改表单:测试连接不落盘,保存走 SaveDrive -->
      <div v-if="showForm" class="drive-form">
        <label>名称<input v-model="form.name" type="text" placeholder="留空 = 用 URL 域名" /></label>
        <label>地址<input v-model="form.url" type="text" placeholder="https://dav.example.com/dav" /></label>
        <label>用户名<input v-model="form.username" type="text" /></label>
        <label>密码<input v-model="form.password" type="password" placeholder="不勾选记住则不落盘" /></label>
        <label>远端根目录<input v-model="form.rootPath" type="text" placeholder="/kist" /></label>
        <label class="check">
          <input v-model="form.rememberPassword" type="checkbox" />
          记住密码(明文保存于本机 config.json,共用设备请勿勾选)
        </label>
        <p v-if="formMsg" class="msg" :class="formOk ? 'ok' : 'err'">{{ formMsg }}</p>
        <div class="row">
          <button @click="onTestDrive">测试连接</button>
          <button class="primary" @click="onSaveDrive">保存档案</button>
          <button @click="showForm = false">取消</button>
        </div>
      </div>
    </section>

    <section>
      <h2>传输与备份</h2>
      <label>并发上传/下载数(1–4)
        <select v-model.number="settings.concurrency">
          <option :value="1">1</option>
          <option :value="2">2(默认)</option>
          <option :value="3">3</option>
          <option :value="4">4</option>
        </select>
      </label>
      <label>加密块大小(MiB)
        <select v-model.number="settings.chunk_mib">
          <option :value="4">4(默认)</option>
          <option :value="8">8</option>
          <option :value="16">16</option>
        </select>
      </label>
      <label class="check">
        <input v-model="settings.auto_backup" type="checkbox" />
        自动备份索引(索引变更后静默 30 秒自动上传;退出前若有未备份变更也会补一次)
      </label>
      <label class="check">
        <input :checked="settings.size_padding === 'on'" type="checkbox" @change="settings.size_padding = ($event.target as HTMLInputElement).checked ? 'on' : 'off'" />
        大小混淆(加密时补零到档位,网盘侧看不出真实大小;开销 ≤10%)
      </label>
      <label>封面缓存预算(TODO-10:封面出库后本地磁盘缓存,超出按最旧淘汰)
        <select v-model.number="settings.cover_cache_mb">
          <option :value="64">64 MB</option>
          <option :value="128">128 MB</option>
          <option :value="256">256 MB</option>
          <option :value="512">512 MB(默认)</option>
          <option :value="1024">1 GB</option>
          <option :value="2048">2 GB</option>
          <option :value="4096">4 GB</option>
        </select>
      </label>
      <div class="row">
        <button @click="onSaveSettings">保存偏好</button>
      </div>
      <div class="divider" />
      <div class="row">
        <button class="primary" @click="onBackup">立即备份索引</button>
        <span v-if="store.lastBackup" class="dim">
          revision {{ store.lastBackup.Revision }} · {{ fullTime(Math.floor(new Date(store.lastBackup.At).getTime() / 1000)) }}
        </span>
      </div>
    </section>

    <section>
      <h2>孤儿清理</h2>
      <p class="dim">
        删除已标记的远端 blob(删除文件后的空间回收);「远端有、索引无」的孤儿只报告不删。
      </p>
      <div class="row">
        <button @click="onPreview">扫描</button>
        <button v-if="gc" class="danger" :disabled="!(gc.TrashOrDeleted?.length)" @click="onRunGC">
          删除 {{ gc.TrashOrDeleted?.length ?? 0 }} 个
        </button>
      </div>
      <div v-if="gc" class="gc-report">
        <p>待删 trash blob:{{ gc.TrashOrDeleted?.length ?? 0 }} 个</p>
        <p>孤儿(只报告):{{ gc.Orphans?.length ?? 0 }} 个</p>
        <p>封面孤儿(covers/,只报告):{{ gc.CoverOrphans?.length ?? 0 }} 个</p>
        <pre v-if="gc.Orphans?.length || gc.CoverOrphans?.length">{{ [...(gc.Orphans ?? []), ...(gc.CoverOrphans ?? [])].slice(0, 20).join('\n') }}{{ (gc.Orphans.length ?? 0) + (gc.CoverOrphans.length ?? 0) > 20 ? `\n… 共 ${(gc.Orphans.length ?? 0) + (gc.CoverOrphans.length ?? 0)} 个` : '' }}</pre>
      </div>
    </section>

    <section>
      <h2>改口令</h2>
      <p class="dim">只重写密钥文件,已上传文件不受影响,无需重加密。</p>
      <label>当前口令<input v-model="passOld" type="password" /></label>
      <label>新口令<input v-model="passNew" type="password" /></label>
      <label>再输入一次<input v-model="passNew2" type="password" /></label>
      <p v-if="passMsg" class="msg err">{{ passMsg }}</p>
      <div class="row">
        <button class="primary" :disabled="!passOld || !passNew" @click="onChangePass">更改口令</button>
      </div>
    </section>

    <section class="danger-zone">
      <h2>会话</h2>
      <div class="row">
        <button class="danger" @click="lock">锁定(擦除内存中的密钥)</button>
      </div>
    </section>
  </div>
</template>

<style scoped>
.settings {
  height: 100%;
  overflow-y: auto;
  padding: 14px;
  display: flex;
  flex-direction: column;
  gap: 14px;
}

section {
  background: var(--panel);
  border: 1px solid var(--line);
  border-radius: 12px;
  padding: 16px;
  display: flex;
  flex-direction: column;
  gap: 10px;
}

h2 {
  font-size: 15px;
}

label {
  display: flex;
  flex-direction: column;
  gap: 4px;
  font-size: 13px;
  color: var(--dim);
  max-width: 420px;
}

label.check {
  flex-direction: row;
  align-items: flex-start;
  gap: 8px;
}

label.check input {
  width: auto;
  margin-top: 3px;
}

select {
  max-width: 200px;
}

.row {
  display: flex;
  gap: 10px;
  align-items: center;
  flex-wrap: wrap;
}

.msg {
  font-size: 13px;
  word-break: break-all;
}

/* 网盘档案列表 */
.drives {
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.drive {
  border: 1px solid var(--line);
  border-radius: var(--radius);
  padding: 10px 12px;
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.drive.active {
  border-color: var(--accent);
  background: rgba(79, 140, 255, 0.06);
}

.d-head {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 8px;
}

.d-name {
  font-size: 14px;
}

.cur {
  font-style: normal;
  color: var(--accent);
  font-size: 12px;
  border: 1px solid var(--accent);
  border-radius: 4px;
  padding: 0 6px;
  margin-left: 6px;
}

.d-actions {
  display: flex;
  gap: 6px;
}

.drive-form {
  border-top: 1px solid var(--line);
  padding-top: 10px;
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.msg.ok {
  color: var(--ok);
}

.msg.err {
  color: var(--err);
}

.dim {
  color: var(--dim);
  font-size: 12px;
}

.divider {
  border-top: 1px solid var(--line);
  margin: 4px 0;
}

.gc-report {
  font-size: 13px;
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.gc-report pre {
  background: var(--bg);
  border: 1px solid var(--line);
  border-radius: var(--radius);
  padding: 8px;
  font-size: 11px;
  overflow: auto;
  max-height: 140px;
  white-space: pre-wrap;
  word-break: break-all;
}
</style>
