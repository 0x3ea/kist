// store.ts — 全局状态与 wailsjs 封装:页面不直接碰 wailsjs(约定见 phase-7)。
// 形态:reactive 单例 + 导出的动作函数;事件订阅在 init() 一次性挂上。
// Go 侧 null 已归一为空数组,这里仍做一层 ?? 兜底(防御反序列化差异)。

import { reactive } from 'vue'
import * as API from '../wailsjs/go/main/App'
import { EventsOn } from '../wailsjs/runtime/runtime'
import { backup, config, index, main, transfer } from '../wailsjs/go/models'
import { parseErr } from './errors'
import { humanSize, shortTime } from './format'

export type Page = 'files' | 'transfers' | 'settings'

export interface Toast {
  id: number
  level: 'info' | 'error'
  text: string
}

let toastSeq = 0
let searchTimer: ReturnType<typeof setTimeout> | null = null

export const store = reactive({
  /** 后端 app:state 快照(startup 事件早于订阅会丢,init 主动拉) */
  state: { Configured: false, Unlocked: false, FileCount: 0, HasLocalKeyfile: false },
  page: 'files' as Page,

  // Files 页
  folder: {
    id: 1,
    crumbs: [] as index.Crumb[],
    entries: [] as index.Entry[],
    summaries: {} as Record<string, index.FolderSummary>, // map[int64] 经 JSON 变字符串键
  },
  view: 'grid' as 'grid' | 'list',
  selection: new Set<number>(), // 选中的文件 id
  folderSelection: new Set<number>(), // 选中的目录 id
  detail: null as main.FileDetail | null,
  search: { active: false, query: '', folders: [] as index.FolderHit[], files: [] as index.FileHit[] },

  // Transfers 页
  transfers: [] as transfer.Transfer[],

  // 缩略图缓存:fileID → dataURL(封面宫格/网格/详情共用)
  thumbs: new Map<number, string>(),

  // Settings 页
  settings: null as config.Settings | null,
  webdav: null as main.WebDAVConfig | null,
  lastBackup: null as backup.BackupInfo | null,
  gcReport: null as main.GCReport | null,

  toasts: [] as Toast[],
  /** 长操作忙碌文案(解锁中/恢复中/测试中…),空串 = 空闲 */
  busy: '',
})

// ---- 基础 ----

export function toast(level: 'info' | 'error', text: string) {
  const t = { id: ++toastSeq, level, text }
  store.toasts.push(t)
  setTimeout(() => {
    const i = store.toasts.findIndex((x) => x.id === t.id)
    if (i >= 0) store.toasts.splice(i, 1)
  }, level === 'error' ? 6000 : 3500)
}

/** 统一的 promise 错误出 toast;返回解析结果供调用方分支 */
export function fail(e: unknown): ReturnType<typeof parseErr> {
  const p = parseErr(String(e))
  toast('error', p.text)
  return p
}

function setBusy(text: string): () => void {
  store.busy = text
  return () => (store.busy = '')
}

export async function init() {
  await refreshState()
  store.transfers = (await API.Transfers()) ?? []
  EventsOn('app:state', (s) => Object.assign(store.state, s ?? {}))
  EventsOn('transfers:changed', (ts) => (store.transfers = ts ?? []))
  EventsOn('transfer:update', (t) => upsertTransfer(t))
  EventsOn('transfer:done', (t) => {
    upsertTransfer(t)
    toast('info', `完成:${t?.name ?? ''}`)
  })
  EventsOn('transfer:error', (t) => {
    upsertTransfer(t)
    toast('error', `${t?.name ?? '传输'}失败:${t?.err ?? '未知错误'}`)
  })
  EventsOn('index:changed', () => {
    // 上传/删除/移动/恢复后刷新;搜索态重跑搜索
    if (store.search.active) runSearch(store.search.query)
    else loadFolder(store.folder.id)
    refreshState()
  })
  EventsOn('notify', (n) => toast(n?.level === 'error' ? 'error' : 'info', n?.text ?? ''))
}

export async function refreshState() {
  try {
    Object.assign(store.state, await API.GetAppState())
  } catch {
    /* GetAppState 不返回错误;兜底静默 */
  }
}

function upsertTransfer(t: transfer.Transfer) {
  if (!t?.id) return
  const i = store.transfers.findIndex((x) => x.id === t.id)
  if (i >= 0) store.transfers[i] = t
  else store.transfers.push(t)
}

// ---- Lock 页:解锁 / 建账户 / 恢复 ----

export async function unlock(pass: string): Promise<main.UnlockResult | null> {
  const done = setBusy('解锁中…')
  try {
    const r = await API.Unlock(pass)
    toast('info', `已解锁(${r.FileCount} 个文件)`)
    return r
  } catch (e) {
    fail(e)
    return null
  } finally {
    done()
  }
}

