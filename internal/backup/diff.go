package backup

import (
	"sort"

	"kist/internal/index"
)

// DiffList 是一栏差异清单:Items 供展示(截断到 diffCap),Total 是真实条数。
type DiffList struct {
	Items []string `json:"items"`
	Total int      `json:"total"`
}

// DiffResult 是文件级 diff 的三栏(TODO-22):仅本地 / 仅远端 / 两侧都有但
// 内容不同。分叉的裁决粒度是整库二选一(不做行级 merge),diff 只是帮用户
// 判断"哪边更接近我想要的"的信息,不是逐文件勾选项。
type DiffResult struct {
	LocalOnly  DiffList `json:"localOnly"`
	RemoteOnly DiffList `json:"remoteOnly"`
	Changed    DiffList `json:"changed"`
}

// diffCap 每栏返回给前端的条数上限:弹窗装不下也不需要上万条路径,
// Total 保真,前端自行提示"共 N 条"。
const diffCap = 200

// DiffIndex 比对两份路径清单(本地 ListLivePaths 与远端 SnapshotPaths 的
// 产物),产出三栏 diff。键为 kind+path(同名目录与文件允许共存);
// 文件以 blob 名判"内容不同"(重上传必换随机对象名,改名/移动不换)。
func DiffIndex(local, remote []index.PathEntry) DiffResult {
	type key struct{ kind, path string }
	lm := make(map[key]string, len(local))
	for _, e := range local {
		lm[key{e.Kind, e.Path}] = e.Blob
	}
	rm := make(map[key]string, len(remote))
	for _, e := range remote {
		rm[key{e.Kind, e.Path}] = e.Blob
	}
	var res DiffResult
	add := func(dl *DiffList, p string) {
		dl.Total++
		if len(dl.Items) < diffCap {
			dl.Items = append(dl.Items, p)
		}
	}
	for k, lb := range lm {
		rb, ok := rm[k]
		switch {
		case !ok:
			add(&res.LocalOnly, k.path)
		case k.kind == "file" && lb != rb:
			add(&res.Changed, k.path)
		}
	}
	for k := range rm {
		if _, ok := lm[k]; !ok {
			add(&res.RemoteOnly, k.path)
		}
	}
	for _, dl := range []*DiffList{&res.LocalOnly, &res.RemoteOnly, &res.Changed} {
		sort.Strings(dl.Items)
	}
	return res
}
