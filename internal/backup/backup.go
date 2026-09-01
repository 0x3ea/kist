// Package backup 实现索引云备份与多设备恢复。
//
// 备份 = VACUUM INTO 一致性快照 → blob 格式加密(Meta 填 Mtime/Revision/DeviceID)
// → PUT /kist/index.enc。备注、缩略图、user_meta 都在库文件里,随备份一起同步。
// 恢复按 LWW(以 revision 判定,不信任系统时钟),落后一方的本地库归档
// 保留而非合并——kist 面向单用户、同时单写者。
package backup

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"kist/internal/crypto"
	"kist/internal/index"
	"kist/internal/remote"
)

// BackupInfo 描述一次成功的索引备份。
type BackupInfo struct {
	Revision uint64
	Size     int64 // 加密后大小(字节)
	At       time.Time
}

// 同步分叉检测的两种结论(TODO-09)。
const (
	KindDiverged    = "diverged"     // 本机与远端在基线后都有改动,拒绝静默覆盖
	KindRemoteAhead = "remote-ahead" // 只有远端动过(本机干净),该拉不该推
)

// Conflict 是 push/pull 前三方比较(本地/基线/远端)拦下的同步决策。
// 实现 error 以便走常规错误通道;CLI/GUI 用 errors.As 取出结构化字段
// 呈现裁决入口(保留本机 = 覆盖远端;保留云端 = 本地归档)。
type Conflict struct {
	Kind         string
	LocalRev     uint64
	RemoteRev    uint64
	BaselineRev  uint64
	RemoteDevice string // hex;注:库经 ReplaceWith 迁移后各端 device_id 会趋同,仅作参考
}

func (e *Conflict) Error() string {
	switch e.Kind {
	case KindDiverged:
		return fmt.Sprintf("同步分叉:本机(revision %d)与远端(revision %d,来自设备 %s)在基线 %d 之后都有改动——保留本机:backup --force;保留云端:pull --force",
			e.LocalRev, e.RemoteRev, e.RemoteDevice, e.BaselineRev)
	default:
		return fmt.Sprintf("远端索引较新(revision %d,本机自基线 %d 后无改动):先 pull 拉取,而不是覆盖远端",
			e.RemoteRev, e.BaselineRev)
	}
}

// remoteIndexMeta 拉取远端 index.enc 并只读头部 Meta(开头部认证章,不解密
// 正文)——三方比较的"远端证人"。从未备份过时 existent=false(O(1) Probe
// 先行,TODO-11 的探测原语)。
// 廉价化(Range 只拉 156B 头)依赖服务器支持 Range,当前网盘(123pan)不支持,
// 走整拉退化;备份本身也要走一次同量级上传,可接受。
func remoteIndexMeta(ctx context.Context, mk crypto.MasterKey, s *remote.Store) (meta crypto.Meta, existent bool, err error) {
	ok, _, err := s.ProbeBlob(ctx, remote.IndexName)
	if err != nil || !ok {
		return meta, false, err // 网络错误上抛;404 = 从未备份,不算错误
	}
	dir, err := os.MkdirTemp("", "kist-probe-*")
	if err != nil {
		return meta, false, err
	}
	defer os.RemoveAll(dir)
	enc := filepath.Join(dir, "index.enc")
	if err := s.GetIndexBlob(ctx, enc); err != nil {
		return meta, false, fmt.Errorf("探测远端索引失败: %w", err)
	}
	f, err := os.Open(enc)
	if err != nil {
		return meta, false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return meta, false, err
	}
	br, err := crypto.NewBlobReader(f, st.Size(), mk)
	if err != nil {
		return meta, false, err // ErrWrongKey(指向别的账户)或 ErrCorruptBlob
	}
	return br.Meta(), true, nil
}

