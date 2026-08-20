package migrate

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"kist/internal/dav"
	"kist/internal/remote"
)

// Options 一次迁移的全部输入;Src 来自当前配置,Dst 为新网盘客户端。
type Options struct {
	Src         *remote.Store
	Dst         dav.Client
	DstRoot     string // 目标根目录(与 Dst 的 RootPath 一致),空则 "/kist"
	Concurrency int    // 并发搬运数,夹取 1–4,默认 2
}

// ObjFailure 记录一个搬运失败的对象;重跑 migrate 会自动重试它。
type ObjFailure struct {
	Name string
	Err  string
}

// Result 是迁移汇总;Failed>0 时不得 --switch。
type Result struct {
	Copied   int
	Skipped  int
	Failed   int
	Bytes    int64
	Failures []ObjFailure
	// NoIndexBackup:源端没有 index.enc(从未 backup)——目标端迁移后
	// 无法被新设备 pull,应尽快在新端 backup 一次。
	NoIndexBackup bool
}

// Run 枚举源端全部对象(keyfile、index.enc、所有 blob)搬运到目标端。
func Run(ctx context.Context, o Options) (Result, error) {
	var res Result
	if o.Concurrency < 1 {
		o.Concurrency = 1
	}
	if o.Concurrency > 4 {
		o.Concurrency = 4
	}
	if o.DstRoot == "" {
		o.DstRoot = "/kist"
	}

	objs, err := o.Src.ListAll(ctx)
	if err != nil {
		return res, fmt.Errorf("migrate: 枚举源端对象失败: %w", err)
	}
	sizes := make(map[string]int64, len(objs))
	names := make([]string, 0, len(objs))
	for _, ob := range objs {
		names = append(names, ob.Name)
		sizes[ob.Name] = ob.Size
	}
	sort.Strings(names)
	if _, ok := sizes[remote.KeyFileName]; !ok {
		return res, fmt.Errorf("migrate: 源端没有 keyfile:不是已初始化的 kist 库")
	}
	if _, ok := sizes[remote.IndexName]; !ok {
		res.NoIndexBackup = true
		slog.Warn("源端没有 index.enc(从未备份):迁移后新端需尽快 backup 一次")
	}
	if err := o.Dst.EnsureRoot(ctx); err != nil {
		return res, fmt.Errorf("migrate: 初始化目标根目录失败: %w", err)
	}

	start := time.Now()
	retry := dav.NewRetrier()
	var mu sync.Mutex
	var next, progress atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < o.Concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= len(names) {
					return
				}
				name := names[i]
				skipped, err := copyOne(ctx, o, retry, name, sizes[name])
				mu.Lock()
				switch {
				case err != nil:
					res.Failed++
					res.Failures = append(res.Failures, ObjFailure{name, err.Error()})
				case skipped:
					res.Skipped++
				default:
					res.Copied++
					res.Bytes += sizes[name]
				}
				mu.Unlock()
				if err != nil {
					slog.Warn("migrate 对象搬运失败(重跑可续)", "name", name, "err", err)
				}
				if n := progress.Add(1); n%100 == 0 {
					slog.Info("migrate 进度", "done", int(n), "total", len(names),
						"elapsed", time.Since(start).Round(time.Second).String())
				}
			}
		}()
	}
	wg.Wait()

	// 校验:keyfile 与 index.enc 双端流式哈希比对(断点跳过的对象同样覆盖)。
	// blob 不逐一回读——AEAD 保证搬坏必在读取时暴露(评估文档的校验哲学)。
	for _, name := range []string{remote.KeyFileName, remote.IndexName} {
		if name == remote.IndexName && res.NoIndexBackup {
			continue
		}
		hs, err := hashStream(func() (io.ReadCloser, error) { return o.Src.GetBlobBody(ctx, name) })
		if err != nil {
			return res, fmt.Errorf("migrate: 校验读取源端 %s 失败: %w", name, err)
		}
		hd, err := hashStream(func() (io.ReadCloser, error) { return o.Dst.GetBody(ctx, o.DstRoot+"/"+name) })
		if err != nil {
			return res, fmt.Errorf("migrate: 校验读取目标端 %s 失败: %w", name, err)
		}
		if hs != hd {
			return res, fmt.Errorf("migrate: %s 搬运后双端哈希不一致,请重跑迁移续传", name)
		}
	}
	slog.Info("migrate 完成", "copied", res.Copied, "skipped", res.Skipped,
		"failed", res.Failed, "bytes", res.Bytes,
		"elapsed", time.Since(start).Round(time.Second).String())
	return res, nil
}

// copyOne 搬运单个对象;返回是否因"目标已存在且大小相同"而跳过。
// 重试在整对象粒度:每次尝试重开 GET、重发 PUT(流不可回卷);
// 目标上的半截残留(大小不符)不会被跳过,下一轮覆盖重传。
func copyOne(ctx context.Context, o Options, retry *dav.Retrier, name string, size int64) (bool, error) {
	abs := o.DstRoot + "/" + name
	found, dsize, err := o.Dst.Probe(ctx, abs)
	if err != nil {
		return false, fmt.Errorf("探测目标: %w", err)
	}
	if found && dsize >= 0 && dsize == size {
		return true, nil // 上次已搬完:断点跳过(dsize<0 = 服务器未报大小,保守不跳)
	}
	err = retry.Do(ctx, func() error {
		body, err := o.Src.GetBlobBody(ctx, name)
		if err != nil {
			return err
		}
		defer body.Close()
		return o.Dst.PutStream(ctx, abs, size, body)
	})
	return false, err
}

func hashStream(open func() (io.ReadCloser, error)) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	body, err := open()
	if err != nil {
		return sum, err
	}
	defer body.Close()
	h := sha256.New()
	if _, err := io.Copy(h, body); err != nil {
		return sum, err
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}
