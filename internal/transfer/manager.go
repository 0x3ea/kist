package transfer

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"kist/internal/crypto"
	"kist/internal/errs"
	"kist/internal/index"
	"kist/internal/remote"
)

// 传输阶段常量(GUI 前端按此渲染状态)。
const (
	PhaseQueued      = "queued"
	PhaseEncrypting  = "encrypting"
	PhaseUploading   = "uploading"
	PhaseDownloading = "downloading"
	PhaseDecrypting  = "decrypting"
	PhaseDeferred    = "deferred" // put --defer 成功:记账完成、产物入出站箱,运输未发生(TODO-13)
	PhaseDone        = "done"
	PhaseError       = "error"
	PhaseCanceled    = "canceled"
)

// Transfer 是一次上传/下载的对外快照(GUI 传输页与 CLI 汇总的数据源)。
type Transfer struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"` // upload | download | push
	Name       string `json:"name"` // 虚拟文件名
	UUID       string `json:"uuid"`
	Phase      string `json:"phase"`
	BytesDone  int64  `json:"bytesDone"`
	BytesTotal int64  `json:"bytesTotal"`
	StartedAt  int64  `json:"startedAt"`
	FinishedAt int64  `json:"finishedAt"`
	Err        string `json:"err,omitempty"`
}

// Deps 是管线的全部外部依赖;MK 返回 false 表示未解锁,任务以 LOCKED 失败
// (push 是纯密文搬运,不解密,MK 可恒为 false)。
type Deps struct {
	Remote      *remote.Store
	DB          *index.DB
	MK          func() (crypto.MasterKey, bool)
	Concurrency func() int // 0 → 2;夹取 1–4,调度器每轮重读(改设置即时生效)
	ChunkMiB    func() int // 0 → 4
	Emit        func(event string, payload any)
	// PushFailDiscard 报告 outbox push 失败政策:keep(默认/nil)= 挂账留证,
	// discard = 失败对象删索引行与产物(TODO-13 两档政策)
	PushFailDiscard func() bool
	// NoPad 报告是否关闭大小量化(TODO-08):nil/false = 默认 v2 档位填充
	NoPad func() bool
}

type job struct {
	ctx    context.Context
	cancel context.CancelFunc
	tr     *Transfer
	// 上传
	srcPath     string
	desiredName string
	folderID    int64
	mtime       int64
	deferred    bool // 上传任务止于"记账+产物入出站箱",不发 PUT(TODO-13)
	pack        bool // 目录打包任务(TODO-15):源是目录,zip 流直挂 BlobWriter
	// 下载/push
	file        index.FileRow
	cover       *index.CoverRow       // push 任务的封面账(TODO-10):非 nil 时按封面账处理
	folderCover *index.FolderCoverRow // push 任务的目录封面账(v6):非 nil 时按封面账处理
	destDir     string
	keepZip     bool // pack 下载不解压,落 <名>.zip(TODO-15)
}

// Manager 是传输管线:动态并发上限的单调度器 + 每任务独立 ctx(可取消)。
type Manager struct {
	deps     Deps
	mu       sync.Mutex
	cond     *sync.Cond
	m        map[string]*Transfer
	order    []string
	pending  []*job
	running  int
	cancels  map[string]context.CancelFunc
	lastEmit map[string]time.Time
}

// NewManager 构造管线。单实例假设:构造时清空 /tmp/kist 下上次进程的残留临时文件。
func NewManager(d Deps) *Manager {
	if d.Concurrency == nil {
		d.Concurrency = func() int { return 2 }
	}
	if d.ChunkMiB == nil {
		d.ChunkMiB = func() int { return 4 }
	}
	m := &Manager{
		deps:     d,
		m:        map[string]*Transfer{},
		cancels:  map[string]context.CancelFunc{},
		lastEmit: map[string]time.Time{},
	}
	m.cond = sync.NewCond(&m.mu)
	if err := os.RemoveAll(tempRoot()); err != nil {
		// 上次残留清不掉只占磁盘、不损正确性:留痕不阻断(TODO-07 静默黑洞)
		slog.Warn("清理上次残留临时目录失败", "dir", tempRoot(), "err", err)
	} else if err := os.MkdirAll(tempRoot(), 0o700); err != nil {
		slog.Warn("重建临时目录失败", "dir", tempRoot(), "err", err)
	}
	go m.dispatch()
	return m
}