// BackupNow 生成快照、加密并上传。推送前做三方比较(本地/基线/远端):
// 只有本机动过才放心推;只有远端动过提示先拉;双方都动过即分叉,拒绝静默
// 覆盖并返回 *Conflict(force=true 跳过检测,分叉裁决"保留本机"走这里)。
// 成功后基线 = 本次推送的 revision。
func BackupNow(ctx context.Context, mk crypto.MasterKey, db *index.DB, s *remote.Store, force bool) (BackupInfo, error) {
	// revision/device 在快照前读取:快照里的值必须与内容一致
	rev, err := db.Revision()
	if err != nil {
		return BackupInfo{}, err
	}
	devHex, err := db.DeviceID()
	if err != nil {
		return BackupInfo{}, err
	}
	devID, err := deviceFromHex(devHex)
	if err != nil {
		return BackupInfo{}, err
	}

	// push 侧执法(TODO-09):远端是哑的,覆盖裁决由客户端完成。
	// 检测条件用"≠ 基线"而非比大小,平局(两侧各写一次)也逃不掉。
	if !force {
		baseline, err := db.LastSyncedRev()
		if err != nil {
			return BackupInfo{}, err
		}
		meta, existent, err := remoteIndexMeta(ctx, mk, s)
		if errors.Is(err, crypto.ErrCorruptBlob) {
			// 远端头部读不出来(如上传中断截断):pull 同样会拒,若再拒推
			// 用户就死锁了。旧系统此处即覆盖自愈,沿用——推是唯一出路。
			slog.Warn("远端索引头部损坏,跳过分叉检测直接推送(自愈)", "err", err)
		} else if err != nil {
			return BackupInfo{}, err // ErrWrongKey = 远端是别人的账户,绝不覆盖
		} else if existent && meta.Revision != baseline {
			c := &Conflict{
				Kind:         KindDiverged,
				LocalRev:     rev,
				RemoteRev:    meta.Revision,
				BaselineRev:  baseline,
				RemoteDevice: hex.EncodeToString(meta.DeviceID[:]),
			}
			if rev == baseline { // 本机没动,是远端单独走了:该拉不该推
				c.Kind = KindRemoteAhead
			}
			slog.Warn("push 被同步检测拦下", "kind", c.Kind, "local", rev, "remote", c.RemoteRev, "baseline", baseline)
			return BackupInfo{}, c
		}
	}

	dir, err := os.MkdirTemp("", "kist-backup-*")
	if err != nil {
		return BackupInfo{}, err
	}
	defer os.RemoveAll(dir)
	snap := filepath.Join(dir, "snap.db")
	enc := filepath.Join(dir, "index.enc")

	if err := db.SnapshotTo(snap); err != nil {
		return BackupInfo{}, err
	}
	if err := encryptFile(mk, snap, enc, crypto.EncryptOptions{
		Mtime: uint64(time.Now().Unix()), Revision: rev, DeviceID: devID,
	}); err != nil {
		return BackupInfo{}, err
	}
	st, err := os.Stat(enc)
	if err != nil {
		return BackupInfo{}, err
	}
	f, err := os.Open(enc)
	if err != nil {
		return BackupInfo{}, err
	}
	defer f.Close()
	if err := s.PutIndexBlob(ctx, f); err != nil {
		return BackupInfo{}, err
	}
	// 推送成功 = 本机与远端在此 revision 上达成一致,基线推进到它
	if err := db.SetLastSyncedRev(rev); err != nil {
		return BackupInfo{}, err
	}
	if err := db.SetLastBackupAt(time.Now().Unix()); err != nil {
		return BackupInfo{}, err
	}
	slog.Info("索引备份完成", "revision", rev, "size", st.Size())
	return BackupInfo{Revision: rev, Size: st.Size(), At: time.Now()}, nil
}

