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
		if a.cfg.URL != "" && a.cfg.Username != "" {
			a.rebuildStoreLocked()
		}
	}
	if db, err := index.Open(config.IndexPath()); err != nil {
		slog.Error("打开索引库失败", "err", err)
	} else {
		a.db = db
	}
	if a.db != nil {
		// MK 闭包读解锁态:未解锁返回 false,任务以 errs.Locked 失败(push 类纯密文
		// 任务不取 key,不受影响)。Concurrency/ChunkMiB/NoPad 每轮重读,改设置即时生效。
		a.mgr = transfer.NewManager(transfer.Deps{
			Remote:      a.storeSnapshot(),
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

// buildStore 由配置装配远端存储(loadStore 的核心步骤)。
func buildStore(cfg *config.StoredConfig) (*remote.Store, error) {
	c, err := dav.New(dav.Config{URL: cfg.URL, Username: cfg.Username, Password: cfg.Password, RootPath: cfg.RootPath})
	if err != nil {
		return nil, errs.Wrap(errs.BadConfig, err)
	}
	return remote.NewStore(c, cfg.RootPath), nil
}

// rebuildStoreLocked 按当前 cfg 重建 store;调用方须持 mu。
func (a *App) rebuildStoreLocked() {
	store, err := buildStore(a.cfg)
	if err != nil {
		slog.Error("远端客户端装配失败", "err", err)
		a.store = nil
		return
	}
	a.store = store
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
	if a.cfg == nil || a.cfg.URL == "" || a.cfg.Username == "" {
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
type AppState struct {
	Configured      bool
	Unlocked        bool
	FileCount       int
	HasLocalKeyfile bool
}

// GetAppState 无错误返回:前端启动即拉,失败信息进不了 promise reject。
func (a *App) GetAppState() AppState {
	a.mu.Lock()
	cfg, unlocked := a.cfg, a.unlocked
	a.mu.Unlock()
	st := AppState{Unlocked: unlocked}
	if cfg != nil && cfg.URL != "" && cfg.Username != "" {
		st.Configured = true
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

// GetWebDAVConfig 回显当前配置(密码仅"记住密码"时才有值,否则不猜)。
func (a *App) GetWebDAVConfig() WebDAVConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cfg == nil {
		return WebDAVConfig{RootPath: "/kist"}
	}
	c := WebDAVConfig{
		URL:              a.cfg.URL,
		Username:         a.cfg.Username,
		RootPath:         a.cfg.RootPath,
		RememberPassword: a.cfg.Settings.RememberPassword,
	}
	if c.RootPath == "" {
		c.RootPath = "/kist"
	}
	if a.cfg.Settings.RememberPassword {
		c.Password = a.cfg.Password
	}
	return c
}

// SaveWebDAVConfig 保存网盘配置并重建远端客户端。密码是否落盘由
// RememberPassword 决定(config.Save 统一处理,未勾选时清空不落盘)。
func (a *App) SaveWebDAVConfig(c WebDAVConfig) (err error) {
	defer a.panicGuard(&err)
	if c.URL == "" || c.Username == "" {
		return a.wrap(errs.New(errs.BadConfig, "URL 与用户名均为必填"))
	}
	a.mu.Lock()
	cfg := a.cfg
	if cfg == nil {
		cfg = &config.StoredConfig{}
	}
	cfg.URL, cfg.Username = c.URL, c.Username
	cfg.RootPath = c.RootPath // 空值由 config.Save 的 normalize 补 /kist
	cfg.Password = c.Password
	cfg.Settings.RememberPassword = c.RememberPassword
	if serr := config.Save(cfg); serr != nil {
		a.mu.Unlock()
		return a.wrap(serr)
	}
	a.cfg = cfg
	a.rebuildStoreLocked()
	store := a.store
	a.mu.Unlock()
	// 管线热更新端点(mgr.SetRemote);在 a.mu 外调用——worker 的 Emit 回调会
	// 反向拿 a.mu,锁内互嵌会构成 ABBA。向导首配时 mgr 的 Remote 还是 nil,
	// 也靠这里补上。
	if a.mgr != nil && store != nil {
		a.mgr.SetRemote(store)
	}
	a.emitState()
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

// CreateAccount 首次建账户(cmdInit 同款顺序:先远端后本地):
// EnsureReady → 远端查重 → 生成 MK+keyfile → PUT 远端 → 本地缓存 → 置解锁态。
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
			"远端已有 keyfile(此网盘目录已被初始化);如需重新开始请先手动清理远端目录"))
	}
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
	if err := store.PutKeyFile(a.callCtx(), kf.Bytes()); err != nil {
		return a.wrap(errs.Wrap(errs.DavError, err))
	}
	if err := os.WriteFile(config.KeyFilePath(), kf.Bytes(), 0o600); err != nil {
		return a.wrap(err)
	}
	a.emitNotify("info", "口令已更改(旧口令即刻失效)")
	return nil
}
