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

/** 右栏目录详情(单击目录行展示):name/path 是点击时刻从列表条目与面包屑
 * 合成的快照;summary 复用列表已聚合的 FolderSummary(不重复建树);
 * meta 来自 GetFolderMeta——取到后才一次性挂上面板,无待补齐中间态。 */
export interface FolderDetail {
  id: number
  name: string
  path: string
  meta: index.FolderMeta
  summary: index.FolderSummary | null
}

let toastSeq = 0
let searchTimer: ReturnType<typeof setTimeout> | null = null

// ---- 自绘确认框(替代原生 confirm:样式割裂、标题写死、按钮文案不可控)----
// App.vue 挂载一次 ConfirmDialog;askConfirm 返回 Promise,调用方 await 后
// 按返回值分支,与原 confirm() 同形。

export const confirmState = reactive<{
  open: boolean
  title: string
  message: string
  danger: boolean // 危险动作:确认键红色强调
  confirmText: string
  resolve: ((ok: boolean) => void) | null
}>({ open: false, title: '', message: '', danger: false, confirmText: '确定', resolve: null })

export function askConfirm(opts: {
  title: string
  message: string
  danger?: boolean
  confirmText?: string
}): Promise<boolean> {
  return new Promise((resolve) => {
    confirmState.title = opts.title
    confirmState.message = opts.message
    confirmState.danger = opts.danger ?? false
    confirmState.confirmText = opts.confirmText ?? '确定'
    confirmState.resolve = resolve
    confirmState.open = true
  })
}

/** ConfirmDialog 的两个出口(按钮/Esc/Enter)都经这里结算,防重复 resolve */
export function settleConfirm(ok: boolean) {
  if (!confirmState.open) return
  confirmState.open = false
  confirmState.resolve?.(ok)
  confirmState.resolve = null
}

/** 同步分叉待裁决(TODO-09):Go 侧 sync:conflict 事件驱动。
 * pending = 冲突存在未裁决(Settings 横幅据此显示);open = 对话框可见。
 * detail = 冲突详情(TODO-22):对话框打开时按需拉取,含文件级 diff 三栏;
 * stale 刷新复用同一字段。 */
export const syncConflict = reactive<{
  pending: boolean
  open: boolean
  kind: string // diverged | remote-ahead
  localRev: number
  remoteRev: number
  baselineRev: number
  remoteDevice: string
  detail: backup.ConflictDetail | null
  detailLoading: boolean
}>({
  pending: false,
  open: false,
  kind: '',
  localRev: 0,
  remoteRev: 0,
  baselineRev: 0,
  remoteDevice: '',
  detail: null,
  detailLoading: false,
})

/** 拉取冲突详情(对话框打开时调用;force=true 用于 stale 后强制刷新) */
export async function loadConflictDetail(force = false) {
  if (syncConflict.detailLoading || (!force && syncConflict.detail)) return
  syncConflict.detailLoading = true
  try {
    const d = await API.SyncConflictDetail()
    syncConflict.detail = d
    // 详情是重新探测的结果,局面可能已与事件时刻不同:一并刷新头部数字
    syncConflict.kind = d.Kind
    syncConflict.localRev = d.LocalRev
    syncConflict.remoteRev = d.RemoteRev
    syncConflict.baselineRev = d.BaselineRev
    syncConflict.remoteDevice = d.RemoteDevice
  } catch (e) {
    fail(e)
  } finally {
    syncConflict.detailLoading = false
  }
}

/** 关掉对话框但保留冲突横幅入口("稍后处理") */
export function dismissConflict() {
  syncConflict.open = false
}