export async function createAccount(pass: string): Promise<boolean> {
  const done = setBusy('建立账户中…')
  try {
    await API.CreateAccount(pass)
    toast('info', '账户已建立,可以开始上传了')
    return true
  } catch (e) {
    fail(e)
    return false
  } finally {
    done()
  }
}

export async function importFromRemote(pass: string): Promise<boolean> {
  const done = setBusy('从远端恢复中…')
  try {
    const r = await API.ImportFromRemote(pass)
    // LWW 三分支提示(replaced/noop/local-newer)
    if (r.Action === 'replaced') {
      toast('info', `已恢复到 revision ${r.RemoteRev}(旧库已归档 backups/)`)
    } else if (r.Action === 'local-newer') {
      toast('info', '本地索引更新,未做替换(建议先备份)')
    } else {
      toast('info', '远端与本地版本一致,无需恢复')
    }
    return true
  } catch (e) {
    fail(e)
    return false
  } finally {
    done()
  }
}

export async function lock() {
  try {
    await API.Lock()
    store.detail = null
    store.selection.clear()
    store.folderSelection.clear()
  } catch (e) {
    fail(e)
  }
}

export async function testConnection(c: main.WebDAVConfig): Promise<main.TestResult> {
  return API.TestConnection(c)
}

export async function saveWebDAVConfig(c: main.WebDAVConfig): Promise<boolean> {
  const done = setBusy('保存配置…')
  try {
    await API.SaveWebDAVConfig(c)
    await refreshState()
    return true
  } catch (e) {
    fail(e)
    return false
  } finally {
    done()
  }
}

// ---- Files 页 ----

export async function loadFolder(id: number) {
  try {
    const v = await API.ListFolder(id)
    store.folder.id = id
    store.folder.crumbs = v.Crumbs ?? []
    store.folder.entries = v.Entries ?? []
    store.folder.summaries = v.Summaries ?? {}
    store.selection.clear()
    store.folderSelection.clear()
  } catch (e) {
    fail(e)
  }
}

export function summaryOf(folderID: number): index.FolderSummary | undefined {
  return store.folder.summaries[String(folderID)]
}

/** 目录行摘要:CLI summaryLine 同款措辞(N 话 · 大小 · ← 时间 · 待传) */
export function summaryText(folderID: number): string {
  const s = summaryOf(folderID)
  if (!s) return ''
  const parts: string[] = []
  if (s.PackCount > 0) parts.push(`${s.PackCount} 话`)
  else if (s.FileCount > 0) parts.push(`${s.FileCount} 文件`)
  if (s.TotalSize > 0) parts.push(humanSize(s.TotalSize))
  if (s.LatestAt > 0) parts.push(`← ${shortTime(s.LatestAt)}`)
  if (s.PendingCount > 0) parts.push(`待传 ${s.PendingCount}`)
  return parts.join(' · ')
}

/** 缩略图 dataURL(带缓存);取不到返回空串(调用方显示占位) */
export async function ensureThumb(fileID: number): Promise<string> {
  const hit = store.thumbs.get(fileID)
  if (hit !== undefined) return hit
  try {
    const t = await API.GetThumbnail(fileID)
    const url = thumbToDataUrl(t)
    store.thumbs.set(fileID, url)
    return url
  } catch {
    store.thumbs.set(fileID, '') // 负缓存:没有缩略图,不反复请求
    return ''
  }
}

// ThumbData.Data 在 Go 是 []byte:JSON 传输为 base64 字符串,但 wails 生成的
// 类型标注是 number[](生成器缺陷)——双态兼容,失败返回空串走占位。
function thumbToDataUrl(t: main.ThumbData): string {
  const d = t?.Data as unknown
  try {
    if (typeof d === 'string') return `data:${t.Mime};base64,${d}`
    if (Array.isArray(d) && d.length > 0) {
      let bin = ''
      for (let i = 0; i < d.length; i++) bin += String.fromCharCode(d[i])
      return `data:${t.Mime};base64,${btoa(bin)}`
    }
  } catch {
    /* 落空走占位 */
  }
  return ''
}

/** 搜索(300ms 防抖,由输入框直接调) */
export function searchDebounced(q: string) {
  store.search.query = q
  if (searchTimer) clearTimeout(searchTimer)
  if (!q.trim()) {
    store.search.active = false
    store.search.folders = []
    store.search.files = []
    return
  }
  searchTimer = setTimeout(() => runSearch(q), 300)
}

