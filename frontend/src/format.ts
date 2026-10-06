// format.ts — 展示格式化,与 CLI(kistctl)同款规则:humanSize 的 KB/MB/GB
// 一位小数、shortTime 统一年月日不省年份。摘要行与传输页共用。

export function humanSize(n: number): string {
  if (n >= 1 << 30) return (n / (1 << 30)).toFixed(1) + 'GB'
  if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1) + 'MB'
  if (n >= 1 << 10) return (n / (1 << 10)).toFixed(1) + 'KB'
  return `${n}B`
}

/** 统一年月日(“← 2025-08-01”形态),不省年份 */
export function shortTime(unix: number): string {
  if (!unix) return ''
  const d = new Date(unix * 1000)
  const two = (v: number) => String(v).padStart(2, '0')
  return `${d.getFullYear()}-${two(d.getMonth() + 1)}-${two(d.getDate())}`
}

/** 详情面板用的时间:精确到分 */
export function fullTime(unix: number | null | undefined): string {
  if (!unix) return '—'
  const d = new Date(unix * 1000)
  const two = (v: number) => String(v).padStart(2, '0')
  return `${d.getFullYear()}-${two(d.getMonth() + 1)}-${two(d.getDate())} ${two(d.getHours())}:${two(d.getMinutes())}`
}

/** 传输阶段 → 中文标签 */
export const PHASE_TEXT: Record<string, string> = {
  queued: '排队中',
  encrypting: '加密中',
  uploading: '上传中',
  downloading: '下载中',
  decrypting: '解密中',
  deferred: '已入出站箱',
  done: '完成',
  error: '失败',
  canceled: '已取消',
}

export function phaseText(p: string): string {
  return PHASE_TEXT[p] ?? p
}

/** 未到终态的阶段集合——侧栏徽标与传输页的"进行中"判定共用 */
const RUNNING_PHASES = new Set(['queued', 'encrypting', 'uploading', 'downloading', 'decrypting'])

export function isRunningPhase(p: string): boolean {
  return RUNNING_PHASES.has(p)
}
