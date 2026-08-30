package main

// app.go — Wails GUI 壳的核心:App 结构、生命周期、错误包装、解锁态管理与
// 状态/解锁类绑定。GUI 不引入业务逻辑,只编排 internal/*(参考 cmd/kistctl
// 的 loadStore/unlockMK/newManager 三个组装 helper)。
//
// 并发纪律:mu 保护 cfg/store/mk/unlocked/backupTimer/lastBackupRev;
// db 与 mgr 在 startup 一次构造视为不可变(index.ReplaceWith 在原句柄上换库,
// Manager 无需重建);绑定方法由 Wails 在独立 goroutine 调用,全部走锁。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"kist/internal/backup"
	"kist/internal/config"
	"kist/internal/crypto"
	"kist/internal/dav"
	"kist/internal/errs"
	"kist/internal/index"
	"kist/internal/logging"
	"kist/internal/remote"
	"kist/internal/transfer"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// 防抖自动备份的静默期:索引变更后等这么多秒无新变更才触发(phase-7 约定)。
const autoBackupDebounce = 30 * time.Second

// 备份类网络操作的超时,防止退出前备份把关窗挂死。
const backupTimeout = 60 * time.Second

// App 是 Wails 绑定的 GUI 壳。
type App struct {
	ctx context.Context

	mu       sync.Mutex
	cfg      *config.StoredConfig // config.Load 的常驻副本;URL/Username 空 = 未配置
	store    *remote.Store        // 已配置且装配成功;nil = 不可用
	mk       crypto.MasterKey     // 解锁后的主密钥;Lock 时 Wipe 清零
	unlocked bool

	db  *index.DB         // startup 打开(新机器得到空库),shutdown 关闭
	mgr *transfer.Manager // startup 构造;MK 闭包读解锁态,Lock 后任务以 LOCKED 失败

	backupTimer   *time.Timer // index:changed 防抖定时器(time.AfterFunc 重置法)
	lastBackupRev uint64      // 会话内最近一次成功备份的 revision(基线 = 解锁时)
	backingUp     atomic.Bool // 备份互斥(防抖/手动/退出并发)
}

// NewApp 创建 GUI 壳实例;装配发生在 startup(此时才有 Wails ctx)。
func NewApp() *App {
	return &App{}
}

// ---- 生命周期 ----

// startup:日志 → 配置 → 索引库(总是打开) → 远端装配(已配置时) → 传输管线 → 派发初始状态。
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	if err := logging.Setup(); err != nil {
		slog.Warn("kist.log 不可用,日志降级 stderr", "err", err)
	}
	if cfg, err := config.Load(); err != nil {
		// 配置损坏不阻断启动:向导允许重配
		slog.Error("读取 config.json 失败", "err", err)
	} else {
		a.cfg = cfg
		if a.cfg.ActiveDrive() != nil {
			// 有活动盘才开库:库文件名锚定盘 ID,旧单库文件在此随迁(TODO-21)。
			// 未配置(无任何盘)时 db/mgr 保持 nil,首配在 SaveDrive 里补开。
			a.reopenVaultLocked()
		}
	}
	wruntime.EventsEmit(ctx, "app:state", a.GetAppState())
}

// shutdown:停防抖 → 条件性退出前备份(已解锁且 revision 落后) → 关库。
// 在途传输随进程终止,临时产物由下次启动的 NewManager 清理(已知边界,见 phase-7 文档)。
func (a *App) shutdown(ctx context.Context) {
	a.mu.Lock()
	if a.backupTimer != nil {
		a.backupTimer.Stop()
		a.backupTimer = nil
	}
	unlocked := a.unlocked
	lastRev := a.lastBackupRev
	a.mu.Unlock()

	if unlocked && a.db != nil {
		if rev, err := a.db.Revision(); err == nil && rev > lastRev {
			// 退出前备份不设 AutoBackup 前提(phase-7 约定);失败仅记日志——
			// 窗口正在关闭,弹提示无意义。
			bctx, cancel := context.WithTimeout(context.Background(), backupTimeout)
			if mk, ok := a.mkSnapshot(); ok {
				if _, err := backup.BackupNow(bctx, mk, a.db, a.storeSnapshot()); err != nil {
					slog.Error("退出前索引备份失败", "err", err)
				}
			}
			cancel()
		}
	}
	if a.db != nil {
		a.db.Close()
	}
}

// ---- 错误包装 ----

// codedError 让前端能从错误字符串里解析出错误码:Wails 把绑定方法的 error
// 序列化为字符串 reject 前端 promise,而 errs.AppError.Error() 只有 Msg 没有
// Code(internal/errs 不动以免影响 CLI 输出),壳层统一包 "[CODE] Msg"。
type codedError struct {
	code, msg string
}

func (e *codedError) Error() string { return "[" + e.code + "] " + e.msg }

