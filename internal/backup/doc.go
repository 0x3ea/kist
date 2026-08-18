// Package backup 实现索引云备份与多设备恢复。
//
// 备份 = VACUUM INTO 一致性快照 → 加密 → PUT /kist/index.enc(备注/缩略图随库同步);
// 恢复按 LWW(以 revision 判定,不信任系统时钟),落后一方的本地库归档保留而非合并。
// 设计文档:docs/phase-5-backup-sync.md
package backup