export const store = reactive({
  /** 后端 app:state 快照(startup 事件早于订阅会丢,init 主动拉) */
  state: { Configured: false, Unlocked: false, FileCount: 0, HasLocalKeyfile: false, DriveName: '', DriveCount: 0 },
  /** init() 拉回真实 state 后置真——此前 state 是初值,分支判定不可依赖 */
  ready: false,
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
  folderDetail: null as FolderDetail | null, // 右栏目录详情(单击目录行)
  search: { active: false, query: '', folders: [] as index.FolderHit[], files: [] as index.FileHit[] },

  // Transfers 页
  transfers: [] as transfer.Transfer[],

  // 缩略图缓存:fileID → dataURL(封面宫格/网格/详情共用)
  thumbs: new Map<number, string>(),
  // 目录自有封面缓存:folderID → dataURL。与 thumbs 分开存——文件与目录
  // id 是两张表各自自增,数值会撞,混在一张 Map 里会互相顶掉(v6)
  folderThumbs: new Map<number, string>(),

  // Settings 页
  settings: null as config.Settings | null,
  drives: [] as main.DriveInfo[], // 多网盘档案(TODO-21)
  lastBackup: null as backup.BackupInfo | null,
  gcReport: null as main.GCReport | null,

  toasts: [] as Toast[],
  /** 长操作忙碌文案(解锁中/恢复中/测试中…),空串 = 空闲 */
  busy: '',
  /** 换库事件序号:Files 页 watch 它清搜索框等页面本地状态(TODO-21) */
  driveSwitchSeq: 0,
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
  store.ready = true
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
  EventsOn('drive:switched', async () => {
    // 换库(TODO-21):管线已随库重建,本地缓存全部作废——回根目录清状态。
    // driveSwitchSeq 通知 Files 页清搜索框(搜索词是页面本地状态)。
    store.detail = null
    store.folderDetail = null
    store.selection.clear()
    store.folderSelection.clear()
    store.thumbs.clear()
    store.folderThumbs.clear()
    exitSearch()
    store.driveSwitchSeq++
    await loadFolder(1)
    store.transfers = (await API.Transfers()) ?? []
    refreshDrives()
  })
  EventsOn('notify', (n) => toast(n?.level === 'error' ? 'error' : 'info', n?.text ?? ''))
  EventsOn('sync:conflict', (c) => {
    // 同步分叉待裁决(TODO-09):自动/手动备份被拦下或启动对账(TODO-22)检出时发出。
    // 详情(文件级 diff)清空待拉——冲突可能在弹窗打开前已过时,打开时重新探测。
    Object.assign(syncConflict, {
      pending: true,
      open: true,
      kind: c?.kind ?? 'diverged',
      localRev: c?.localRev ?? 0,
      remoteRev: c?.remoteRev ?? 0,
      baselineRev: c?.baselineRev ?? 0,
      remoteDevice: c?.remoteDevice ?? '',
      detail: null,
      detailLoading: false,
    })
    loadConflictDetail()
  })
}

/** 分叉裁决(TODO-09):keep-local = 覆盖远端;keep-remote = 采纳远端(本机归档)。
 * 裁决带 expectRemoteRev(TODO-22 硬要求):后端 force 前重检远端头部,
 * 已变化时返回 false——刷新详情,弹窗保持打开,让用户基于新差异重新裁决。 */
export async function resolveConflict(action: 'keep-local' | 'keep-remote'): Promise<boolean> {
  const done = setBusy('处理同步冲突…')
  try {
    // keep-remote 需复核口令(与 Lock 页"从远端恢复索引"同款 prompt)
    const pass = action === 'keep-remote' ? (prompt('请输入口令以采纳远端索引') ?? '') : ''
    if (action === 'keep-remote' && !pass) return false
    const expect = syncConflict.detail?.RemoteRev ?? syncConflict.remoteRev
    const resolved = await API.ResolveConflict(action, pass, expect)
    if (!resolved) {
      toast('info', '云端又有新变化,已刷新差异,请重新确认')
      await loadConflictDetail(true)
      return false
    }
    syncConflict.pending = false
    syncConflict.open = false
    return true
  } catch (e) {
    fail(e)
    return false
  } finally {
    done()
  }
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
    store.folderDetail = null
    store.selection.clear()
    store.folderSelection.clear()
  } catch (e) {
    fail(e)
  }
}

export async function testConnection(c: main.WebDAVConfig): Promise<main.TestResult> {
  return API.TestConnection(c)
}

/** 向导首配:写活动盘,无档案则建第一个盘(后端 SaveWebDAVConfig) */
export async function saveWebDAVConfig(c: main.WebDAVConfig): Promise<boolean> {
  const done = setBusy('保存配置…')
  try {
    await API.SaveWebDAVConfig(c)
    await Promise.all([refreshState(), refreshDrives()])
    return true
  } catch (e) {
    fail(e)
    return false
  } finally {
    done()
  }
}

// ---- 多网盘档案(TODO-21) ----

export async function refreshDrives() {
  try {
    store.drives = (await API.ListDrives()) ?? []
  } catch (e) {
    fail(e)
  }
}

