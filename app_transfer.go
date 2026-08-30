package main

// app_transfer.go — 传输、设置与维护类绑定。对话框在 Wails v2.15 的 JS 运行时
// 侧不可用(runtime.d.ts 无 Open*Dialog 导出),只能落到 Go 绑定(PickFiles/PickDir)。

import (
	"context"
	"fmt"

	"kist/internal/backup"
	"kist/internal/config"
	"kist/internal/errs"
	"kist/internal/transfer"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// PickFiles 打开多选文件对话框;取消返回空表(前端按"未选择"处理)。
func (a *App) PickFiles() (paths []string, err error) {
	defer a.panicGuard(&err)
	if a.ctx == nil {
		return nil, a.wrap(errs.New(errs.Internal, "窗口尚未就绪"))
	}
	paths, err = wruntime.OpenMultipleFilesDialog(a.ctx, wruntime.OpenDialogOptions{
		Title: "选择要上传的文件",
	})
	if err != nil {
		return nil, a.wrap(errs.New(errs.BadConfig, err.Error()))
	}
	if paths == nil {
		paths = []string{}
	}
	return paths, nil
}

// PickDir 打开目录选择对话框(上传文件夹/下载落盘目录共用);取消返回空串。
func (a *App) PickDir() (dir string, err error) {
	defer a.panicGuard(&err)
	if a.ctx == nil {
		return "", a.wrap(errs.New(errs.Internal, "窗口尚未就绪"))
	}
	dir, err = wruntime.OpenDirectoryDialog(a.ctx, wruntime.OpenDialogOptions{
		Title: "选择目录",
	})
	if err != nil {
		return "", a.wrap(errs.New(errs.BadConfig, err.Error()))
	}
	return dir, nil
}

// PickImageFile 打开单选图片对话框(GUI 导入文件封面,TODO-17);取消返回空串。
// 过滤器与 MakeThumbnail 的嗅探范围一致(jpeg/png/gif/bmp/webp)。
func (a *App) PickImageFile() (path string, err error) {
	defer a.panicGuard(&err)
	if a.ctx == nil {
		return "", a.wrap(errs.New(errs.Internal, "窗口尚未就绪"))
	}
	path, err = wruntime.OpenFileDialog(a.ctx, wruntime.OpenDialogOptions{
		Title: "选择封面图片",
		Filters: []wruntime.FileFilter{
			{DisplayName: "图片 (*.jpg;*.jpeg;*.png;*.gif;*.bmp;*.webp)",
				Pattern: "*.jpg;*.jpeg;*.png;*.gif;*.bmp;*.webp"},
		},
	})
	if err != nil {
		return "", a.wrap(errs.New(errs.BadConfig, err.Error()))
	}
	return path, nil
}

// UploadPaths 异步入队上传(文件/文件夹;文件夹默认按叶子目录打包,TODO-15 行为),
// 立即返回入队数;进度经 transfer:update 事件推送。destFolderID 0 归一化为根。
func (a *App) UploadPaths(paths []string, destFolderID int64) (n int, err error) {
	defer a.panicGuard(&err)
	if len(paths) == 0 {
		return 0, a.wrap(errs.New(errs.BadConfig, "未选择任何文件"))
	}
	if err := a.requireUnlocked(); err != nil {
		return 0, a.wrap(err)
	}
	mgr, err := a.requireMgr()
	if err != nil {
		return 0, a.wrap(err)
	}
	if destFolderID <= 0 {
		destFolderID = 1
	}
	n, err = mgr.UploadPaths(a.callCtx(), paths, destFolderID)
	if err != nil {
		return n, a.wrap(err)
	}
	return n, nil
}

// DownloadTo 异步入队下载(pack 默认解压还原目录),立即返回入队数。
func (a *App) DownloadTo(fileIDs []int64, destDir string) (n int, err error) {
	defer a.panicGuard(&err)
	if len(fileIDs) == 0 {
		return 0, a.wrap(errs.New(errs.BadConfig, "未选择任何文件"))
	}
	if destDir == "" {
		return 0, a.wrap(errs.New(errs.BadConfig, "未选择下载目录"))
	}
	if err := a.requireUnlocked(); err != nil {
		return 0, a.wrap(err)
	}
	mgr, err := a.requireMgr()
	if err != nil {
		return 0, a.wrap(err)
	}
	n, err = mgr.DownloadTo(a.callCtx(), fileIDs, destDir)
	if err != nil {
		return n, a.wrap(err)
	}
	return n, nil
}

// CancelTransfer 取消一个传输;不存在或已结束返回 NOT_FOUND。
func (a *App) CancelTransfer(id string) (err error) {
	defer a.panicGuard(&err)
	mgr, err := a.requireMgr()
	if err != nil {
		return a.wrap(err)
	}
	if !mgr.Cancel(id) {
		return a.wrap(errs.New(errs.NotFound, "传输不存在或已结束"))
	}
	return nil
}

// Transfers 返回全部传输快照(按入队顺序);Transfers 页初始化用,
// 之后靠 transfers:changed/transfer:update 增量。
func (a *App) Transfers() []transfer.Transfer {
	if a.mgr == nil {
		return []transfer.Transfer{}
	}
	return a.mgr.Snapshot()
}

// ---- 设置 ----

// GetSettings 回显行为偏好;配置读失败时给默认值(与 config.normalize 一致)。
func (a *App) GetSettings() config.Settings {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cfg != nil {
		return a.cfg.Settings
	}
	return config.Settings{Concurrency: 2, ChunkMiB: 4, OutboxPushFail: "keep", SizePadding: "on", CoverCacheMB: 512}
}

// SaveSettings 保存行为偏好(并发/块大小对进行中传输的下一任务生效,
// 管线闭包每轮重读)。config.Save 的 normalize 会夹取非法值。
func (a *App) SaveSettings(s config.Settings) (err error) {
	defer a.panicGuard(&err)
	a.mu.Lock()
	cfg := a.cfg
	if cfg == nil {
		cfg = &config.StoredConfig{}
	}
	cfg.Settings = s
	if serr := config.Save(cfg); serr != nil {
		a.mu.Unlock()
		return a.wrap(serr)
	}
	a.cfg = cfg
	a.mu.Unlock()
	return nil
}

// ---- 维护 ----

// BackupIndexNow 手动备份索引(Settings"立即备份");成功后更新会话基线。
func (a *App) BackupIndexNow() (info backup.BackupInfo, err error) {
	defer a.panicGuard(&err)
	if err := a.requireUnlocked(); err != nil {
		return info, a.wrap(err)
	}
	store, err := a.requireStore()
	if err != nil {
		return info, a.wrap(err)
	}
	if _, err := a.requireDB(); err != nil {
		return info, a.wrap(err)
	}
	mk, _ := a.mkSnapshot()
	ctx, cancel := context.WithTimeout(context.Background(), backupTimeout)
	defer cancel()
	info, err = backup.BackupNow(ctx, mk, a.db, store)
	if err != nil {
		return info, a.wrap(err)
	}
	a.mu.Lock()
	a.lastBackupRev = info.Revision
	a.mu.Unlock()
	a.emitNotify("info", fmt.Sprintf("索引已备份(revision %d,%s)",
		info.Revision, info.At.Format("2006-01-02 15:04")))
	return info, nil
}

// GCReport 孤儿清理报告:TrashOrDeleted 在预览时是待删清单、实跑时是已删清单;
// Orphans(远端有、索引无)与 CoverOrphans(covers/ 命名空间同款,TODO-10)
// 只报告不删——删除孤儿需要用户确认语义,本期不做。
type GCReport struct {
	TrashOrDeleted []string
	Orphans        []string
	CoverOrphans   []string
	DryRun         bool
}

// PreviewGC 预览:trash blob 待删清单 + 孤儿清单,不动远端。
func (a *App) PreviewGC() (r GCReport, err error) {
	defer a.panicGuard(&err)
	r, err = a.runGC(true)
	return r, a.wrap(err)
}

// RunGC 实删 trash blob(软删除文件的远端占位);孤儿仍然只报告。
func (a *App) RunGC() (r GCReport, err error) {
	defer a.panicGuard(&err)
	r, err = a.runGC(false)
	if err == nil && len(r.TrashOrDeleted) > 0 {
		a.emitNotify("info", fmt.Sprintf("已清理 %d 个远端 blob", len(r.TrashOrDeleted)))
	}
	return r, a.wrap(err)
}

func (a *App) runGC(dryRun bool) (GCReport, error) {
	store, err := a.requireStore()
	if err != nil {
		return GCReport{}, err
	}
	db, err := a.requireDB()
	if err != nil {
		return GCReport{}, err
	}
	res, err := transfer.RunGC(a.callCtx(), store, db, dryRun)
	if err != nil {
		return GCReport{}, err
	}
	if res.Deleted == nil {
		res.Deleted = []string{}
	}
	if res.Orphans == nil {
		res.Orphans = []string{}
	}
	if res.CoverOrphans == nil {
		res.CoverOrphans = []string{}
	}
	return GCReport{TrashOrDeleted: res.Deleted, Orphans: res.Orphans, CoverOrphans: res.CoverOrphans, DryRun: dryRun}, nil
}