// encryptFile 用 blob 格式把 src 加密写到 dst(备份与未来其他整库导出共用)。
func encryptFile(mk crypto.MasterKey, src, dst string, opt crypto.EncryptOptions) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	bw, err := crypto.NewBlobWriter(out, mk, opt)
	if err != nil {
		out.Close()
		return err
	}
	if _, err := io.Copy(bw, in); err != nil {
		out.Close()
		return err
	}
	if err := bw.Close(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// PullResult 是恢复决策结果;Action ∈ replaced | noop | local-newer。
// Forked=true 表示这是分叉裁决(force)采纳远端的结果,本地改动已归档。
type PullResult struct {
	Action       string
	RemoteRev    uint64
	LocalRev     uint64
	BaselineRev  uint64 // 决策时的同步基线(TODO-09)
	RemoteDevice string // hex,与本地 device_id 不同即"另一台设备"(经 ReplaceWith 迁移后会趋同,仅参考)
	Forked       bool
}

// PullRemote 拉取远端索引备份并按三方比较决策(TODO-09:revision 是证人,
// 只回答"谁在基线之后动过",不再比大小——平局盲区随之消除):
//
//	本机未动 + 远端动过   快进:完整解密校验后 ReplaceWith(旧库自动归档 backups/)
//	双方都未动            noop
//	只有本机动过          local-newer,不动本地(建议先推送备份)
//	双方都动过            分叉!返回 *Conflict;force=true 时裁决"保留云端"
//	                      ——本机改动归档,采纳远端并把基线推进到它
//
// 替换前必须完整解密:块认证 + 明文 SHA 终检全部通过才落地,
// 防止把损坏的快照装进本地。替换后基线必须显式重写:快照是对方整库,
// 带着的是对方的基线,不能沿用。
func PullRemote(ctx context.Context, mk crypto.MasterKey, s *remote.Store, db *index.DB, force bool) (PullResult, error) {
	dir, err := os.MkdirTemp("", "kist-pull-*")
	if err != nil {
		return PullResult{}, err
	}
	defer os.RemoveAll(dir)
	enc := filepath.Join(dir, "index.enc")

	if err := s.GetIndexBlob(ctx, enc); err != nil {
		return PullResult{}, fmt.Errorf("拉取远端索引备份失败(可能从未备份过): %w", err)
	}
	f, err := os.Open(enc)
	if err != nil {
		return PullResult{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return PullResult{}, err
	}
	br, err := crypto.NewBlobReader(f, st.Size(), mk)
	if err != nil {
		return PullResult{}, err // ErrWrongKey(指向别的账户)或 ErrCorruptBlob
	}
	meta := br.Meta()
	localRev, err := db.Revision()
	if err != nil {
		return PullResult{}, err
	}
	baseline, err := db.LastSyncedRev()
	if err != nil {
		return PullResult{}, err
	}
	res := PullResult{
		RemoteRev:    meta.Revision,
		LocalRev:     localRev,
		BaselineRev:  baseline,
		RemoteDevice: hex.EncodeToString(meta.DeviceID[:]),
	}
	localMoved := localRev != baseline
	remoteMoved := meta.Revision != baseline
	switch {
	case !localMoved && !remoteMoved:
		res.Action = "noop"
		slog.Info("pull 决策:版本一致,无需恢复", "revision", localRev)
		return res, nil
	case localMoved && !remoteMoved:
		res.Action = "local-newer"
		slog.Info("pull 决策:本地更新,保留本地", "local", localRev, "remote", res.RemoteRev)
		return res, nil
	case localMoved && remoteMoved && !force:
		slog.Warn("pull 被同步检测拦下:分叉", "local", localRev, "remote", res.RemoteRev, "baseline", baseline)
		return res, &Conflict{
			Kind:         KindDiverged,
			LocalRev:     localRev,
			RemoteRev:    meta.Revision,
			BaselineRev:  baseline,
			RemoteDevice: res.RemoteDevice,
		}
	}

	// 快进(force 时含分叉裁决"保留云端"):完整解密到明文库文件,任一块
	// 认证失败即中止
	plain := filepath.Join(dir, "snap.db")
	out, err := os.Create(plain)
	if err != nil {
		return res, err
	}
	if _, err := io.Copy(out, br); err != nil {
		out.Close()
		return res, err
	}
	if err := out.Close(); err != nil {
		return res, err
	}
	if err := f.Close(); err != nil {
		return res, err
	}
	if err := db.ReplaceWith(plain); err != nil {
		return res, err
	}
	// 快照带着对方的基线,不能沿用:采纳远端后,共识点就是它的 revision
	if err := db.SetLastSyncedRev(meta.Revision); err != nil {
		return res, err
	}
	res.Action = "replaced"
	res.Forked = localMoved // 双方都动过走到这里 = 分叉裁决
	if res.Forked {
		slog.Info("pull 决策:分叉裁决,保留云端", "local", localRev, "remote", res.RemoteRev, "baseline", baseline)
	} else {
		slog.Info("pull 决策:远端更新,本地已恢复", "local", localRev, "remote", res.RemoteRev, "device", res.RemoteDevice)
	}
	return res, nil
}

func deviceFromHex(s string) ([8]byte, error) {
	var id [8]byte
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 8 {
		return id, fmt.Errorf("backup: 非法 device_id %q", s)
	}
	copy(id[:], b)
	return id, nil
}