export interface DriveForm {
  id: string
  name: string
  url: string
  username: string
  password: string
  rootPath: string
  rememberPassword: boolean
}

/** 新增(ID 空)或编辑网盘档案;编辑活动盘时后端热更新远端客户端 */
export async function saveDrive(f: DriveForm): Promise<boolean> {
  const done = setBusy('保存网盘档案…')
  try {
    await API.SaveDrive({
      ID: f.id,
      Name: f.name,
      URL: f.url,
      Username: f.username,
      Password: f.password,
      RootPath: f.rootPath,
      RememberPassword: f.rememberPassword,
    })
    await refreshDrives()
    return true
  } catch (e) {
    fail(e)
    return false
  } finally {
    done()
  }
}

export async function deleteDrive(id: string): Promise<boolean> {
  try {
    await API.DeleteDrive(id)
    await refreshDrives()
    return true
  } catch (e) {
    fail(e)
    return false
  }
}

/** 切换当前库:后端要求管线空闲;成功后 drive:switched 事件统一清状态 */
export async function activateDrive(id: string): Promise<boolean> {
  const done = setBusy('切换网盘…')
  try {
    await API.SetActiveDrive(id)
    await refreshDrives()
    return true
  } catch (e) {
    fail(e)
    return false
  } finally {
    done()
  }
}

// ---- Files 页 ----

/** 普通点击的选区语义:唯一选中(替换整个选区,文件/目录分侧归位)。
 * 详情面板的开合与双击判定由调用方编排——这里只管选区。 */
export function selectOnly(entry: { IsFolder: boolean; ID: number }): void {
  store.selection.clear()
  store.folderSelection.clear()
  ;(entry.IsFolder ? store.folderSelection : store.selection).add(entry.ID)
}

/** Ctrl/⌘+点击的多选语义:切换该条目选中态,不清其余、不动详情面板
 * (多选时面板跟 flash 没有意义);文件侧清到空时顺手收起文件详情。
 * 目录侧免 250ms 双击判定——多选态下按住 Ctrl 不会是想双击进入。 */
export function ctrlToggleSelect(entry: { IsFolder: boolean; ID: number }): void {
  const sel = entry.IsFolder ? store.folderSelection : store.selection
  if (sel.has(entry.ID)) {
    sel.delete(entry.ID)
    if (!entry.IsFolder && store.selection.size === 0) store.detail = null
  } else {
    sel.add(entry.ID)
  }
}

/** 判断点击是否处于多选态(Ctrl 按住;⌘ 是 mac 惯例,顺手兼容) */
export function isMultiSelectClick(ev: MouseEvent): boolean {
  return ev.ctrlKey || ev.metaKey
}

export async function loadFolder(id: number) {
  try {
    const v = await API.ListFolder(id)
    const entries = v.Entries ?? []
    const fileIDs = new Set(entries.filter((e) => !e.IsFolder).map((e) => e.ID))
    const folderIDs = new Set(entries.filter((e) => e.IsFolder).map((e) => e.ID))
    store.folder.id = id
    store.folder.crumbs = v.Crumbs ?? []
    store.folder.entries = entries
    store.folder.summaries = v.Summaries ?? {}
    // 选中与面板快照按"条目仍在当前列表"修剪,而非无条件清空:
    // 换目录时旧条目必不在新列表 → 等价清空(文件/目录 id 是两张表各自
    // 自增,数值可撞,必须按 IsFolder 分侧比对);同目录刷新(index:changed)
    // 则保留幸存者——否则右栏备注一保存就被刷新事件收起,选区也被抹掉。
    for (const fid of store.selection) if (!fileIDs.has(fid)) store.selection.delete(fid)
    for (const fid of store.folderSelection) if (!folderIDs.has(fid)) store.folderSelection.delete(fid)
    if (store.detail && !fileIDs.has(store.detail.ID)) store.detail = null
    if (store.folderDetail && !folderIDs.has(store.folderDetail.id)) store.folderDetail = null
    else if (store.folderDetail)
      // 面板里的目录还在:摘要换成本轮重算的(封面链/待传数等随索引变化)
      store.folderDetail.summary = store.folder.summaries[String(store.folderDetail.id)] ?? null
  } catch (e) {
    fail(e)
  }
}

export function summaryOf(folderID: number): index.FolderSummary | undefined {
  return store.folder.summaries[String(folderID)]
}

