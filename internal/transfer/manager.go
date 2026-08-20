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
	PhaseDone        = "done"
	PhaseError       = "error"
	PhaseCanceled    = "canceled"
)

// Transfer 是一次上传/下载的对外快照(GUI 传输页与 CLI 汇总的数据源)。
type Transfer struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"` // upload | download
	Name       string `json:"name"` // 虚拟文件名
	UUID       string `json:"uuid"`
	Phase      string `json:"phase"`
	BytesDone  int64  `json:"bytesDone"`
	BytesTotal int64  `json:"bytesTotal"`
	StartedAt  int64  `json:"startedAt"`
	FinishedAt int64  `json:"finishedAt"`
	Err        string `json:"err,omitempty"`
}

// Deps 是管线的全部外部依赖;MK 返回 false 表示未解锁,任务以 LOCKED 失败。
type Deps struct {
	Remote      *remote.Store
	DB          *index.DB
	MK          func() (crypto.MasterKey, bool)
	Concurrency func() int // 0 → 2;夹取 1–4,调度器每轮重读(改设置即时生效)
	ChunkMiB    func() int // 0 → 4
	Emit        func(event string, payload any)
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
	// 下载
	file    index.FileRow
	destDir string
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

func tempRoot() string { return filepath.Join(os.TempDir(), "kist") }

// UploadPaths 展开文件/文件夹并入队,立即返回;返回入队文件数。
// 文件夹先在一次事务里建全部子目录(空目录也会保留——可接受的取舍),
// 再逐文件入队;符号链接一律跳过防环。
func (m *Manager) UploadPaths(ctx context.Context, paths []string, destFolderID int64) (int, error) {
	queued := 0
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return queued, fmt.Errorf("transfer: %s: %w", p, err)
		}
		if !st.IsDir() {
			m.enqueueUpload(ctx, p, st.Name(), destFolderID, st.ModTime().Unix())
			queued++
			continue
		}

		// 上传文件夹时,文件夹自身的名字构成第一级目录:
		// put ./资料 --dest /测试 → /测试/资料/...(与直觉一致)
		base := filepath.Base(p)

		// 第一遍:收集全部子目录(跳过符号链接目录)
		var dirs [][]string
		err = filepath.WalkDir(p, func(fp string, d fs.DirEntry, err error) error {
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
			if err != nil || rel == "." {
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
			m.enqueueUpload(ctx, fp, fi.Name(), folderID, fi.ModTime().Unix())
			queued++
			return nil
		})
		if err != nil {
			return queued, fmt.Errorf("transfer: 展开 %s: %w", p, err)
		}
	}
	return queued, nil
}

// DownloadTo 把索引中的文件下载解密到 destDir(不存在则创建);返回入队数。
func (m *Manager) DownloadTo(ctx context.Context, fileIDs []int64, destDir string) (int, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return 0, err
	}
	queued := 0
	for _, fid := range fileIDs {
		f, err := m.deps.DB.GetFile(fid)
		if err != nil {
			return queued, errs.From(err)
		}
		m.enqueueDownload(ctx, f, destDir)
		queued++
	}
	return queued, nil
}

func (m *Manager) enqueueUpload(ctx context.Context, srcPath, name string, folderID, mtime int64) {
	jctx, cancel := context.WithCancel(ctx)
	id := newHexID()
	j := &job{ctx: jctx, cancel: cancel, srcPath: srcPath, desiredName: name,
		folderID: folderID, mtime: mtime}
	j.tr = &Transfer{ID: id, Kind: "upload", Name: name, Phase: PhaseQueued,
		BytesTotal: 0, StartedAt: time.Now().Unix()}
	m.add(j)
}

func (m *Manager) enqueueDownload(ctx context.Context, f index.FileRow, destDir string) {
	jctx, cancel := context.WithCancel(ctx)
	id := newHexID()
	j := &job{ctx: jctx, cancel: cancel, file: f, destDir: destDir}
	j.tr = &Transfer{ID: id, Kind: "download", Name: f.Name, UUID: f.UUID, Phase: PhaseQueued,
		BytesTotal: f.Size + f.CipherSize, StartedAt: time.Now().Unix()}
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

func (m *Manager) worker(j *job) {
	slog.Info("传输开始", "kind", j.tr.Kind, "name", j.tr.Name)
	var err error
	if j.tr.Kind == "upload" {
		err = m.runUpload(j)
	} else {
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
	} else {
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
