package transfer

import (
	"context"

	"kist/internal/index"
	"kist/internal/remote"
)

// GCResult 是一次 gc 的报告。孤儿一律只报告不删(删除中断或另一设备索引
// 回退所致,删除权在用户);封面孤儿(TODO-10,covers 命名空间)单独分组,
// 因为其中可能有 custom 语义的用户内容——与文件孤儿同样保守。
type GCResult struct {
	Deleted      []string // trash 且远端确认存在,已物理删除(不分 kind)
	Orphans      []string // 主命名空间孤儿(远端有、索引无)
	CoverOrphans []string // 封面命名空间孤儿(只报告)
}

// RunGC 垃圾回收(TODO-10 起覆盖两个远端命名空间):
//   - blobs 表里 state=trash 且远端确认存在的对象 → 删除;按 kind 路由
//     命名空间(file → /kist/,cover → /kist/covers/),kind 是唯一权威;
//   - 远端有、索引无的对象 → 孤儿,按命名空间分组报告,不自动删。
//
// "gc 不回收有引用的封面"由引用侧保证:封面引用消失时旧 blob 已同事务
// trash(替换/清除/rm 级联/discard 级联),gc 只删 trash,不碰 active。
func RunGC(ctx context.Context, store *remote.Store, db *index.DB, dryRun bool) (GCResult, error) {
	var res GCResult

	remoteList, err := store.ListBlobs(ctx)
	if err != nil {
		return res, err
	}
	mainSet := make(map[string]bool, len(remoteList))
	for _, n := range remoteList {
		mainSet[n] = true
	}
	coverList, err := store.ListCoverBlobs(ctx)
	if err != nil {
		return res, err
	}
	coverSet := make(map[string]bool, len(coverList))
	for _, o := range coverList {
		coverSet[o.Name] = true
	}

	records, err := db.ListBlobRecords()
	if err != nil {
		return res, err
	}
	states := make(map[string]index.BlobRecord, len(records))
	for _, r := range records {
		states[r.Name] = r
	}

	for _, r := range records {
		if r.State != "trash" {
			continue
		}
		isCover := r.Kind == "cover"
		exists := mainSet[r.Name]
		if isCover {
			exists = coverSet[r.Name]
		}
		if !exists {
			continue
		}
		if dryRun {
			res.Deleted = append(res.Deleted, r.Name)
			continue
		}
		if isCover {
			if err := store.DeleteCoverBlob(ctx, r.Name); err != nil {
				return res, err
			}
		} else {
			if err := store.DeleteBlob(ctx, r.Name); err != nil {
				return res, err
			}
		}
		res.Deleted = append(res.Deleted, r.Name)
	}
	for _, n := range remoteList {
		if _, ok := states[n]; !ok {
			res.Orphans = append(res.Orphans, n)
		}
	}
	for _, o := range coverList {
		if _, ok := states[o.Name]; !ok {
			res.CoverOrphans = append(res.CoverOrphans, o.Name)
		}
	}
	return res, nil
}