// tempRoot 传输临时根。KIST_TMPDIR 可覆盖:多实例(GUI+CLI)并行时默认共用
// /tmp/kist 会互相清理对方产物(CLAUDE.md 记录的坑),需要隔离的场合各自指定;
// 测试也用它避免并行包之间对 /tmp/kist 的争用(构造 NewManager 即清场)。
func tempRoot() string {
	if d := os.Getenv("KIST_TMPDIR"); d != "" {
		return d
	}
	return filepath.Join(os.TempDir(), "kist")
}

// SetRemote 替换远端存储(GUI 保存新 WebDAV 配置后热更新):之后发起的网络
// 调用走新端点,在途任务持有旧 client 自行收尾。Deps 其余项都是闭包动态读,
// Remote 是唯一需要显式替换的依赖。nil 不接受——置空意味着失能,调用方不该这么做。
func (m *Manager) SetRemote(s *remote.Store) {
	if s == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deps.Remote = s
}

// remoteSnapshot 在锁下取远端存储:worker 任务期持有快照,避免与 SetRemote 的
// 并发替换构成 data race(-race 可检出)。
func (m *Manager) remoteSnapshot() *remote.Store {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deps.Remote
}

// UploadOptions 是上传的可选项(TODO-15)。
type UploadOptions struct {
	// Expand 保留逐文件展开的旧行为(每文件一 blob,文件夹镜像为虚拟
	// 目录);默认按打包粒度解析:put 根为叶子目录时整根一个 pack,
	// 含子目录时逐叶子成 pack,混杂层散文件按普通文件入库。
	Expand bool
}

// UploadPaths 展开文件/文件夹并入队,立即返回;返回入队任务数。
func (m *Manager) UploadPaths(ctx context.Context, paths []string, destFolderID int64, opts ...UploadOptions) (int, error) {
	return m.uploadPaths(ctx, paths, destFolderID, false, expandOpt(opts))
}

// DeferPaths 与 UploadPaths 同构,但止于"加密 + 记账(uploading)+ 产物入
// 出站箱",不发起 PUT(TODO-13:运输与记账分离,上传交给 push/手工搬运)。
func (m *Manager) DeferPaths(ctx context.Context, paths []string, destFolderID int64, opts ...UploadOptions) (int, error) {
	return m.uploadPaths(ctx, paths, destFolderID, true, expandOpt(opts))
}

func expandOpt(opts []UploadOptions) bool {
	for _, o := range opts {
		if o.Expand {
			return true
		}
	}
	return false
}

func (m *Manager) uploadPaths(ctx context.Context, paths []string, destFolderID int64, deferred, expand bool) (int, error) {
	queued := 0
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return queued, fmt.Errorf("transfer: %s: %w", p, err)
		}
		if !st.IsDir() {
			m.enqueueUploadSpec(ctx, uploadSpec{src: p, name: st.Name(), folder: destFolderID,
				mtime: st.ModTime().Unix()}, deferred)
			queued++
			continue
		}
		var n int
		if expand {
			n, err = m.expandFolder(ctx, p, destFolderID, deferred)
		} else {
			n, err = m.packFolder(ctx, p, destFolderID, deferred)
		}
		queued += n
		if err != nil {
			return queued, err
		}
	}
	return queued, nil
}

// expandFolder 是 --expand 的逐文件展开(旧行为):每文件一 blob,文件夹
// 镜像为虚拟目录;符号链接一律跳过防环(与打包模式的整次拒绝不同)。
func (m *Manager) expandFolder(ctx context.Context, p string, destFolderID int64, deferred bool) (int, error) {
	queued := 0
	// 上传文件夹时,文件夹自身的名字构成第一级目录:
	// put ./资料 --dest /测试 → /测试/资料/...(与直觉一致)
	base := filepath.Base(p)

	// 第一遍:收集全部子目录(跳过符号链接目录)
	var dirs [][]string
	err := filepath.WalkDir(p, func(fp string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fs.SkipDir
		}
		rel, err := filepath.Rel(p, fp)
		if err != nil {
			return err
		}
		if rel == "." {
			// 根目录自身也要入清单:叶子目录(无子目录)时没有更深的目录
			// 会连带建出它,漏掉会让根下文件拿到零值 folderID 撞外键
			// (文件夹下载的 Mixed e2e 首次暴露,此前用例的展开目标都有子目录)
			dirs = append(dirs, []string{base})
			return nil
		}
		dirs = append(dirs, append([]string{base}, splitSegments(rel)...))
		return nil
	})
	if err != nil {
		return queued, fmt.Errorf("transfer: 展开 %s: %w", p, err)
	}

	// 一次性建目录结构;逐级记录 id——文件夹根下的文件需要第一级的 id,
	// 只记最深一级会让它们拿到零值 folderID 而触发外键失败
	folderIDs := map[string]int64{}
	err = m.deps.DB.WithTx(func(tx *sql.Tx) error {
		for _, segs := range dirs {
			for i := 1; i <= len(segs); i++ {
				id, err := m.deps.DB.EnsureFolderPath(tx, destFolderID, segs[:i])
				if err != nil {
					return err
				}
				folderIDs[joinSegments(segs[:i])] = id
			}
		}
		return nil
	})
	if err != nil {
		return queued, err
	}

	// 第二遍:文件入队
	err = filepath.WalkDir(p, func(fp string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(p, fp)
		if err != nil {
			return err
		}
		folderID := folderIDs[base] // 文件夹根下的文件
		if dir := filepath.Dir(rel); dir != "." {
			folderID = folderIDs[base+"/"+filepath.ToSlash(dir)]
		}
		m.enqueueUploadSpec(ctx, uploadSpec{src: fp, name: fi.Name(), folder: folderID,
			mtime: fi.ModTime().Unix()}, deferred)
		queued++
		return nil
	})
	if err != nil {
		return queued, fmt.Errorf("transfer: 展开 %s: %w", p, err)
	}
	return queued, nil
}