// wrap 把任意错误归类为 AppError 后包成 codedError;已包装的原样返回。
func (a *App) wrap(err error) error {
	if err == nil {
		return nil
	}
	var ce *codedError
	if errors.As(err, &ce) {
		return err
	}
	ae, _ := errs.From(err).(*errs.AppError) // From 对非 AppError 恒归类为 *AppError
	if ae == nil {
		ae = &errs.AppError{Code: errs.Internal, Msg: err.Error()}
	}
	return &codedError{code: ae.Code, msg: ae.Msg}
}

// panicGuard 把绑定方法里的 panic 转成 INTERNAL codedError,防止裸 panic
// 击穿 Wails 崩掉窗口。用法:defer a.panicGuard(&err)(须配合命名返回值)。
func (a *App) panicGuard(errp *error) {
	if v := recover(); v != nil {
		slog.Error("绑定方法 panic 已恢复", "panic", v)
		*errp = &codedError{code: errs.Internal, msg: fmt.Sprintf("内部错误:%v", v)}
	}
}

// ---- 组装与解锁态 helper(cmd/kistctl 同款逻辑的 GUI 版) ----

// buildStore 由盘档案装配远端存储(loadStore 的核心步骤)。
func buildStore(d *config.Drive) (*remote.Store, error) {
	c, err := dav.New(dav.Config{URL: d.URL, Username: d.Username, Password: d.Password, RootPath: d.RootPath})
	if err != nil {
		return nil, errs.Wrap(errs.BadConfig, err)
	}
	return remote.NewStore(c, d.RootPath), nil
}

// reopenVaultLocked 装配活动盘三件套:关旧库 → 索引文件随迁(幂等)→ 开新库 →
// 重建 store 与传输管线。换盘/首配/开新库都走这里。
// 并发纪律:调用方须持 mu,且保证管线空闲(mgr 为 nil 或 Idle)——mgr 的 DB 是
// 直接引用,不做热换,换库必重建。
func (a *App) reopenVaultLocked() {
	if a.backupTimer != nil {
		a.backupTimer.Stop()
		a.backupTimer = nil
	}
	a.mgr = nil // 旧管线引用旧库句柄,必须随库重建
	if a.db != nil {
		a.db.Close()
		a.db = nil
	}
	d := a.cfg.ActiveDrive()
	if d == nil {
		a.store = nil
		return
	}
	if err := index.MigrateIndexFile(config.LegacyIndexPath(), config.DriveIndexPath(d.ID)); err != nil {
		slog.Error("索引文件随迁失败", "err", err)
	}
	db, err := index.Open(config.DriveIndexPath(d.ID))
	if err != nil {
		slog.Error("打开索引库失败", "err", err)
		a.store = nil
		return
	}
	a.db = db
	a.rebuildStoreLocked()
	a.buildMgrLocked()
	a.lastBackupRev = 0 // 基线随库重设
	if rev, rerr := db.Revision(); rerr == nil {
		a.lastBackupRev = rev
	}
}