/** 摘要构成:main = N 话 · 大小 · 待传;time = 子树最新时间。
 * 拆开是因为列表视图时间有独立列,内联在摘要里会随前缀宽度参差不齐。 */
function summaryParts(folderID: number): { main: string[]; time: string } {
  const s = summaryOf(folderID)
  const main: string[] = []
  let time = ''
  if (s) {
    if (s.PackCount > 0) main.push(`${s.PackCount} 话`)
    else if (s.FileCount > 0) main.push(`${s.FileCount} 文件`)
    if (s.TotalSize > 0) main.push(humanSize(s.TotalSize))
    if (s.LatestAt > 0) time = shortTime(s.LatestAt)
    if (s.PendingCount > 0) main.push(`待传 ${s.PendingCount}`)
  }
  return { main, time }
}

/** 目录行摘要(卡片视图):CLI summaryLine 同款单行措辞(N 话 · 大小 · ← 时间 · 待传) */
export function summaryText(folderID: number): string {
  const { main, time } = summaryParts(folderID)
  if (time) main.push(`← ${time}`)
  return main.join(' · ')
}

/** 目录行摘要(列表视图):不含时间——时间走独立「修改时间」列对齐 */
export function folderSummaryText(folderID: number): string {
  return summaryParts(folderID).main.join(' · ')
}

/** 列表视图目录行的时间列:目录自身 ModifiedAt 恒为 0,展示子树最新时间 */
export function folderTimeText(folderID: number): string {
  return summaryParts(folderID).time
}

/** 封面 dataURL(带缓存 + 有界并发队列);取不到返回空串(调用方显示占位)。
 * TODO-10 出库后封面可能走网络(磁盘缓存未命中 → 远端 covers 命名空间),
 * 全量无上限预取会瞬间压满并发:这里限 4 路排队,负缓存防止对"无封面"反复请求。 */
const THUMB_CONCURRENCY = 4
let thumbInflight = 0
const thumbWaiters: Array<() => void> = []

export async function ensureThumb(fileID: number): Promise<string> {
  const hit = store.thumbs.get(fileID)
  if (hit !== undefined) return hit
  await acquireThumb()
  try {
    const again = store.thumbs.get(fileID) // 排队期间可能已被并发取到
    if (again !== undefined) return again
    const t = await API.GetCover(fileID)
    const url = thumbToDataUrl(t)
    store.thumbs.set(fileID, url)
    return url
  } catch {
    store.thumbs.set(fileID, '') // 负缓存:没有封面/暂不可达,不反复请求
    return ''
  } finally {
    releaseThumb()
  }
}

function acquireThumb(): Promise<void> {
  if (thumbInflight < THUMB_CONCURRENCY) {
    thumbInflight++
    return Promise.resolve()
  }
  return new Promise((res) =>
    thumbWaiters.push(() => {
      thumbInflight++
      res()
    })
  )
}

function releaseThumb() {
  thumbInflight--
  thumbWaiters.shift()?.()
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

/** 目录自有封面的 dataURL(v6):与 ensureThumb 同款缓存/队列/负缓存,只是
 * 缓存与取数(GetFolderCover)都按 folderID 走——两套 id 空间不混用。
 * 取不到返回空串,调用方回落派生宫格。 */
export async function ensureFolderCover(folderID: number): Promise<string> {
  const hit = store.folderThumbs.get(folderID)
  if (hit !== undefined) return hit
  await acquireThumb()
  try {
    const again = store.folderThumbs.get(folderID)
    if (again !== undefined) return again
    const t = await API.GetFolderCover(folderID)
    const url = thumbToDataUrl(t)
    store.folderThumbs.set(folderID, url)
    return url
  } catch {
    store.folderThumbs.set(folderID, '') // 负缓存:无封面/挂账中/暂不可达
    return ''
  } finally {
    releaseThumb()
  }
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
    store.folderDetail = null // 文件/目录详情互斥:打开一侧收起另一侧
    store.detail = await API.FileInfo(fileID)
    if (store.detail?.HasThumb) ensureThumb(fileID)
  } catch (e) {
    fail(e)
  }
}

/** 单击目录行:右栏展示目录元数据(交互约定:单击 = 详情、双击 = 进入)。
 * name/path 由当前列表条目与面包屑合成,summary 复用列表聚合结果;先取
 * GetFolderMeta 再一次性挂上(面板无"待补齐"中间态)。文件/目录详情互斥:
 * 打开一侧即收起另一侧。 */