// packFolder 按 TODO-15 打包粒度上传文件夹:先校验(非 UTF-8 名、symlink/
// 特殊文件使整次 put 拒绝),再规划叶子 pack/散文件/虚拟目录,一次事务
// 建目录后逐任务入队。返回入队数。
func (m *Manager) packFolder(ctx context.Context, p string, destFolderID int64, deferred bool) (int, error) {
	plan, err := planPackTree(p)
	if err != nil {
		return 0, err
	}
	base := filepath.Base(p)
	folderIDs := map[string]int64{}
	// 整根成 pack 且无虚拟目录时没有任何目录要建,不开事务——空 WithTx
	// 也会 +1 revision,凭空逼出一次备份(TODO-10 验收"revision 不虚高")
	if !plan.RootIsPack || len(plan.VirtualDirs) > 0 {
		err = m.deps.DB.WithTx(func(tx *sql.Tx) error {
			if !plan.RootIsPack {
				// 根名构成第一级目录(整根成 pack 时不需要)
				id, err := m.deps.DB.EnsureFolderPath(tx, destFolderID, []string{base})
				if err != nil {
					return err
				}
				folderIDs[base] = id
			}
			for _, segs := range plan.VirtualDirs {
				full := append([]string{base}, segs...)
				id, err := m.deps.DB.EnsureFolderPath(tx, destFolderID, full)
				if err != nil {
					return err
				}
				folderIDs[joinSegments(full)] = id
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	folderOf := func(parent []string) int64 {
		if len(parent) == 0 {
			return folderIDs[base]
		}
		return folderIDs[joinSegments(append([]string{base}, parent...))]
	}
	queued := 0
	if rp := plan.RootPack; rp != nil {
		m.enqueueUploadSpec(ctx, uploadSpec{src: rp.AbsDir, name: rp.Name, folder: destFolderID,
			mtime: rp.Mtime, pack: true}, deferred)
		queued++
	}
	for _, pr := range plan.Packs {
		m.enqueueUploadSpec(ctx, uploadSpec{src: pr.AbsDir, name: pr.Name,
			folder: folderOf(parentSegs(pr.RelSegs)), mtime: pr.Mtime, pack: true}, deferred)
		queued++
	}
	for _, lf := range plan.Loose {
		m.enqueueUploadSpec(ctx, uploadSpec{src: lf.AbsPath, name: lf.Name,
			folder: folderOf(parentSegs(lf.RelSegs)), mtime: lf.Mtime}, deferred)
		queued++
	}
	return queued, nil
}

// DownloadOptions 是下载的可选项(TODO-15)。
type DownloadOptions struct {
	// KeepZip:pack 条目不解压还原成文件夹,直接落 <名>.zip
	// (喂漫画阅读器可自行改名 .cbz)。
	KeepZip bool
}

// DownloadTo 把索引中的文件下载解密到 destDir(不存在则创建);返回入队数。
// pack 条目默认解压还原成文件夹(临时目录整体 rename 保原子落盘)。
// DownloadEntriesTo 的文件 only 薄包装(整目录下载见它)。
func (m *Manager) DownloadTo(ctx context.Context, fileIDs []int64, destDir string, opts ...DownloadOptions) (int, error) {
	queued, _, err := m.DownloadEntriesTo(ctx, fileIDs, nil, destDir, opts...)
	return queued, err
}

// DownloadEntriesTo 把文件与文件夹一起下载解密(与 MoveEntries 同构的
// "文件与目录同收"入口):文件平铺落 destDir;每个文件夹包一层 "<目录名>/"
// 并按虚拟结构重建子树——嵌套目录与空目录都建,pack 条目仍走解压还原。
//
// uploading 的两种待遇是刻意的:显式点名的 fileIDs 照常入队,由运行时守卫
// 给出"先 outbox push"的指引性拒绝(TODO-13 语义,outbox e2e 固化);子树内
// 扫出来的 uploading 则规划期跳过并计入 skipped、其所在目录照建——批量里
// 跳过给明确统计比逐个失败有用,且该条目在浏览界面本就不可见。
//
// 顺序铁律:全部规划 → 全部 mkdir → 全部入队——worker 在 add() 后即被
// 并发消费,目录必须先于任何任务存在:空目录没有任务替它建;pack 条目的
// 运行时 uniqueLocalName 消解也依赖先建目录才"看得见"同名的虚拟目录
// (索引允许文件与目录同名,后建会让 rename 撞车)。
// 校验/规划遍先收齐全部问题再统一落盘(planFolderTree/MoveEntries 同哲学)。
func (m *Manager) DownloadEntriesTo(ctx context.Context, fileIDs, folderIDs []int64, destDir string, opts ...DownloadOptions) (queued, skipped int, err error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return 0, 0, err
	}
	keepZip := false
	for _, o := range opts {
		if o.KeepZip {
			keepZip = true
		}
	}

	var specs []downloadSpec
	var mkdirs []string
	taken := map[string]bool{} // 同批根目录占位:尚未落盘,文件系统看不见
	for _, fid := range fileIDs {
		f, err := m.deps.DB.GetFile(fid)
		if err != nil {
			return 0, skipped, errs.From(err)
		}
		specs = append(specs, downloadSpec{file: f, destDir: destDir, name: f.Name})
	}
	for _, folderID := range folderIDs {
		ft, err := m.planFolderTree(folderID, destDir, taken)
		if err != nil {
			return 0, skipped, err
		}
		skipped += ft.skipped
		mkdirs = append(mkdirs, ft.rootLocal)
		for _, d := range ft.dirs {
			mkdirs = append(mkdirs, filepath.Join(ft.rootLocal, d))
		}
		specs = append(specs, ft.specs...)
	}
	for _, d := range mkdirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return 0, skipped, err
		}
	}
	for _, s := range specs {
		m.enqueueDownloadSpec(ctx, s, keepZip)
		queued++
	}
	return queued, skipped, nil
}

// uploadSpec 是一次上传任务的入队描述(普通文件与 pack 共用)。
type uploadSpec struct {
	src    string
	name   string
	folder int64
	mtime  int64
	pack   bool
}

func (m *Manager) enqueueUploadSpec(ctx context.Context, s uploadSpec, deferred bool) {
	jctx, cancel := context.WithCancel(ctx)
	id := newHexID()
	j := &job{ctx: jctx, cancel: cancel, srcPath: s.src, desiredName: s.name,
		folderID: s.folder, mtime: s.mtime, deferred: deferred, pack: s.pack}
	j.tr = &Transfer{ID: id, Kind: "upload", Name: s.name, Phase: PhaseQueued,
		BytesTotal: 0, StartedAt: time.Now().Unix()}
	m.add(j)
}

func (m *Manager) add(j *job) {
	m.mu.Lock()
	m.m[j.tr.ID] = j.tr
	m.order = append(m.order, j.tr.ID)
	m.cancels[j.tr.ID] = j.cancel
	m.pending = append(m.pending, j)
	m.mu.Unlock()
	m.cond.Broadcast()
	m.emitSnapshot(j.tr) // 触发 transfers:changed 语义,由上层订阅
	m.emit("transfers:changed", m.Snapshot())
}

// dispatch 单调度循环:每轮在并发上限内尽量派发,然后等新任务或完成通知。
func (m *Manager) dispatch() {
	m.mu.Lock()
	for {
		limit := m.limit()
		for m.running < limit && len(m.pending) > 0 {
			j := m.pending[0]
			m.pending = m.pending[1:]
			m.running++
			go m.worker(j)
		}
		m.cond.Wait() // add 与 worker 完成都会 Broadcast 唤醒
	}
}

func (m *Manager) limit() int {
	n := m.deps.Concurrency()
	if n < 1 {
		n = 1
	}
	if n > 4 {
		n = 4
	}
	return n
}

func (m *Manager) chunkBytes() uint32 {
	mib := m.deps.ChunkMiB()
	if mib <= 0 {
		mib = 4
	}
	return uint32(mib) << 20
}

// noPad 上传加密是否关闭大小量化(默认开,TODO-08)。
func (m *Manager) noPad() bool {
	return m.deps.NoPad != nil && m.deps.NoPad()
}

func (m *Manager) worker(j *job) {
	slog.Info("传输开始", "kind", j.tr.Kind, "name", j.tr.Name)
	var err error
	switch j.tr.Kind {
	case "upload":
		err = m.runUpload(j)
	case "push":
		err = m.runPush(j)
	default:
		err = m.runDownload(j)
	}
	m.mu.Lock()
	m.running--
	tr := m.m[j.tr.ID]
	if err != nil {
		if j.ctx.Err() != nil {
			tr.Phase = PhaseCanceled
		} else {
			tr.Phase = PhaseError
			tr.Err = errs.From(err).Error()
		}
	} else if tr.Phase != PhaseDeferred {
		tr.Phase = PhaseDone
	}
	tr.FinishedAt = time.Now().Unix()
	m.cond.Broadcast()
	m.mu.Unlock()
	m.emitSnapshot(tr)
	// 传输起止与结果留痕(TODO-07):起止/完成记 Info,失败记 Error;
	// 取消是用户主动行为,记 Info 即可
	switch tr.Phase {
	case PhaseDone:
		slog.Info("传输完成", "kind", tr.Kind, "name", tr.Name)
		m.emit("transfer:done", *tr)
	case PhaseDeferred:
		slog.Info("已入出站箱待传", "kind", tr.Kind, "name", tr.Name)
	case PhaseCanceled:
		slog.Info("传输已取消", "kind", tr.Kind, "name", tr.Name)
	case PhaseError:
		slog.Error("传输失败", "kind", tr.Kind, "name", tr.Name, "err", tr.Err)
		m.emit("transfer:error", *tr)
	}
}

// Cancel 取消一个传输;已结束的任务返回 false。
func (m *Manager) Cancel(id string) bool {
	m.mu.Lock()
	cancel := m.cancels[id]
	m.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

// Snapshot 返回按入队顺序的全部传输快照。
func (m *Manager) Snapshot() []Transfer {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Transfer, 0, len(m.order))
	for _, id := range m.order {
		out = append(out, *m.m[id])
	}
	return out
}

// Idle 报告是否没有排队或运行中的任务(CLI/e2e 等待收尾用)。
func (m *Manager) Idle() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running == 0 && len(m.pending) == 0
}