// archiveLocalIndexLocked 把活动盘的本地索引文件归档到 backups/(开新库前的
// 清场:旧文件可能是上一任库的行,不能混入新库)。库为空时不动文件返回空串;
// 归档失败返回错误(调用方沿用原文件开库,仅告警)。调用方须持 mu 且管线空闲;
// 调用后应 reopenVaultLocked。
func (a *App) archiveLocalIndexLocked() (string, error) {
	if a.db == nil || a.cfg == nil {
		return "", nil
	}
	d := a.cfg.ActiveDrive()
	if d == nil {
		return "", nil
	}
	if s, err := a.db.FolderSummary(1); err == nil && s.FileCount == 0 && s.PendingCount == 0 {
		return "", nil // 空库直接复用文件,不值得归档
	}
	src := config.DriveIndexPath(d.ID)
	if err := a.db.Close(); err != nil {
		return "", err
	}
	a.db = nil
	bdir := config.BackupDir()
	if err := os.MkdirAll(bdir, 0o700); err != nil {
		return "", err
	}
	dst := filepath.Join(bdir, fmt.Sprintf("index-%s-%d.db", d.ID, time.Now().Unix()))
	if err := os.Rename(src, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// rebuildStoreLocked 按活动盘重建 store;调用方须持 mu。
func (a *App) rebuildStoreLocked() {
	a.store = nil
	d := a.cfg.ActiveDrive()
	if d == nil {
		return
	}
	store, err := buildStore(d)
	if err != nil {
		slog.Error("远端客户端装配失败", "err", err)
		return
	}
	a.store = store
}

// buildMgrLocked 按当前 db/store 构建传输管线;无库不构建。
func (a *App) buildMgrLocked() {
	if a.db == nil {
		return
	}
	// MK 闭包读解锁态:未解锁返回 false,任务以 errs.Locked 失败(push 类纯密文
	// 任务不取 key,不受影响)。Concurrency/ChunkMiB/NoPad 每轮重读,改设置即时生效。
	a.mgr = transfer.NewManager(transfer.Deps{
		Remote:      a.store,
		DB:          a.db,
		MK:          a.mkSnapshot,
		Concurrency: a.settingInt(func(s config.Settings) int { return s.Concurrency }),
		ChunkMiB:    a.settingInt(func(s config.Settings) int { return s.ChunkMiB }),
		NoPad: func() bool {
			a.mu.Lock()
			defer a.mu.Unlock()
			return a.cfg != nil && a.cfg.Settings.SizePadding == "off"
		},
		PushFailDiscard: func() bool {
			a.mu.Lock()
			defer a.mu.Unlock()
			return a.cfg != nil && a.cfg.Settings.OutboxPushFail == "discard"
		},
		Emit: a.onTransferEvent,
	})
}

// storeSnapshot 取当前 store 指针(可能为 nil)。
func (a *App) storeSnapshot() *remote.Store {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.store
}

// mkSnapshot 是注入管线的解锁态闭包:未解锁返回 false → 任务以 errs.Locked 失败。
func (a *App) mkSnapshot() (crypto.MasterKey, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mk, a.unlocked
}

// settingInt 在 mu 下读配置数值项。
func (a *App) settingInt(get func(config.Settings) int) func() int {
	return func() int {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.cfg == nil {
			return 0 // Manager 对 0 取默认(并发 2 / 块 4MiB)
		}
		return get(a.cfg.Settings)
	}
}

// callCtx 返回绑定调用的上下文;窗口关闭时 a.ctx 取消,长任务随之终止。
func (a *App) callCtx() context.Context {
	if a.ctx != nil {
		return a.ctx
	}
	return context.Background()
}

// requireDB / requireStore / requireMgr:绑定方法的前置检查。
func (a *App) requireDB() (*index.DB, error) {
	if a.db == nil {
		return nil, errs.New(errs.Internal, "索引库不可用:请查看 KIST_HOME/kist.log 后重启")
	}
	return a.db, nil
}

func (a *App) requireStore() (*remote.Store, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cfg == nil || !a.cfg.Configured() {
		return nil, errs.New(errs.NotConfigured, "尚未配置网盘:请先在向导或设置里填写 WebDAV 配置")
	}
	if a.store == nil {
		return nil, errs.New(errs.BadConfig, "网盘客户端不可用:请重新保存 WebDAV 配置")
	}
	return a.store, nil
}

func (a *App) requireMgr() (*transfer.Manager, error) {
	if a.mgr == nil {
		return nil, errs.New(errs.Internal, "传输管线不可用:索引库打开失败,请重启应用")
	}
	return a.mgr, nil
}

func (a *App) requireUnlocked() error {
	if _, ok := a.mkSnapshot(); !ok {
		return errs.New(errs.Locked, "尚未解锁:请先输入口令解锁")
	}
	return nil
}

// loadKeyFileBytes 本地 keyfile 优先,没有则拉远端并缓存(CLI 同款);
// pulled 标记是否发生了远端拉取(新设备路径,UnlockResult 要上报)。
func (a *App) loadKeyFileBytes(store *remote.Store) (b []byte, pulled bool, err error) {
	if b, rerr := os.ReadFile(config.KeyFilePath()); rerr == nil {
		return b, false, nil
	}
	b, err = store.GetKeyFile(a.callCtx())
	if err != nil {
		return nil, false, errs.Wrap(errs.DavError, err)
	}
	if err := os.WriteFile(config.KeyFilePath(), b, 0o600); err != nil {
		return nil, false, err
	}
	return b, true, nil
}

// unlockKeyFile 是 Unlock / ImportFromRemote / ChangePassphrase 共用的前半程:
// 取 keyfile 字节 → 静态校验 → 口令解出 MK。
func (a *App) unlockKeyFile(store *remote.Store, pass string) (mk crypto.MasterKey, pulled bool, err error) {
	b, pulled, err := a.loadKeyFileBytes(store)
	if err != nil {
		return crypto.MasterKey{}, pulled, err
	}
	kf, err := crypto.ParseKeyFile(b)
	if err != nil {
		return crypto.MasterKey{}, pulled, errs.Wrap(errs.Corrupt, err)
	}
	mk, err = kf.Unlock(pass)
	if err != nil {
		return crypto.MasterKey{}, pulled, errs.From(err) // 错口令 → AUTH_FAILED
	}
	return mk, pulled, nil
}

// setUnlocked 置解锁态并刷新备份基线(revision 只在会话内追踪:
// sync_state.last_backup_at 存时间不存 revision,无 getter 可查)。
func (a *App) setUnlocked(mk crypto.MasterKey) {
	a.mu.Lock()
	a.mk = mk
	a.unlocked = true
	if a.db != nil {
		if rev, err := a.db.Revision(); err == nil {
			a.lastBackupRev = rev
		}
	}
	a.mu.Unlock()
}

// fileCount 根子树文件总数(GetAppState 用;空库/异常容错为 0)。
func (a *App) fileCount() int {
	if a.db == nil {
		return 0
	}
	s, err := a.db.FolderSummary(1)
	if err != nil {
		return 0
	}
	return s.FileCount
}

// ---- 事件接线 ----

// onTransferEvent 是管线的 Emit 回调:全部事件透传前端;index:changed 同时
// 驱动壳层防抖备份。App 既是转发器又是消费者,不经 EventsOn 自我订阅。
func (a *App) onTransferEvent(event string, payload any) {
	if event == "index:changed" {
		a.scheduleAutoBackup()
	}
	if a.ctx == nil {
		return // startup 前守卫(EventsEmit 需要 ctx)
	}
	wruntime.EventsEmit(a.ctx, event, payload)
}

// emitState 派发应用状态快照。
func (a *App) emitState() {
	if a.ctx == nil {
		return
	}
	wruntime.EventsEmit(a.ctx, "app:state", a.GetAppState())
}

// emitIndexChanged 通知前端刷新列表(壳层自身的写操作:移动/备注/元数据/恢复)。
func (a *App) emitIndexChanged(reason string) {
	a.onTransferEvent("index:changed", map[string]any{"reason": reason})
}

// emitNotify 通用提示(备份完成/失败、恢复结果等)。
func (a *App) emitNotify(level, text string) {
	if a.ctx == nil {
		return
	}
	wruntime.EventsEmit(a.ctx, "notify", map[string]string{"level": level, "text": text})
}

// ---- 防抖自动备份(壳层机制,phase-7 约定) ----

// scheduleAutoBackup 在收到 index:changed 后重置 30s 定时器;
// 仅 AutoBackup 开启且已解锁时生效。
func (a *App) scheduleAutoBackup() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cfg == nil || !a.cfg.Settings.AutoBackup || !a.unlocked {
		return
	}
	if a.backupTimer != nil {
		a.backupTimer.Stop()
	}
	a.backupTimer = time.AfterFunc(autoBackupDebounce, a.runAutoBackup)
}

// runAutoBackup 执行一次自动备份;正在备份时重排定时器(备份期间到达的变更不丢)。
// 用独立 Background 超时 ctx 而非 a.ctx:退出序列里 a.ctx 可能已取消而备份仍应完成。
func (a *App) runAutoBackup() {
	if !a.backingUp.CompareAndSwap(false, true) {
		a.scheduleAutoBackup()
		return
	}
	defer a.backingUp.Store(false)
	mk, ok := a.mkSnapshot()
	if !ok {
		return
	}
	store := a.storeSnapshot()
	if store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), backupTimeout)
	defer cancel()
	info, err := backup.BackupNow(ctx, mk, a.db, store)
	if err != nil {
		slog.Warn("自动备份失败", "err", err)
		a.emitNotify("error", "自动备份失败:"+err.Error())
		return
	}
	a.mu.Lock()
	a.lastBackupRev = info.Revision
	a.mu.Unlock()
	a.emitNotify("info", fmt.Sprintf("索引已自动备份(revision %d)", info.Revision))
}

