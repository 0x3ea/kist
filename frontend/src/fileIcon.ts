// fileIcon.ts — 文件类型 → lucide 图标与色调的统一映射:
// 卡片宫格/列表/详情/元数据对话框共用,替代原先各处不一致的 emoji/色块占位。
// 色调刻意压低明度与饱和度,在深色主题里做"识别信号"而非"视觉主角"。
import type { Component } from 'vue'
import {
  BookOpen,
  File,
  FileArchive,
  FileAudio,
  FileImage,
  FileText,
  FileVideo,
  Folder,
  FolderArchive,
} from 'lucide-vue-next'

export interface FileKind {
  icon: Component
  color: string
}

// 目录恒为蓝(与主题 accent 同族);pack 是"一话一对象"的目录打包物,
// 用紫色 FolderArchive 与普通目录/普通压缩包都区分开
export const folderKind: FileKind = { icon: Folder, color: '#6f9fdd' }
const packKind: FileKind = { icon: FolderArchive, color: '#b48cf2' }

// 图像扩展名与后端 isImagePath 保持一致(internal/transfer/pack.go),
// 新增类型两边同步改
const imageExt = new Set(['.jpg', '.jpeg', '.png', '.gif', '.bmp', '.webp'])
const videoExt = new Set(['.mp4', '.mkv', '.avi', '.mov', '.webm', '.wmv', '.flv', '.m4v'])
const bookExt = new Set(['.epub', '.mobi', '.azw3'])
const pdfExt = new Set(['.pdf'])
const audioExt = new Set(['.mp3', '.flac', '.m4a', '.wav', '.ogg', '.opus'])
const archiveExt = new Set(['.zip', '.rar', '.7z'])

const byExt: Array<[Set<string>, FileKind]> = [
  [imageExt, { icon: FileImage, color: '#5cc8a8' }],
  [videoExt, { icon: FileVideo, color: '#e08fb6' }],
  [bookExt, { icon: BookOpen, color: '#e0b25c' }],
  [pdfExt, { icon: FileText, color: '#e07b6a' }],
  [audioExt, { icon: FileAudio, color: '#6fc3e8' }],
  [archiveExt, { icon: FileArchive, color: '#c9a06f' }],
]

// fileKind 按扩展名(不区分大小写)挑图标;未知类型回落中性 File。
// dot <= 0 同时覆盖"无扩展名"与 dotfile(".gitignore" 不是扩展名 .gitignore)
export function fileKind(name: string, pack = false): FileKind {
  if (pack) return packKind
  const fallback = { icon: File, color: '#8ba0b8' }
  const dot = name.lastIndexOf('.')
  if (dot <= 0) return fallback
  const ext = name.slice(dot).toLowerCase()
  for (const [set, kind] of byExt) {
    if (set.has(ext)) return kind
  }
  return fallback
}
