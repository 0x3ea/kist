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
	"fmt"
	"io"
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

// BackupNow 生成快照、加密并上传。CLI 手动触发;
// GUI 阶段另接防抖自动备份与退出前备份(Phase 7)。
func BackupNow(ctx context.Context, mk crypto.MasterKey, db *index.DB, s *remote.Store) (BackupInfo, error) {
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
	if err := db.SetLastBackupAt(time.Now().Unix()); err != nil {
		return BackupInfo{}, err
	}
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
type PullResult struct {
	Action       string
	RemoteRev    uint64
	LocalRev     uint64
	RemoteDevice string // hex,与本地 device_id 不同即"另一台设备"
}

// PullRemote 拉取远端索引备份并按 LWW 决策:
//
//	remote > local  完整解密校验后 ReplaceWith(旧库自动归档 backups/)
//	remote == local noop
//	remote < local  local-newer,不动本地(建议先推送备份)
//
// 替换前必须完整解密:块认证 + 明文 SHA 终检全部通过才落地,
// 防止把损坏的快照装进本地。
func PullRemote(ctx context.Context, mk crypto.MasterKey, s *remote.Store, db *index.DB) (PullResult, error) {
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
	res := PullResult{
		RemoteRev:    meta.Revision,
		LocalRev:     localRev,
		RemoteDevice: hex.EncodeToString(meta.DeviceID[:]),
	}
	switch {
	case res.RemoteRev < localRev:
		res.Action = "local-newer"
		return res, nil
	case res.RemoteRev == localRev:
		res.Action = "noop"
		return res, nil
	}

	// 远端较新:完整解密到明文库文件,任一块认证失败即中止
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
	res.Action = "replaced"
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