// ---- 状态/配置/解锁 绑定 ----

// AppState 启动/解锁状态快照;HasLocalKeyfile 供 Lock 页分支:
// 已配置 + 无本地 keyfile = 新设备,显示"从远端恢复"入口。
// DriveName/DriveCount 供 Lock/设置页展示当前库所在盘(TODO-21)。
type AppState struct {
	Configured      bool
	Unlocked        bool
	FileCount       int
	HasLocalKeyfile bool
	DriveName       string
	DriveCount      int
}

// GetAppState 无错误返回:前端启动即拉,失败信息进不了 promise reject。
func (a *App) GetAppState() AppState {
	a.mu.Lock()
	cfg, unlocked := a.cfg, a.unlocked
	a.mu.Unlock()
	st := AppState{Unlocked: unlocked}
	if cfg != nil {
		if cfg.Configured() {
			st.Configured = true
		}
		if d := cfg.ActiveDrive(); d != nil {
			st.DriveName = d.Name
			st.DriveCount = len(cfg.Drives)
		}
	}
	if _, err := os.Stat(config.KeyFilePath()); err == nil {
		st.HasLocalKeyfile = true
	}
	st.FileCount = a.fileCount()
	return st
}

// WebDAVConfig 前端提交/回显的网盘配置(与口令体系无关的 WebDAV 账户凭据)。
type WebDAVConfig struct {
	URL              string
	Username         string
	Password         string
	RootPath         string
	RememberPassword bool
}