export async function runSearch(q: string) {
  if (!q.trim()) return
  try {
    const v = await API.SearchAll(q, 100)
    store.search.active = true
    store.search.query = q
    store.search.folders = v.Folders ?? []
    store.search.files = v.Files ?? []
  } catch (e) {
    fail(e)
  }
}

export function exitSearch() {
  store.search.active = false
  store.search.query = ''
  store.search.folders = []
  store.search.files = []
}

export async function openDetail(fileID: number) {
  try {
    store.detail = await API.FileInfo(fileID)
    if (store.detail?.HasThumb) ensureThumb(fileID)
  } catch (e) {
    fail(e)
  }
}

export async function saveNote(fileID: number, note: string): Promise<boolean> {
  try {
    await API.SetNote(fileID, note)
    if (store.detail?.ID === fileID) store.detail.Note = note
    return true
  } catch (e) {
    fail(e)
    return false
  }
}

export async function deleteEntries(): Promise<boolean> {
  const files = [...store.selection]
  const folders = [...store.folderSelection]
  if (files.length === 0 && folders.length === 0) return false
  try {
    await API.DeleteEntries(files, folders)
    store.detail = null
    toast('info', `已删除 ${files.length} 个文件、${folders.length} 个目录(远端占位由孤儿清理回收)`)
    return true
  } catch (e) {
    fail(e)
    return false
  }
}

export async function moveSelected(destPath: string): Promise<boolean> {
  const files = [...store.selection]
  if (files.length === 0) return false
  try {
    const dest = await API.EnsureFolder(destPath)
    await API.MoveFiles(files, dest)
    toast('info', `已移动 ${files.length} 个文件`)
    return true
  } catch (e) {
    fail(e)
    return false
  }
}

export async function getFolderMeta(folderID: number): Promise<index.FolderMeta | null> {
  try {
    return await API.GetFolderMeta(folderID)
  } catch (e) {
    fail(e)
    return null
  }
}

export async function saveFolderMeta(folderID: number, u: index.FolderMetaUpdate): Promise<boolean> {
  try {
    await API.UpdateFolderMeta(folderID, u)
    toast('info', '目录元数据已保存')
    return true
  } catch (e) {
    fail(e)
    return false
  }
}

// ---- Transfers 页 ----

export function goTransfers() {
  store.page = 'transfers'
}

export async function upload(kind: 'files' | 'folder') {
  try {
    const paths = kind === 'files' ? await API.PickFiles() : [await API.PickDir()]
    const picked = paths.filter((p) => p)
    if (picked.length === 0) return
    const n = await API.UploadPaths(picked, store.folder.id)
    toast('info', `已入队 ${n} 个任务`)
    goTransfers()
  } catch (e) {
    fail(e)
  }
}

export async function downloadSelected() {
  const files = [...store.selection]
  if (files.length === 0) return
  try {
    const dir = await API.PickDir()
    if (!dir) return
    const n = await API.DownloadTo(files, dir)
    toast('info', `已入队 ${n} 个下载`)
    goTransfers()
  } catch (e) {
    fail(e)
  }
}

export async function cancelTransfer(id: string) {
  try {
    await API.CancelTransfer(id)
  } catch (e) {
    fail(e)
  }
}

// ---- Settings 页 ----

export async function loadSettings() {
  try {
    store.settings = await API.GetSettings()
    store.webdav = await API.GetWebDAVConfig()
  } catch (e) {
    fail(e)
  }
}

export async function saveSettings(s: config.Settings): Promise<boolean> {
  try {
    await API.SaveSettings(s)
    store.settings = { ...s }
    return true
  } catch (e) {
    fail(e)
    return false
  }
}

export async function backupNow(): Promise<boolean> {
  const done = setBusy('备份索引…')
  try {
    const info = await API.BackupIndexNow()
    store.lastBackup = info
    return true
  } catch (e) {
    fail(e)
    return false
  } finally {
    done()
  }
}

export async function previewGC(): Promise<main.GCReport | null> {
  const done = setBusy('扫描远端…')
  try {
    store.gcReport = await API.PreviewGC()
    return store.gcReport
  } catch (e) {
    fail(e)
    return null
  } finally {
    done()
  }
}

export async function runGC(): Promise<boolean> {
  const done = setBusy('清理中…')
  try {
    store.gcReport = await API.RunGC()
    return true
  } catch (e) {
    fail(e)
    return false
  } finally {
    done()
  }
}

export async function changePassphrase(oldPass: string, newPass: string): Promise<boolean> {
  const done = setBusy('改口令…')
  try {
    await API.ChangePassphrase(oldPass, newPass)
    return true
  } catch (e) {
    fail(e)
    return false
  } finally {
    done()
  }
}
