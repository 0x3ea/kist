# Phase 5 — 索引云备份与多设备恢复(CLI)internal/backup

> 状态:未开始
> 前置:Phase 4
> 产出:`internal/backup/backup.go` + 测试;`remote.Store` 增 index blob 方法;`kistctl backup/pull`;e2e 双设备场景
> (GUI 侧的接线——按钮、新设备向导、自动备份——在 Phase 7 落地)

## 目标

索引不再只活在一台机器上:CLI 下手动加密备份到 `/kist/index.enc`,新设备凭口令拉回 keyfile + 索引,恢复完整文件目录结构。**本阶段结束即"CLI 全流程"完成里程碑**,之后才引入 GUI 环境。

## 要做什么(任务清单)

- [ ] `remote.Store` 增 `PutIndexBlob(f *os.File)` / `GetIndexBlob(tmpPath string) error`(远端固定名 `/kist/index.enc`)
- [ ] `internal/backup`:BackupNow / PullRemote(LWW)
- [ ] `kistctl backup` / `kistctl pull` 子命令
- [ ] e2e 扩展:双 KIST_HOME 家目录互推与冲突归档

## 设计说明

### 备份格式

完全复用 blob 加密格式:对 `index.db` 的快照做 `NewBlobWriter` 流式加密,`EncryptOptions` 填入真实字段:`Mtime`(备份时刻)、`Revision`(备份时的库 revision)、`DeviceID`(本机 device_id)。远端固定名 `/kist/index.enc`。

备注、缩略图、user_meta 都存在 `index.db` 里,**随加密备份一起同步,新设备 pull 后立即可见**,无需单独通道。备份是整库上传,所以缩略图体积就是备份流量:最长边 512px、典型 25–45KB/张(上限 128KB),千张图索引约 25–45MB,可接受;若未来需要更大缩略图,应改为"缩略图单独成库/单独通道"再评估。

### BackupNow

```go
type BackupInfo struct { Revision uint64; Size int64; At time.Time }
func BackupNow(mk crypto.MasterKey, db *index.DB, s *remote.Store) (BackupInfo, error)
```

流程:`SnapshotTo(tmp)`(VACUUM INTO,一致性快照,不必停库)→ 加密 → PutIndexBlob → `SetLastBackupAt`。

CLI 阶段仅手动触发(`kistctl backup`);防抖自动备份与退出前备份属于应用生命周期,Phase 7 在 GUI 里接。

### PullRemote(LWW 决策表)

```go
type PullResult struct {
    Action string // "replaced" | "noop" | "local-newer"
    RemoteRev, LocalRev uint64
    RemoteDevice string
}
func PullRemote(mk crypto.MasterKey, s *remote.Store, db *index.DB) (PullResult, error)
```

| 比较 | 动作 |
|---|---|
| remote.rev > local.rev | 旧库归档到 `backups/index-<rev>-<ts>.db` → `ReplaceWith`(远端快照)→ `replaced` |
| remote.rev == local.rev | `noop` |
| remote.rev < local.rev | `local-newer`,不动本地,提示"本地更新,建议先备份推送" |

替换发生且 `remote.device_id != 本机 device_id` 时附带提示:"另一台设备的改动已被当前版本覆盖,被覆盖的库已归档在 backups/ 可手动找回"。**明确不做行级合并**:单用户、同时单写者假设;落后一方的改动进归档而不是 merge。

### 新设备恢复(CLI 路径)

本地无 keyfile → GET 远端 keyfile → `Unlock(pass)`(失败即 `AUTH_FAILED`)→ 缓存到本地 `~/.config/kist/keyfile` → `PullRemote`。

### kistctl

```
kistctl backup                # BackupNow,打印 revision / 大小 / 时间
kistctl pull --pass-stdin     # 拉远端 keyfile(如需)+ PullRemote,打印 Action 与归档提示
```

## 预期结果

两个"设备"(两个 KIST_HOME 家目录)各自上传文件后,互相 pull 都能看到对方的文件记录;远端始终只多一个 `index.enc`。至此 CLI 覆盖:配置、初始化、上传、浏览/搜索、下载、删除、清理、备份、恢复——全流程。

## 验收标准

1. `go test ./internal/backup/ -race -count=1` 全绿(基于 e2e 的本地 WebDAV + 双 KIST_HOME),至少覆盖:
   - 设备 A put 两文件(其一为图片并写了备注)→ backup → 设备 B(KIST_HOME=空目录)pull → B 的 ls 出现同样记录,**缩略图与备注完整**;A、B revision 一致
   - B 再 put 一文件 → backup → A pull → A 看到三个文件(replaced 分支)
   - A 本地有更新未备份时 pull → `local-newer`,本地不变
   - 被替换方在 `backups/` 有归档文件,可单独打开校验
   - 错口令 pull → `AUTH_FAILED`
2. 手动演练(本地 WebDAV 或真实网盘):
   ```bash
   KIST_HOME=/tmp/devA go run ./cmd/kistctl backup
   KIST_HOME=/tmp/devB go run ./cmd/kistctl pull --pass-stdin
   KIST_HOME=/tmp/devB go run ./cmd/kistctl ls /
   ```
3. 全量回归:`go test ./... -race -count=1` 全绿