// GetWebDAVConfig 回显活动盘配置(密码仅"记住密码"时才有值,否则不猜)。
func (a *App) GetWebDAVConfig() WebDAVConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cfg == nil {
		return WebDAVConfig{RootPath: "/kist"}
	}
	c := WebDAVConfig{RootPath: "/kist"}
	if d := a.cfg.ActiveDrive(); d != nil {
		c = WebDAVConfig{
			URL:              d.URL,
			Username:         d.Username,
			RootPath:         d.RootPath,
			RememberPassword: d.RememberPassword,
		}
		if c.RootPath == "" {
			c.RootPath = "/kist"
		}
		if d.RememberPassword {
			c.Password = d.Password
		}
	}
	return c
}

// SaveWebDAVConfig 兼容入口(首配向导用):写活动盘,无盘则建第一盘。
// 名称留空由 normalize 兜底为 URL host;密码落盘与否走 SaveDrive 同一逻辑。
func (a *App) SaveWebDAVConfig(c WebDAVConfig) error {
	a.mu.Lock()
	id := ""
	if a.cfg != nil {
		if d := a.cfg.ActiveDrive(); d != nil {
			id = d.ID
		}
	}
	a.mu.Unlock()
	return a.SaveDrive(DriveInput{
		ID:               id,
		URL:              c.URL,
		Username:         c.Username,
		Password:         c.Password,
		RootPath:         c.RootPath,
		RememberPassword: c.RememberPassword,
	})
}

// DriveInfo 档案列表条目;密码仅"记住密码"时回显(编辑表单预填用)。
type DriveInfo struct {
	ID               string
	Name             string
	URL              string
	Username         string
	RootPath         string
	RememberPassword bool
	Password         string
	Active           bool
}

// ListDrives 列出全部网盘档案(设置页列表)。
func (a *App) ListDrives() []DriveInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cfg == nil {
		return []DriveInfo{}
	}
	out := make([]DriveInfo, 0, len(a.cfg.Drives))
	for _, d := range a.cfg.Drives {
		di := DriveInfo{
			ID:               d.ID,
			Name:             d.Name,
			URL:              d.URL,
			Username:         d.Username,
			RootPath:         d.RootPath,
			RememberPassword: d.RememberPassword,
			Active:           d.ID == a.cfg.Active,
		}
		if d.RememberPassword {
			di.Password = d.Password
		}
		out = append(out, di)
	}
	return out
}

// DriveInput 档案新增/编辑表单;ID 空 = 新增。
type DriveInput struct {
	ID               string
	Name             string
	URL              string
	Username         string
	Password         string
	RootPath         string
	RememberPassword bool
}

// SaveDrive 新增或编辑网盘档案。编辑活动盘时重建远端客户端并热更新管线端点;
// 库文件锚定盘 ID,编辑凭据/改名不动库。密码是否落盘由 RememberPassword 决定
// (config.Save 逐盘处理,未勾选时清空不落盘)。
func (a *App) SaveDrive(in DriveInput) (err error) {
	defer a.panicGuard(&err)
	if in.URL == "" || in.Username == "" {
		return a.wrap(errs.New(errs.BadConfig, "URL 与用户名均为必填"))
	}
	a.mu.Lock()
	cfg := a.cfg
	if cfg == nil {
		cfg = &config.StoredConfig{}
	}
	// 校验在工作副本上做,通过后一次落子——cfg 与 a.cfg 是同一对象,
	// 边改边查会让被拒绝的输入残留在活配置里(实测踩过:查重拒绝的
	// 档案已 append 进切片,后续查重被它误伤)
	work := *cfg
	work.Drives = append([]config.Drive(nil), cfg.Drives...)
	var d *config.Drive
	if in.ID == "" {
		work.Drives = append(work.Drives, config.Drive{ID: config.NewDriveID()})
		d = &work.Drives[len(work.Drives)-1]
	} else {
		d = work.DriveByID(in.ID)
		if d == nil {
			a.mu.Unlock()
			return a.wrap(errs.New(errs.BadConfig, "网盘档案不存在:可能已被删除,请刷新列表"))
		}
	}
	d.Name = in.Name
	d.URL, d.Username = in.URL, in.Username
	d.RootPath = in.RootPath // 空值由 config.Save 的 normalize 补 /kist
	d.Password = in.Password
	d.RememberPassword = in.RememberPassword
	// 查重必须在字段落位之后(比对的是新值,不是空壳)
	if dup := work.FindDuplicate(d); dup != nil {
		a.mu.Unlock()
		return a.wrap(errs.New(errs.BadConfig,
			fmt.Sprintf("与档案「%s」同地址/用户名/根目录——两个档案会指向同一个库,互相覆盖备份", dup.Name)))
	}
	if serr := config.Save(&work); serr != nil {
		a.mu.Unlock()
		return a.wrap(serr)
	}
	*cfg = work
	a.cfg = cfg // 首配时 a.cfg 原为 nil,这里统一认领
	wasActive := d.ID == cfg.Active
	firstVault := wasActive && a.db == nil // 首个档案:库还没开过
	if wasActive {
		if firstVault {
			a.reopenVaultLocked() // 开库 + 建管线(store 也在内)
		} else {
			a.rebuildStoreLocked()
		}
	}
	store := a.store
	a.mu.Unlock()
	// 管线热更新端点(mgr.SetRemote);在 a.mu 外调用——worker 的 Emit 回调会
	// 反向拿 a.mu,锁内互嵌会构成 ABBA。
	if wasActive && !firstVault && a.mgr != nil && store != nil {
		a.mgr.SetRemote(store)
	}
	a.emitState()
	return nil
}

