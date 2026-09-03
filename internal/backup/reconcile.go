package backup

// reconcile.go — 启动对账(TODO-22):把 TODO-09 的三方比较从"写操作前"
// 提前到"解锁后"。对账不发明新决策逻辑,四个局面全部委托现有原语
// (BackupNow/PullRemote 内部重跑检测,幂等且防竞态——预检通过后对端仍可能
// 恰在此刻推送,只有原语自己的检测能拦住)。
//
// 设计决策(评估文档 docs/todo/22):
//   - 本地领先"补推"而非"回滚":本地≠云端有两种成因(对端推了/本地没推出
//     去),后者一律回滚会把离线工作从视图抹掉、离线删除复活
//   - 分叉保留人工裁决,diff 只是信息;裁决前必须重检(EnsureRemoteRev)

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"kist/internal/crypto"
	"kist/internal/index"
	"kist/internal/remote"
)

// 对账的四种结论(ReconcileResult.Action)。
const (
	ReconcileNoop     = "noop"     // 双方在基线后都没动
	ReconcilePushed   = "pushed"   // 只有本地动过:补推成功(云端被本地快进)
	ReconcilePulled   = "pulled"   // 只有远端动过:快进拉取成功(本地被云端快进)
	ReconcileConflict = "conflict" // 双方都动过:分叉,交人裁决
)

// ConflictDetail 的 Kind 额外取值(对话框只在分叉/远端领先时打开,
// 后两个用于 stale 刷新后局面已变的兜底展示)。
const (
	KindLocalAhead = "local-ahead" // 只有本地动过
	KindInSync     = "in-sync"     // 双方都没动(冲突已被解决)
)

// ReconcileResult 是一次启动对账的结论;Pushed/Pulled/Conflict 各带对应明细。
type ReconcileResult struct {
	Action   string
	Pull     *PullResult // Action=pulled 时非 nil
	Backup   *BackupInfo // Action=pushed 时非 nil
	Conflict *Conflict   // Action=conflict 时非 nil
}

// Reconcile 解锁后对账:探测远端头部 → 三方分类 → 委托原语执行。
//
//	双方未动            noop(一次探测的代价,零写流量)
//	只有本地动过        补推(BackupNow,远端未动过即安全快进)
//	只有远端动过        快进拉(PullRemote,本地未动过即零丢失)
//	双方都动过          返回分叉 Conflict,不执行任何动作
//
// 探测本身的错误(离线/WrongKey/CorruptBlob)原样上抛,调用方降级提示即可:
// 每次 push/pull 自带同款检测,正确性不依赖对账,失败无害可跳过。
func Reconcile(ctx context.Context, mk crypto.MasterKey, db *index.DB, s *remote.Store) (ReconcileResult, error) {
	rev, err := db.Revision()
	if err != nil {
		return ReconcileResult{}, err
	}
	baseline, err := db.LastSyncedRev()
	if err != nil {
		return ReconcileResult{}, err
	}
	meta, existent, err := remoteIndexMeta(ctx, mk, s)
	if err != nil {
		return ReconcileResult{}, err
	}
	localMoved := rev != baseline
	remoteMoved := existent && meta.Revision != baseline
	// 分支必须显式排除另一侧:case localMoved 会把"双方都动过"截走
	switch {
	case !localMoved && !remoteMoved:
		slog.Info("启动对账:本机与远端一致", "revision", rev)
		return ReconcileResult{Action: ReconcileNoop}, nil
	case localMoved && !remoteMoved:
		// 补推而非回滚(TODO-22 设计决策一):离线/崩溃留下的未推送工作
		// 先入账。force=false:预检与执行之间若对端恰好推送,原语会拦下转分叉。
		info, err := BackupNow(ctx, mk, db, s, false)
		if err != nil {
			return ReconcileResult{}, err
		}
		slog.Info("启动对账:已补推本地未同步改动", "revision", info.Revision, "baseline", baseline)
		return ReconcileResult{Action: ReconcilePushed, Backup: &info}, nil
	case !localMoved && remoteMoved:
		// 快进拉取:本机干净,ReplaceWith 零丢失(不碰远端 blob)
		r, err := PullRemote(ctx, mk, s, db, false)
		if err != nil {
			return ReconcileResult{}, err
		}
		slog.Info("启动对账:已快进到远端版本", "local", rev, "remote", r.RemoteRev, "baseline", baseline)
		return ReconcileResult{Action: ReconcilePulled, Pull: &r}, nil
	default:
		c := &Conflict{
			Kind:         KindDiverged,
			LocalRev:     rev,
			RemoteRev:    meta.Revision,
			BaselineRev:  baseline,
			RemoteDevice: hex.EncodeToString(meta.DeviceID[:]),
		}
		slog.Warn("启动对账检出分叉,待人工裁决", "local", rev, "remote", c.RemoteRev, "baseline", baseline)
		return ReconcileResult{Action: ReconcileConflict, Conflict: c}, nil
	}
}

