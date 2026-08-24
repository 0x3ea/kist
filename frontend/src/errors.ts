// errors.ts — 绑定层错误的前端解析:Go 侧把 AppError 包成 "[CODE] Msg"
// 字符串(Wails 对 error 只传字符串),这里反解出错误码并映射可读文案。
// store 里所有 catch 统一走 parseErr(String(e)).text 出 toast。

const CODE_TEXT: Record<string, string> = {
  BAD_CONFIG: '配置无效',
  NOT_CONFIGURED: '尚未配置网盘',
  DAV_ERROR: '网盘通信失败',
  AUTH_FAILED: '口令错误',
  NOT_FOUND: '未找到目标',
  CORRUPT: '数据损坏或密钥不符',
  LOCKED: '尚未解锁',
  INTERNAL: '内部错误',
}

export interface ParsedError {
  code: string
  msg: string
  /** 面向用户的完整文案 */
  text: string
}

export function parseErr(raw: string): ParsedError {
  // Msg 本身可能含换行,用 [\s\S] 兜底;匹配不上按原始文本整体展示
  const m = /^\[([A-Z_]+)\]\s?([\s\S]*)$/.exec(raw ?? '')
  if (!m) return { code: 'INTERNAL', msg: raw ?? '', text: raw ?? '未知错误' }
  const label = CODE_TEXT[m[1]] ?? m[1]
  return { code: m[1], msg: m[2], text: m[2] ? `${label}:${m[2]}` : label }
}
