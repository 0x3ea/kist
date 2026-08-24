<script setup lang="ts">
// Settings.vue — 设置页:WebDAV 配置、行为偏好(并发/块大小/自动备份/大小混淆)、
// 立即备份、孤儿清理两步(预览→确认)、改口令、锁定。改并发/块大小对进行中
// 传输的下一任务生效(管线闭包每轮重读)。
import { onMounted, reactive, ref } from 'vue'
import { config, main } from '../../wailsjs/go/models'
import {
  store,
  backupNow,
  changePassphrase,
  loadSettings,
  lock,
  previewGC,
  runGC,
  saveSettings,
  saveWebDAVConfig,
  testConnection,
} from '../store'
import { fullTime } from '../format'

const davMsg = ref('')
const davOk = ref(false)

// WebDAV 表单:进入页面时拉当前值;密码仅在勾了"记住密码"时有回显
const dav = reactive(new main.WebDAVConfig({ RootPath: '/kist' }))
const settings = reactive({
  concurrency: 2,
  chunk_mib: 4,
  auto_backup: true,
  size_padding: 'on',
})

onMounted(async () => {
  await loadSettings()
  if (store.webdav) Object.assign(dav, store.webdav)
  if (store.settings) {
    settings.concurrency = store.settings.concurrency
    settings.chunk_mib = store.settings.chunk_mib
    settings.auto_backup = store.settings.auto_backup
    settings.size_padding = store.settings.size_padding
  }
})

async function onTest() {
  davMsg.value = ''
  const r = await testConnection(dav)
  davOk.value = r.Ok
  davMsg.value = r.Detail
}

async function onSaveDav() {
  if (await saveWebDAVConfig(dav)) davMsg.value = ''
}

async function onSaveSettings() {
  // 展开 store.settings 保留表单未覆盖的字段(remember_password/outbox_push_fail)
  const s = new config.Settings(store.settings ?? {})
  s.concurrency = settings.concurrency
  s.chunk_mib = settings.chunk_mib
  s.auto_backup = settings.auto_backup
  s.size_padding = settings.size_padding
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
  if (!confirm(`确认删除远端 ${n} 个已标记的 blob?此操作不可撤销。`)) return
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
      <label>地址<input v-model="dav.URL" type="text" placeholder="https://dav.example.com/dav" /></label>
      <label>用户名<input v-model="dav.Username" type="text" /></label>
      <label>密码<input v-model="dav.Password" type="password" placeholder="留空 = 不修改已存密码" /></label>
      <label>远端根目录<input v-model="dav.RootPath" type="text" /></label>
      <label class="check">
        <input v-model="dav.RememberPassword" type="checkbox" />
        记住密码(明文保存于本机 config.json,共用设备请勿勾选)
      </label>
      <p v-if="davMsg" class="msg" :class="davOk ? 'ok' : 'err'">{{ davMsg }}</p>
      <div class="row">
        <button @click="onTest">测试连接</button>
        <button class="primary" @click="onSaveDav">保存配置</button>
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
        <pre v-if="gc.Orphans?.length">{{ gc.Orphans.slice(0, 20).join('\n') }}{{ gc.Orphans.length > 20 ? `\n… 共 ${gc.Orphans.length} 个` : '' }}</pre>
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
