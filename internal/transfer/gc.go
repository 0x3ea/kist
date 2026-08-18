package transfer

import (
	"context"

	"kist/internal/index"
	"kist/internal/remote"
)

// RunGC 垃圾回收:
//   - blobs 表里 state=trash 且远端确认存在的对象 → 删除(dryRun 只列出);
//   - 远端有、索引无的对象 → 孤儿,只报告不删(删除中断或另一设备索引回退所致,
//     是否清理由用户决定,Phase 7 GUI 会给出确认界面)。
func RunGC(ctx context.Context, store *remote.Store, db *index.DB, dryRun bool) (deleted, orphans []string, err error) {
	remoteList, err := store.ListBlobs(ctx)
	if err != nil {
		return nil, nil, err
	}
	remoteSet := make(map[string]bool, len(remoteList))
	for _, n := range remoteList {
		remoteSet[n] = true
	}
	states, err := db.ListBlobStates()
	if err != nil {
		return nil, nil, err
	}
	for name, state := range states {
		if state != "trash" || !remoteSet[name] {
			continue
		}
		if dryRun {
			deleted = append(deleted, name)
			continue
		}
		if err := store.DeleteBlob(ctx, name); err != nil {
			return deleted, orphans, err
		}
		deleted = append(deleted, name)
	}
	for _, n := range remoteList {
		if _, ok := states[n]; !ok {
			orphans = append(orphans, n)
		}
	}
	return deleted, orphans, nil
}