let folderDetailSeq = 0
export async function openFolderDetail(folderID: number): Promise<boolean> {
  // 目录必须还在当前列表(250ms 单击判定窗口内可能已切目录):条目不在
  // (换目录/已删)则静默放弃——面板数据要从条目与面包屑合成快照
  const entry = store.folder.entries.find((e) => e.IsFolder && e.ID === folderID)
  if (!entry) return false
  const seq = ++folderDetailSeq
  try {
    const meta = await API.GetFolderMeta(folderID)
    if (seq !== folderDetailSeq) return true // 在途期间又点了别的目录:本结果作废
    const base = store.folder.crumbs.slice(1).map((c) => c.Name).join('/')
    store.detail = null
    store.folderDetail = {
      id: folderID,
      name: entry.Name,
      path: `${base ? '/' + base : ''}/${entry.Name}`,
      meta,
      summary: store.folder.summaries[String(folderID)] ?? null,
    }
    return true
  } catch (e) {
    fail(e)
    return false
  }
}

/** 右栏目录备注保存:只动 Note 一项(FolderMetaUpdate 其余字段留 nil = 不动),
 * 成功后就地更新面板快照——列表刷新由后端 index:changed 事件负责 */
export async function saveFolderNote(folderID: number, note: string): Promise<boolean> {
  const u = new index.FolderMetaUpdate()
  u.Note = note
  try {
    await API.UpdateFolderMeta(folderID, u)
    if (store.folderDetail?.id === folderID) store.folderDetail.meta.Note = note
    return true
  } catch (e) {
    fail(e)
    return false
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
    store.folderDetail = null
    toast('info', `已删除 ${files.length} 个文件、${folders.length} 个目录(远端占位由孤儿清理回收)`)
    return true
  } catch (e) {
    fail(e)
    return false
  }
}

export async function moveSelected(destID: number): Promise<boolean> {
  const files = [...store.selection]
  const folders = [...store.folderSelection]
  if (files.length === 0 && folders.length === 0) return false
  try {
    await API.MoveEntries(files, folders, destID)
    toast('info', `已移动 ${files.length} 个文件、${folders.length} 个目录`)
    return true
  } catch (e) {
    fail(e)
    return false
  }
}

/** 目录选择器(MoveDialog)专用:只读列某目录的面包屑与子文件夹,
 * 不碰全局 folder 状态(弹窗内浏览不能动 Files 页的导航位置)。 */
export async function peekFolders(
  id: number,
): Promise<{ crumbs: index.Crumb[]; folders: index.Entry[] } | null> {
  try {
    const v = await API.ListFolder(id)
    return { crumbs: v.Crumbs ?? [], folders: (v.Entries ?? []).filter((e) => e.IsFolder) }
  } catch (e) {
    fail(e)
    return null
  }
}

/** 目录选择器:在虚拟路径下建目录(EnsureFolder 幂等、多级宽容),返回 id */
export async function ensureFolderAt(path: string): Promise<number | null> {
  try {
    return await API.EnsureFolder(path)
  } catch (e) {
    fail(e)
    return null
  }
}

/** 在当前目录下建虚拟文件夹(可多级,幂等);零远端流量(TODO-18) */
export async function createFolder(name: string): Promise<boolean> {
  const base = store.folder.crumbs
    .slice(1)
    .map((c) => c.Name)
    .join('/')
  try {
    await API.EnsureFolder(base ? `/${base}/${name}` : `/${name}`)
    toast('info', '文件夹已创建')
    return true
  } catch (e) {
    fail(e)
    return false
  }
}

/** 重命名目录(纯索引零流量);撞名/非法名由后端报错走 toast(TODO-19) */
export async function renameFolder(folderID: number, name: string): Promise<boolean> {
  try {
    await API.RenameFolder(folderID, name)
    // 右栏目录详情若正展示该目录,同步改名(path 只换最后一段)
    const d = store.folderDetail
    if (d?.id === folderID) {
      d.name = name
      d.path = d.path.slice(0, d.path.lastIndexOf('/') + 1) + name
    }
    toast('info', '已重命名')
    return true
  } catch (e) {
    fail(e)
    return false
  }
}