// DeleteDrive 删除网盘档案(非活动、非最后一个)。本地索引文件保留——
// 上面可能是整库的明文索引,宁可落灰也不静默删。
func (a *App) DeleteDrive(id string) (err error) {
	defer a.panicGuard(&err)
	a.mu.Lock()
	cfg := a.cfg
	if cfg == nil || cfg.DriveByID(id) == nil {
		a.mu.Unlock()
		return a.wrap(errs.New(errs.BadConfig, "网盘档案不存在:可能已被删除,请刷新列表"))
	}
	if id == cfg.Active {
		a.mu.Unlock()
		return a.wrap(errs.New(errs.BadConfig, "不能删除当前网盘:请先切换到其他网盘"))
	}
	if len(cfg.Drives) <= 1 {
		a.mu.Unlock()
		return a.wrap(errs.New(errs.BadConfig, "至少保留一个网盘档案"))
	}
	name := cfg.DriveByID(id).Name
	kept := make([]config.Drive, 0, len(cfg.Drives)-1)
	for _, d := range cfg.Drives {
		if d.ID != id {
			kept = append(kept, d)
		}
	}
	cfg.Drives = kept
	if serr := config.Save(cfg); serr != nil {
		a.mu.Unlock()
		return a.wrap(serr)
	}
	a.cfg = cfg
	a.mu.Unlock()
	a.emitNotify("info", fmt.Sprintf("已删除档案「%s」(本地索引文件保留在 %s)", name, config.DriveIndexPath(id)))
	a.emitState()
	return nil
}

// SetActiveDrive 切换当前库(TODO-21 核心):要求管线空闲;切走前旧盘
// 尽力而为补一次备份;切换后保持解锁态(共用 keyfile,同一把 MK),
// 前端收到 drive:switched 回根目录。
func (a *App) SetActiveDrive(id string) (err error) {
	defer a.panicGuard(&err)
	a.mu.Lock()
	cfg := a.cfg
	if cfg == nil || cfg.DriveByID(id) == nil {
		a.mu.Unlock()
		return a.wrap(errs.New(errs.BadConfig, "网盘档案不存在:可能已被删除,请刷新列表"))
	}
	if id == cfg.Active {
		a.mu.Unlock()
		return nil // 幂等
	}
	if a.mgr == nil || !a.mgr.Idle() {
		a.mu.Unlock()
		return a.wrap(errs.New(errs.Busy, "有传输任务进行中:请等待完成或取消后再切换网盘"))
	}
	// 切走前补备份:快照后放锁执行(BackupNow 是秒级网络操作,不捂 mu)。
	// 字段直读——mkSnapshot 内部也拿 a.mu,持锁时调用会自锁。
	snapUnlocked := a.unlocked
	snapDB, snapStore := a.db, a.store
	snapMK := a.mk
	snapRev := a.lastBackupRev
	a.mu.Unlock()
	if snapUnlocked && snapDB != nil && snapStore != nil {
		if rev, rerr := snapDB.Revision(); rerr == nil && rev > snapRev {
			bctx, cancel := context.WithTimeout(context.Background(), backupTimeout)
			if _, berr := backup.BackupNow(bctx, snapMK, snapDB, snapStore); berr != nil {
				// 备份失败不拦切换:本地库文件还在,数据不丢,只是远端备份落后
				slog.Warn("切换前备份失败(继续切换)", "err", berr)
			}
			cancel()
		}
	}
	a.mu.Lock()
	cfg.Active = id // 切换本体:库文件与管线都锚定活动盘 ID
	if serr := config.Save(cfg); serr != nil {
		a.mu.Unlock()
		return a.wrap(serr)
	}
	a.cfg = cfg
	a.reopenVaultLocked() // 关旧库开新库 + 重建管线;保持解锁态(MK 共用)
	name := ""
	if d := cfg.ActiveDrive(); d != nil {
		name = d.Name
	}
	a.mu.Unlock()
	a.emitState()
	a.emitNotify("info", "已切换到网盘「"+name+"」")
	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, "drive:switched", map[string]string{"name": name})
	}
	return nil
}