// setPhase 更新阶段;阶段变化总是外发事件。
func (m *Manager) setPhase(tr *Transfer, phase string) {
	m.mu.Lock()
	if tr.Phase == phase {
		m.mu.Unlock()
		return
	}
	tr.Phase = phase
	m.mu.Unlock()
	m.emitSnapshot(tr)
}

// setProgress 更新进度;按 200ms 节流外发。
func (m *Manager) setProgress(tr *Transfer, done int64) {
	m.mu.Lock()
	tr.BytesDone = done
	should := time.Since(m.lastEmit[tr.ID]) > 200*time.Millisecond
	if should {
		m.lastEmit[tr.ID] = time.Now()
	}
	m.mu.Unlock()
	if should {
		m.emitSnapshot(tr)
	}
}

func (m *Manager) setTotal(tr *Transfer, total int64) {
	m.mu.Lock()
	tr.BytesTotal = total
	m.mu.Unlock()
}

func (m *Manager) emitSnapshot(tr *Transfer) {
	m.mu.Lock()
	cp := *tr
	m.mu.Unlock()
	m.emit("transfer:update", cp)
}

func (m *Manager) emit(event string, payload any) {
	if m.deps.Emit != nil {
		m.deps.Emit(event, payload)
	}
}

// newHexID 生成 16 随机字节的 hex:既当传输 ID,也当 blob 对象名。
func newHexID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("transfer: 系统随机源不可用: " + err.Error())
	}
	return fmt.Sprintf("%x", b[:])
}

// splitSegments 把相对路径拆成目录段(过滤空段,兼容尾斜杠)。
func splitSegments(rel string) []string {
	var segs []string
	for _, s := range strings.Split(filepath.ToSlash(rel), "/") {
		if s != "" && s != "." {
			segs = append(segs, s)
		}
	}
	return segs
}

func joinSegments(segs []string) string { return strings.Join(segs, "/") }