// 文件重命名:与目录同款纯索引零流量。详情面板若开着就同步改名,
// 免得面板停留旧名(loadFolder 会经 index:changed 防抖刷新列表)
export async function renameFile(fileID: number, name: string): Promise<boolean> {
  try {
    await API.RenameFile(fileID, name)
    if (store.detail?.ID === fileID) {
      store.detail.Name = name
      store.detail.Path = store.detail.Path.slice(0, store.detail.Path.lastIndexOf('/') + 1) + name
    }
    toast('info', '已重命名')
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

// ---- 文件元数据(TODO-17):tag 挂文件 + 手动封面 ----

export async function getFileMeta(fileID: number): Promise<index.FileMeta | null> {
  try {
    return await API.GetFileMeta(fileID)
  } catch (e) {
    fail(e)
    return null
  }
}

export async function saveFileMeta(fileID: number, u: index.FileMetaUpdate): Promise<boolean> {
  try {
    await API.UpdateFileMeta(fileID, u)
    // 详情面板若开着就同步改过的字段(nil = 未动),免得面板停留旧值
    if (store.detail?.ID === fileID) {
      if (u.Note !== undefined) store.detail.Note = u.Note
      if (u.Tags !== undefined) store.detail.Tags = u.Tags
    }
    toast('info', '文件元数据已保存')
    return true
  } catch (e) {
    fail(e)
    return false
  }
}

/** 选一张本地图片(GUI 封面导入);取消返回空串 */
export async function pickImageFile(): Promise<string> {
  try {
    return await API.PickImageFile()
  } catch (e) {
    fail(e)
    return ''
  }
}

/**
 * 导入/清除文件封面(localPath 空 = 清除)。TODO-10:导入走网络,断网自动
 * 回退出站箱(deferred)。成功后失效封面缓存——旧图与"无封面"负缓存都不
 * 可信,强制下次重取;详情面板同步 HasThumb。
 */
export async function setFileCover(fileID: number, localPath: string): Promise<'ok' | 'deferred' | null> {
  try {
    const deferred = await API.SetFileCover(fileID, localPath)
    store.thumbs.delete(fileID)
    if (store.detail?.ID === fileID) {
      store.detail.HasThumb = localPath !== ''
      if (localPath && !deferred) ensureThumb(fileID)
    }
    toast('info', localPath ? (deferred ? '封面已入出站箱:outbox push 后对其他设备可见' : '封面已导入') : '封面已清除')
    return deferred ? 'deferred' : 'ok'
  } catch (e) {
    fail(e)
    return null
  }
}

/**
 * 导入/清除目录自有封面(v6,localPath 空 = 清除):后端同 SetFileCover
 * 管线——已有封面删旧换新,断网回退出站箱。成功后失效缓存;右栏面板若
 * 正展示该目录,就地把摘要的 CustomCover 置位/复位,面板与卡片即时跟上
 * (列表级刷新由 index:changed 事件驱动)。
 */
export async function setFolderCover(
  folderID: number,
  localPath: string,
): Promise<'ok' | 'deferred' | null> {
  try {
    const deferred = await API.SetFolderCover(folderID, localPath)
    store.folderThumbs.delete(folderID)
    if (store.folderDetail?.id === folderID && store.folderDetail.summary) {
      store.folderDetail.summary.CustomCover = localPath !== '' && !deferred
    }
    toast('info', localPath ? (deferred ? '封面已入出站箱:outbox push 后对其他设备可见' : '封面已导入') : '封面已清除')
    return deferred ? 'deferred' : 'ok'
  } catch (e) {
    fail(e)
    return null
  }
}

// ---- Transfers 页 ----

export function goTransfers() {
  store.page = 'transfers'
}

export async function upload(kind: 'files' | 'folder') {
  try {
    // 文件夹走多选目录对话框(空表=取消,静默返回);核心管线一目录一任务(叶子打包)
    const paths = kind === 'files' ? await API.PickFiles() : await API.PickDirs()
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
  const folders = [...store.folderSelection]
  if (files.length + folders.length === 0) return
  try {
    const dir = await API.PickDir()
    if (!dir) return
    // 文件与目录同收:目录整棵子树按虚拟结构还原到所选目录的 <目录名>/ 下
    const plan = await API.DownloadEntries(files, folders, dir)
    const extra = plan.skipped > 0 ? `,跳过 ${plan.skipped} 个待上传` : ''
    toast('info', `已入队 ${plan.queued} 个下载${extra}`)
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
    store.drives = (await API.ListDrives()) ?? []
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
