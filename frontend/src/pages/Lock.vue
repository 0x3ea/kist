<script setup lang="ts">
// Lock.vue — 解锁页三分支(phase-7 约定):
//   1) 未配置 → 首次向导:WebDAV 表单 + 测试连接 → 设口令 → 建账户
//   2) 已配置且有本地 keyfile → 输口令解锁;若解锁后发现本地空库且远端有
//      备份(UnlockResult.SuggestPullIndex),提示一键"从远端恢复"
//   3) 已配置且无本地 keyfile → 新设备,直接"从远端恢复"(ImportFromRemote)
import { computed, reactive, ref } from 'vue'
import { main } from '../../wailsjs/go/models'
import { store, createAccount, importFromRemote, saveWebDAVConfig, testConnection, unlock } from '../store'

const state = store.state

// ---- 分支判定 ----
const mode = computed<'wizard' | 'unlock' | 'recover'>(() => {
  if (!state.Configured) return 'wizard'
  return state.HasLocalKeyfile ? 'unlock' : 'recover'
})

// ---- 向导:第一步 WebDAV,第二步口令 ----
const step = ref<'webdav' | 'passphrase'>('webdav')
const dav = reactive(new main.WebDAVConfig({ RootPath: '/kist' }))
const testMsg = ref('')
const testOk = ref(false)
const pass1 = ref('')
const pass2 = ref('')

async function onSaveDav() {
  testMsg.value = ''
  testOk.value = false
  const r = await testConnection(dav)
  testMsg.value = r.Detail
  testOk.value = r.Ok
  if (!r.Ok) return
  if (await saveWebDAVConfig(dav)) {
    // 本地库非空(换网盘目录复用旧数据)会让新账户混账——确认后再继续
    if (state.FileCount > 0 && !confirm(`本地索引已有 ${state.FileCount} 个文件。\n在此网盘上新建账户不会清除它们,但列表会混合两个来源的数据。继续吗?`)) {
      return
    }
    step.value = 'passphrase'
  }
}

async function onCreate() {
  if (pass1.value.length < 4) {
    testMsg.value = '口令至少 4 个字符'
    return
  }
  if (pass1.value !== pass2.value) {
    testMsg.value = '两次输入的口令不一致'
    return
  }
  testMsg.value = ''
  await createAccount(pass1.value)
}

// ---- 解锁 ----
const pass = ref('')
const unlockHint = ref<main.UnlockResult | null>(null)

async function onUnlock() {
  const r = await unlock(pass.value)
  pass.value = ''
  if (r) unlockHint.value = r
}

async function onPullIndex() {
  // 解锁后补拉索引(口令已验证过,但 ImportFromRemote 需要再次传入)
  const p = prompt('请再输入一次口令以拉取远端索引') ?? ''
  if (!p) return
  await importFromRemote(p)
  unlockHint.value = null
}

// ---- 新设备恢复 ----
const recoverPass = ref('')
const recoverNote = ref('')

async function onRecover() {
  const ok = await importFromRemote(recoverPass.value)
  if (ok) {
    recoverPass.value = ''
    recoverNote.value = ''
  } else {
    recoverNote.value = '恢复失败:请确认口令与网盘连通性后重试'
  }
}
</script>

<template>
  <div class="lock">
    <div class="card">
      <h1>kist</h1>
      <p class="sub">WebDAV 加密网盘管理器</p>

      <!-- ① 首次向导 -->
      <template v-if="mode === 'wizard'">
        <template v-if="step === 'webdav'">
          <h2>配置网盘</h2>
          <label>WebDAV 地址<input v-model="dav.URL" type="text" placeholder="https://dav.example.com/dav" /></label>
          <label>用户名<input v-model="dav.Username" type="text" /></label>
          <label>密码<input v-model="dav.Password" type="password" placeholder="不勾选记住则不落盘" /></label>
          <label>远端根目录<input v-model="dav.RootPath" type="text" placeholder="/kist" /></label>
          <label class="check">
            <input v-model="dav.RememberPassword" type="checkbox" />
            记住密码(明文保存在本机 config.json,共用设备请勿勾选)
          </label>
          <p v-if="testMsg" class="msg" :class="testOk ? 'ok' : 'err'">{{ testMsg }}</p>
          <button class="primary" @click="onSaveDav">测试连接并保存</button>
        </template>
        <template v-else>
          <h2>设置加密口令</h2>
          <p class="hint">口令用于加密所有文件;忘记口令 = 数据无法恢复,请牢记。</p>
          <label>口令<input v-model="pass1" type="password" @keyup.enter="onCreate" /></label>
          <label>再输入一次<input v-model="pass2" type="password" @keyup.enter="onCreate" /></label>
          <p v-if="testMsg" class="msg err">{{ testMsg }}</p>
          <button class="primary" @click="onCreate">建立账户</button>
        </template>
      </template>

      <!-- ② 解锁 -->
      <template v-else-if="mode === 'unlock'">
        <h2>解锁</h2>
        <p class="hint">{{ state.FileCount }} 个文件在索引中</p>
        <label>口令<input v-model="pass" type="password" @keyup.enter="onUnlock" /></label>
        <button class="primary" :disabled="!pass" @click="onUnlock">解锁</button>

        <!-- 本地空库且远端有备份:引导恢复(缩略图/备注随索引回来) -->
        <div v-if="unlockHint?.SuggestPullIndex" class="pull-hint">
          本地索引是空的,但远端检测到备份。
          <button @click="onPullIndex">从远端恢复索引</button>
        </div>
      </template>

      <!-- ③ 新设备恢复 -->
      <template v-else>
        <h2>从远端恢复</h2>
        <p class="hint">
          本机没有密钥文件——像是新设备。输入原有口令,将从网盘拉回密钥与索引
          (文件列表、备注、缩略图);落后于本地的改动会归档而非合并。
        </p>
        <label>口令<input v-model="recoverPass" type="password" @keyup.enter="onRecover" /></label>
        <p v-if="recoverNote" class="msg err">{{ recoverNote }}</p>
        <button class="primary" :disabled="!recoverPass" @click="onRecover">恢复</button>
      </template>
    </div>
  </div>
</template>

<style scoped>
.lock {
  height: 100%;
  display: flex;
  align-items: center;
  justify-content: center;
  overflow: auto;
  padding: 24px;
}

.card {
  width: 380px;
  background: var(--panel);
  border: 1px solid var(--line);
  border-radius: 12px;
  padding: 28px;
  display: flex;
  flex-direction: column;
  gap: 12px;
}

h1 {
  font-size: 26px;
  letter-spacing: 2px;
}

h2 {
  font-size: 16px;
  margin-top: 4px;
}

.sub {
  color: var(--dim);
  margin-top: -8px;
}

.hint {
  color: var(--dim);
  font-size: 13px;
}

label {
  display: flex;
  flex-direction: column;
  gap: 4px;
  font-size: 13px;
  color: var(--dim);
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

.pull-hint {
  background: var(--panel-2);
  border: 1px solid var(--line);
  border-radius: var(--radius);
  padding: 10px;
  font-size: 13px;
  display: flex;
  flex-direction: column;
  gap: 8px;
  align-items: flex-start;
}
</style>