// StaleError 是裁决前重检(EnsureRemoteRev)检出的"远端已变化"。
// 启动对账把冲突弹窗的存活窗口从秒级拉到分钟级,期间对端可能又推一版;
// force 动作跳过一切检测,不核对就会静默覆盖对端的新推送。
type StaleError struct {
	Expected uint64 // 弹窗打开时看到的远端 revision
	Current  uint64 // 裁决时重新探测到的远端 revision(0=远端已无索引)
}

func (e *StaleError) Error() string {
	return fmt.Sprintf("远端已变化:裁决依据是 revision %d,当前为 %d,请刷新差异后重新裁决", e.Expected, e.Current)
}

// EnsureRemoteRev 校验远端头部仍是 expect;不是则返回 *StaleError,
// 网络类错误原样上抛。keep-local(覆盖远端)与 keep-remote(采纳远端)
// 都应在 force 动作前调用。
func EnsureRemoteRev(ctx context.Context, mk crypto.MasterKey, s *remote.Store, expect uint64) error {
	meta, existent, err := remoteIndexMeta(ctx, mk, s)
	if err != nil {
		return err
	}
	cur := uint64(0)
	if existent {
		cur = meta.Revision
	}
	if cur != expect {
		return &StaleError{Expected: expect, Current: cur}
	}
	return nil
}

// ConflictDetail 是冲突对话框的按需详情(TODO-22):三方局面快照 + 文件级
// diff 三栏。按需拉取而非冲突发生时预计算——push 时被拦下的冲突没有现成
// diff,启动对账检出的冲突在用户点开时可能已 stale,一个入口通吃两种检出
// 路径,也天然服务"stale 后刷新"。
type ConflictDetail struct {
	Kind         string
	LocalRev     uint64
	RemoteRev    uint64
	BaselineRev  uint64
	RemoteDevice string
	// Diff 恒有序列化(远端没动时是零值:三栏 Total=0)。不要挂 json tag——
	// 空名 tag(如 ",omitempty")会被 wails 绑定生成器整字段丢弃,前端类型
	// 缺 Diff 直接编译失败(实测踩过)。
	Diff DiffResult
}

// FetchConflictDetail 整拉远端索引(123pan 无 Range,一次下载 meta 与正文
// 两用),重算当前三方局面;远端动过时解密快照与本地清单做文件级比对。
func FetchConflictDetail(ctx context.Context, mk crypto.MasterKey, db *index.DB, s *remote.Store) (ConflictDetail, error) {
	ok, _, err := s.ProbeBlob(ctx, remote.IndexName)
	if err != nil {
		return ConflictDetail{}, err
	}
	if !ok {
		return ConflictDetail{}, fmt.Errorf("远端还没有索引备份,没有可对比的差异")
	}
	dir, err := os.MkdirTemp("", "kist-detail-*")
	if err != nil {
		return ConflictDetail{}, err
	}
	defer os.RemoveAll(dir)
	enc := filepath.Join(dir, "index.enc")
	if err := s.GetIndexBlob(ctx, enc); err != nil {
		return ConflictDetail{}, fmt.Errorf("拉取远端索引失败: %w", err)
	}
	f, err := os.Open(enc)
	if err != nil {
		return ConflictDetail{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ConflictDetail{}, err
	}
	br, err := crypto.NewBlobReader(f, st.Size(), mk)
	if err != nil {
		return ConflictDetail{}, err // ErrWrongKey(指向别人的账户)或 ErrCorruptBlob
	}
	meta := br.Meta()
	localRev, err := db.Revision()
	if err != nil {
		return ConflictDetail{}, err
	}
	baseline, err := db.LastSyncedRev()
	if err != nil {
		return ConflictDetail{}, err
	}
	d := ConflictDetail{
		LocalRev:     localRev,
		RemoteRev:    meta.Revision,
		BaselineRev:  baseline,
		RemoteDevice: hex.EncodeToString(meta.DeviceID[:]),
	}
	localMoved, remoteMoved := localRev != baseline, meta.Revision != baseline
	switch {
	case localMoved && remoteMoved:
		d.Kind = KindDiverged
	case remoteMoved:
		d.Kind = KindRemoteAhead
	case localMoved:
		d.Kind = KindLocalAhead
	default:
		d.Kind = KindInSync
	}
	if !remoteMoved {
		return d, nil // 远端没动,没有"另一侧清单"可比
	}
	// 完整解密快照(块认证 + 明文终检),认证不过即中止,不产出半截清单
	plain := filepath.Join(dir, "snap.db")
	out, err := os.Create(plain)
	if err != nil {
		return ConflictDetail{}, err
	}
	if _, err := io.Copy(out, br); err != nil {
		out.Close()
		return ConflictDetail{}, err
	}
	if err := out.Close(); err != nil {
		return ConflictDetail{}, err
	}
	remotePaths, err := index.SnapshotPaths(plain)
	if err != nil {
		return ConflictDetail{}, fmt.Errorf("读取远端快照清单失败: %w", err)
	}
	localPaths, err := db.ListLivePaths()
	if err != nil {
		return ConflictDetail{}, err
	}
	d.Diff = DiffIndex(localPaths, remotePaths)
	return d, nil
}