// TestResult 连接测试结果;Detail 直接可读(成功/失败原因)。
type TestResult struct {
	Ok     bool
	Detail string
}

// TestConnection 用给定配置试连(不落盘、不建远端目录——EnsureReady 留给建账户)。
func (a *App) TestConnection(c WebDAVConfig) TestResult {
	if c.URL == "" || c.Username == "" {
		return TestResult{Ok: false, Detail: "URL 与用户名均为必填"}
	}
	cl, err := dav.New(dav.Config{URL: c.URL, Username: c.Username, Password: c.Password, RootPath: c.RootPath})
	if err != nil {
		return TestResult{Ok: false, Detail: err.Error()}
	}
	ctx, cancel := context.WithTimeout(a.callCtx(), 15*time.Second)
	defer cancel()
	if err := remote.NewStore(cl, c.RootPath).Ping(ctx); err != nil {
		return TestResult{Ok: false, Detail: err.Error()}
	}
	return TestResult{Ok: true, Detail: "连接正常"}
}

// CreateAccount 首次建账户:EnsureReady → 远端查重 → 生成 MK+keyfile →
// PUT 远端 → 本地缓存 → 置解锁态。本地已有 keyfile 时改为"开新库"分支
// (推现有 keyfile,沿用口令与 MK,见 TODO-21)。
// 顺序沿用 cmdInit:先远端后本地。
func (a *App) CreateAccount(passphrase string) (err error) {
	defer a.panicGuard(&err)
	store, err := a.requireStore()
	if err != nil {
		return a.wrap(err)
	}
	ctx := a.callCtx()
	if err := store.EnsureReady(ctx); err != nil {
		return a.wrap(errs.Wrap(errs.DavError, err))
	}
	exists, err := store.KeyFileExists(ctx)
	if err != nil {
		return a.wrap(errs.Wrap(errs.DavError, err))
	}
	if exists {
		return a.wrap(errs.New(errs.BadConfig,
			"远端已有 keyfile(此网盘目录已被初始化);请在解锁页解锁或从远端恢复"))
	}
	// 共用 keyfile(TODO-21):本地已有 keyfile = 在这块盘开新库——沿用现有
	// 密钥与口令,绝不生成新 MK(多库共用 keyfile 的前提),口令不符即拒绝。
	if local, lerr := os.ReadFile(config.KeyFilePath()); lerr == nil {
		kf, kerr := crypto.ParseKeyFile(local)
		if kerr != nil {
			return a.wrap(errs.Wrap(errs.Corrupt, kerr))
		}
		mk, uerr := kf.Unlock(passphrase)
		if uerr != nil {
			return a.wrap(errs.From(uerr)) // AUTH_FAILED:开新库必须沿用现有口令
		}
		if err := store.PutKeyFile(ctx, local); err != nil {
			return a.wrap(errs.Wrap(errs.DavError, err))
		}
		a.mu.Lock()
		if a.mgr != nil && !a.mgr.Idle() {
			a.mu.Unlock()
			return a.wrap(errs.New(errs.Busy, "有传输任务进行中:请等待完成或取消后再开新库"))
		}
		archived, aerr := a.archiveLocalIndexLocked()
		a.reopenVaultLocked() // 库文件已归档消失 → Open 得到全新空库
		a.mu.Unlock()
		a.setUnlocked(mk)
		if aerr != nil {
			slog.Warn("开新库:本地索引归档失败(沿用原文件)", "err", aerr)
		} else if archived != "" {
			a.emitNotify("info", "已在「"+a.cfg.ActiveDrive().Name+"」开新库;原本地索引归档于 "+archived)
		}
		a.emitState()
		return nil
	}
	// 首次建库(本地无 keyfile):生成新密钥
	kf, mk, err := crypto.CreateKeyFile(passphrase, crypto.DefaultArgon2Params())
	if err != nil {
		return a.wrap(errs.From(err))
	}
	if err := store.PutKeyFile(ctx, kf.Bytes()); err != nil {
		return a.wrap(errs.Wrap(errs.DavError, err))
	}
	if err := os.WriteFile(config.KeyFilePath(), kf.Bytes(), 0o600); err != nil {
		return a.wrap(err)
	}
	a.setUnlocked(mk) // 刚设完口令,直接进入解锁态
	a.emitState()
	return nil
}

// UnlockResult 解锁结果;SuggestPullIndex = 本地空库且远端有 index.enc
// (O(1) PROPFIND 探测,不下载),前端据此引导"从远端恢复"。
type UnlockResult struct {
	PulledRemoteKeyfile bool
	SuggestPullIndex    bool
	FileCount           int
}

// Unlock 口令解锁:本地 keyfile 优先,无则拉远端缓存(新设备路径)。
func (a *App) Unlock(passphrase string) (r UnlockResult, err error) {
	defer a.panicGuard(&err)
	store, err := a.requireStore()
	if err != nil {
		return r, a.wrap(err)
	}
	mk, pulled, err := a.unlockKeyFile(store, passphrase)
	if err != nil {
		return r, a.wrap(err)
	}
	a.setUnlocked(mk)
	r.PulledRemoteKeyfile = pulled
	r.FileCount = a.fileCount()
	if r.FileCount == 0 {
		if ok, _, perr := store.ProbeBlob(a.callCtx(), remote.IndexName); perr == nil && ok {
			r.SuggestPullIndex = true
		}
	}
	a.emitState()
	return r, nil
}

// ImportFromRemote 新设备恢复:keyfile 拉取 + 解锁 + 索引 LWW 拉取替换。
// PullResult.Action ∈ replaced|noop|local-newer,三分支文案由前端呈现。
func (a *App) ImportFromRemote(passphrase string) (r backup.PullResult, err error) {
	defer a.panicGuard(&err)
	store, err := a.requireStore()
	if err != nil {
		return r, a.wrap(err)
	}
	mk, _, err := a.unlockKeyFile(store, passphrase)
	if err != nil {
		return r, a.wrap(err)
	}
	a.setUnlocked(mk)
	r, err = backup.PullRemote(a.callCtx(), mk, store, a.db)
	if err != nil {
		return r, a.wrap(err)
	}
	a.setUnlocked(mk) // 库可能被 ReplaceWith 换掉,基线重设
	a.emitState()
	a.emitIndexChanged("pull")
	return r, nil
}

// Lock 锁定:MK 擦除清零,停防抖定时器;db/mgr 保留(Lock 页仍可看文件数)。
func (a *App) Lock() (err error) {
	defer a.panicGuard(&err)
	a.mu.Lock()
	a.mk.Wipe()
	a.mk = crypto.MasterKey{}
	a.unlocked = false
	if a.backupTimer != nil {
		a.backupTimer.Stop()
		a.backupTimer = nil
	}
	a.mu.Unlock()
	a.emitState()
	return nil
}

// ChangePassphrase 改口令:Rewrap 只重写 110 字节 keyfile,MK 不变无需重加密。
// 成功后本地与远端同步为新字节。
func (a *App) ChangePassphrase(oldPass, newPass string) (err error) {
	defer a.panicGuard(&err)
	store, err := a.requireStore()
	if err != nil {
		return a.wrap(err)
	}
	b, _, err := a.loadKeyFileBytes(store)
	if err != nil {
		return a.wrap(err)
	}
	kf, err := crypto.ParseKeyFile(b)
	if err != nil {
		return a.wrap(errs.Wrap(errs.Corrupt, err))
	}
	if err := kf.Rewrap(oldPass, newPass); err != nil {
		return a.wrap(errs.From(err)) // 旧口令错 → AUTH_FAILED
	}
	// 本地先行:本地 keyfile 是共用 keyfile 的母本
	if err := os.WriteFile(config.KeyFilePath(), kf.Bytes(), 0o600); err != nil {
		return a.wrap(err)
	}
	// 多盘扇出(TODO-21):向所有拿得到密码的盘推新 keyfile,尽力而为。
	// 未记密码/推失败的盘,远端 keyfile 停在旧口令——只影响该盘的新设备恢复
	// (需旧口令),本机不受影响(本地已换新,MK 相同)。
	a.mu.Lock()
	drives := append([]config.Drive(nil), a.cfg.Drives...)
	a.mu.Unlock()
	okCnt, skipCnt := 0, 0
	var failed []string
	for _, d := range drives {
		if !d.RememberPassword || d.Password == "" {
			skipCnt++
			continue
		}
		ctx, cancel := context.WithTimeout(a.callCtx(), backupTimeout)
		if st, cerr := buildStore(&d); cerr == nil {
			if perr := st.PutKeyFile(ctx, kf.Bytes()); perr == nil {
				okCnt++
				cancel()
				continue
			}
		}
		cancel()
		failed = append(failed, d.Name)
	}
	msg := fmt.Sprintf("口令已更改(旧口令即刻失效);keyfile 已同步 %d 块盘", okCnt)
	if skipCnt > 0 {
		msg += fmt.Sprintf(";%d 块未记密码的盘未同步(新设备恢复它们需旧口令)", skipCnt)
	}
	if len(failed) > 0 {
		msg += ";同步失败:" + strings.Join(failed, "、")
	}
	level := "info"
	if len(failed) > 0 {
		level = "error"
	}
	a.emitNotify(level, msg)
	return nil
}
