// kistctl 是 kist 的命令行入口,与 GUI 共享全部核心逻辑(internal/ 下各包)。
// 规格见 docs/phase-4-pipeline-cli.md。
//
// 口令获取方式(两种口令都适用):
//   - 环境变量 KIST_PASS
//   - --pass-stdin:从 stdin 读一行(避免口令进 shell history)
package main

import (
	"bufio"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"kist/internal/backup"
	"kist/internal/config"
	"kist/internal/crypto"
	"kist/internal/dav"
	"kist/internal/errs"
	"kist/internal/index"
	"kist/internal/logging"
	"kist/internal/migrate"
	"kist/internal/remote"
	"kist/internal/transfer"
)

// version/commit 由构建时 -ldflags 注入;直接 go run/build 时显示 dev。
var (
	version = "dev"
	commit  = "unknown"
)

const usageText = `用法:kistctl <子命令> [参数]

子命令:
  config set --url <URL> --user <用户名> [--root /kist] [--pass-stdin]  配置网盘
  init    --pass-stdin                  建账户:主密钥+keyfile+远端初始化
  unlock  --pass-stdin                  校验口令(本地 keyfile 优先,无则拉远端)
  put     <路径...> [--dest /目录] [--defer] [--expand] --pass-stdin  加密上传(文件夹默认按叶子目录打包,一话一对象;--expand 逐文件;--defer 只入出站箱)
  outbox  list|push|verify|discard       出站箱:待上传产物的搬运与收账(TODO-13)
  ls      [/路径]                        列虚拟目录(目录行附子树摘要:话数·大小·最近更新)
  search  <关键词>                       搜索文件名、备注、目录名与目录 tag
  meta    set <目录|文件> [--note 文本] [--tag a,b] [--cover <uuid|id|0>(仅目录)]  设置元数据(不带 flag 则显示当前值);meta list 列出全部
  mv      <uuid|id...> <目标目录>        纯索引移动文件,零远端流量(归属给错时的便宜纠错)
  get     <uuid|id> --to <目录> [--keep-zip] --pass-stdin  下载解密(文件夹条目还原成目录;--keep-zip 落 zip)
  info    <uuid|id>                      查看明细(时间/备注/缩略图)
  note    <id> [--set 文本]              查看/设置备注
  rm      <id...>                        软删除文件
  gc      [--dry-run]                    清理 trash blob、报告孤儿
  backup  --pass-stdin                   加密备份索引到远端 index.enc
  pull    --pass-stdin                   从远端恢复索引(新设备/多设备同步)
  migrate --url <新URL> --user <用户名> [--pass-stdin] [--root /kist] [--switch]  网盘间纯密文迁移(断点续搬,不解锁)
  version                               显示版本`

func main() {
	// 全局日志:落盘 KIST_HOME/kist.log(TODO-07);失败降级为标准错误,不阻断命令
	if err := logging.Setup(); err != nil {
		fmt.Fprintln(os.Stderr, "警告:日志初始化失败,降级为标准错误输出:", err)
	}
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errs.New(errs.BadConfig, usageText)
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "config":
		err = cmdConfig(rest)
	case "init":
		err = cmdInit(rest)
	case "unlock":
		err = cmdUnlock(rest)
	case "put":
		err = cmdPut(rest)
	case "outbox":
		err = cmdOutbox(rest)
	case "ls":
		err = cmdLs(rest)
	case "search":
		err = cmdSearch(rest)
	case "meta":
		err = cmdMeta(rest)
	case "mv":
		err = cmdMv(rest)
	case "get":
		err = cmdGet(rest)
	case "info":
		err = cmdInfo(rest)
	case "note":
		err = cmdNote(rest)
	case "rm":
		err = cmdRm(rest)
	case "gc":
		err = cmdGc(rest)
	case "backup":
		err = cmdBackup(rest)
	case "pull":
		err = cmdPull(rest)
	case "migrate":
		err = cmdMigrate(rest)
	case "version", "-v", "--version":
		fmt.Printf("kistctl %s (%s)\n", version, commit)
	default:
		err = errs.New(errs.BadConfig, fmt.Sprintf("未知子命令 %q\n%s", cmd, usageText))
	}
	return err
}

// parseArgs 解析前先把参数重排为"flag 在前、位置参数在后":
// Go flag 遇到首个位置参数即停止解析,而 kistctl put /路径 --dest /x
// 这种直觉写法应当被允许。
func parseArgs(fs *flag.FlagSet, args []string) error {
	return fs.Parse(reorderFlags(fs, args))
}

func reorderFlags(fs *flag.FlagSet, args []string) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" { // "--" 之后强制视为位置参数
			rest = append(rest, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			// 非布尔 flag 的独立取值跟随其后
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && flagTakesValue(fs, a) {
				flags = append(flags, args[i+1])
				i++
			}
		} else {
			rest = append(rest, a)
		}
	}
	return append(flags, rest...)
}

func flagTakesValue(fs *flag.FlagSet, name string) bool {
	f := fs.Lookup(strings.TrimLeft(name, "-"))
	if f == nil {
		return false
	}
	bv, ok := f.Value.(interface{ IsBoolFlag() bool })
	return !ok || !bv.IsBoolFlag()
}

// readPass 取口令:KIST_PASS 优先,否则从 stdin 读一行。
func readPass(useStdin bool) (string, error) {
	if v := os.Getenv("KIST_PASS"); v != "" {
		return v, nil
	}
	if !useStdin {
		return "", errs.New(errs.BadConfig, "未提供口令:用 --pass-stdin 或环境变量 KIST_PASS")
	}
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return "", errs.New(errs.BadConfig, "stdin 无口令输入")
	}
	return sc.Text(), sc.Err()
}

func passStdinFlag(fs *flag.FlagSet) *bool {
	return fs.Bool("pass-stdin", false, "从 stdin 读一行口令(或用环境变量 KIST_PASS)")
}

// loadStore 读取配置并连上远端;未配置时返回 NOT_CONFIGURED。
func loadStore() (*config.StoredConfig, *remote.Store, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	if cfg.URL == "" || cfg.Username == "" {
		return nil, nil, errs.New(errs.NotConfigured,
			"尚未配置网盘:先执行 kistctl config set --url <URL> --user <用户名> --pass-stdin")
	}
	c, err := dav.New(dav.Config{URL: cfg.URL, Username: cfg.Username, Password: cfg.Password, RootPath: cfg.RootPath})
	if err != nil {
		return nil, nil, errs.Wrap(errs.BadConfig, err)
	}
	return cfg, remote.NewStore(c, cfg.RootPath), nil
}

func openIndex() (*index.DB, error) {
	db, err := index.Open(config.IndexPath())
	if err != nil {
		return nil, errs.From(err)
	}
	return db, nil
}

// loadKeyFileBytes 本地 keyfile 优先;没有则从远端拉一份并缓存(新设备)。
func loadKeyFileBytes(store *remote.Store) ([]byte, error) {
	if b, err := os.ReadFile(config.KeyFilePath()); err == nil {
		return b, nil
	}
	b, err := store.GetKeyFile(context.Background())
	if err != nil {
		return nil, errs.Wrap(errs.DavError, err)
	}
	if err := os.WriteFile(config.KeyFilePath(), b, 0o600); err != nil {
		return nil, err
	}
	return b, nil
}

func unlockMK(store *remote.Store, pass string) (crypto.MasterKey, error) {
	b, err := loadKeyFileBytes(store)
	if err != nil {
		return crypto.MasterKey{}, err
	}
	kf, err := crypto.ParseKeyFile(b)
	if err != nil {
		return crypto.MasterKey{}, errs.Wrap(errs.Corrupt, err)
	}
	mk, err := kf.Unlock(pass)
	if err != nil {
		return crypto.MasterKey{}, errs.From(err)
	}
	return mk, nil
}

// splitVirtualPath 把虚拟目录路径 "/a/b" 拆成 ["a","b"]。
// 容忍空段与 "."(如手滑写 "./result");".." 直接报错——
// 虚拟路径从根写起,不存在向上逃逸。
func splitVirtualPath(p string) ([]string, error) {
	var segs []string
	for _, s := range strings.Split(strings.TrimSpace(p), "/") {
		switch s {
		case "", ".":
			continue
		case "..":
			return nil, errs.New(errs.BadConfig,
				"虚拟目录路径不支持 \"..\":请从根写起,如 /测试/子目录")
		default:
			segs = append(segs, s)
		}
	}
	return segs, nil
}

// ---- 子命令实现 ----

func cmdConfig(args []string) error {
	if len(args) == 0 || args[0] != "set" {
		return errs.New(errs.BadConfig, "用法:kistctl config set --url <URL> --user <用户名> [--pass-stdin]")
	}
	args = args[1:]
	fs := flag.NewFlagSet("config set", flag.ContinueOnError)
	url := fs.String("url", "", "WebDAV 地址(如 https://dav.example.com/dav)")
	user := fs.String("user", "", "用户名")
	root := fs.String("root", "", "远端根目录(默认 /kist)")
	passStdin := fs.Bool("pass-stdin", false, "从 stdin 读一行 WebDAV 密码")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if *url != "" {
		cfg.URL = *url
	}
	if *user != "" {
		cfg.Username = *user
	}
	if *root != "" {
		cfg.RootPath = *root
	}
	if *passStdin {
		pw, err := readPass(true) // 这里的口令是 WebDAV 账户密码
		if err != nil {
			return err
		}
		cfg.Password = pw
		cfg.Settings.RememberPassword = true
		fmt.Println("注意:WebDAV 密码将以明文保存在", config.ConfigPath())
	}
	if cfg.URL == "" || cfg.Username == "" {
		return errs.New(errs.BadConfig, "--url 与 --user 均为必填")
	}
	if err := config.Save(cfg); err != nil {
		return err
	}

	// 顺手验证连通性,失败不阻断保存
	_, store, err := loadStore()
	if err != nil {
		return err
	}
	if err := store.Ping(context.Background()); err != nil {
		return errs.Wrap(errs.DavError, fmt.Errorf("配置已保存,但连接失败: %w", err))
	}
	fmt.Println("配置已保存,连接正常")
	return nil
}

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	passStdin := passStdinFlag(fs)
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	pass, err := readPass(*passStdin)
	if err != nil {
		return err
	}
	_, store, err := loadStore()
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := store.EnsureReady(ctx); err != nil {
		return errs.Wrap(errs.DavError, err)
	}
	exists, err := store.KeyFileExists(ctx)
	if err != nil {
		return errs.Wrap(errs.DavError, err)
	}
	if exists {
		return errs.New(errs.BadConfig,
			"远端已有 keyfile(此网盘目录已被初始化);如需重新开始请先手动清理远端目录")
	}
	kf, mk, err := crypto.CreateKeyFile(pass, crypto.DefaultArgon2Params())
	if err != nil {
		return errs.From(err)
	}
	if err := store.PutKeyFile(ctx, kf.Bytes()); err != nil {
		return errs.Wrap(errs.DavError, err)
	}
	if err := os.WriteFile(config.KeyFilePath(), kf.Bytes(), 0o600); err != nil {
		return err
	}
	db, err := openIndex()
	if err != nil {
		return err
	}
	db.Close()
	mk.Wipe()
	fmt.Printf("账户已建立:远端 %s/keyfile,本地索引 %s\n", cfgRootPath(), config.IndexPath())
	return nil
}

func cfgRootPath() string {
	cfg, _ := config.Load()
	if cfg.RootPath == "" {
		return "/kist"
	}
	return cfg.RootPath
}

func cmdUnlock(args []string) error {
	fs := flag.NewFlagSet("unlock", flag.ContinueOnError)
	passStdin := passStdinFlag(fs)
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	pass, err := readPass(*passStdin)
	if err != nil {
		return err
	}
	_, store, err := loadStore()
	if err != nil {
		return err
	}
	mk, err := unlockMK(store, pass)
	if err != nil {
		return err
	}
	mk.Wipe()
	fmt.Println("解锁成功,口令与 keyfile 校验通过")
	return nil
}

// newManager 构建 CLI 版管线;Emit 只在阶段变化时打印一行。
func newManager(cfg *config.StoredConfig, store *remote.Store, db *index.DB, mk crypto.MasterKey) *transfer.Manager {
	var mu sync.Mutex
	lastPhase := map[string]string{}
	emit := func(event string, payload any) {
		if event != "transfer:update" {
			return
		}
		tr, ok := payload.(transfer.Transfer)
		if !ok {
			return
		}
		mu.Lock()
		last := lastPhase[tr.ID]
		lastPhase[tr.ID] = tr.Phase
		mu.Unlock()
		if tr.Phase != last {
			fmt.Printf("  [%s] %s\n", tr.Phase, tr.Name)
		}
	}
	return transfer.NewManager(transfer.Deps{
		Remote:      store,
		DB:          db,
		MK:          func() (crypto.MasterKey, bool) { return mk, true },
		Concurrency: func() int { return cfg.Settings.Concurrency },
		ChunkMiB:    func() int { return cfg.Settings.ChunkMiB },
		NoPad:       func() bool { return cfg.Settings.SizePadding == "off" },
		Emit:        emit,
	})
}

// waitAndReport 等全部传输结束并汇总;返回失败/取消数(deferred 不算失败)。
func waitAndReport(m *transfer.Manager) int {
	for !m.Idle() {
		time.Sleep(100 * time.Millisecond)
	}
	fails := 0
	for _, tr := range m.Snapshot() {
		switch tr.Phase {
		case transfer.PhaseDone:
			fmt.Printf("  ✓ %s\n", tr.Name)
		case transfer.PhaseDeferred:
			fmt.Printf("  ⏸ 已入出站箱(待上传): %s\n", tr.Name)
		case transfer.PhaseCanceled:
			fmt.Printf("  – 已取消 %s\n", tr.Name)
			fails++
		default:
			fmt.Printf("  ✗ %s:%s\n", tr.Name, tr.Err)
			fails++
		}
	}
	return fails
}

func cmdPut(args []string) error {
	fs := flag.NewFlagSet("put", flag.ContinueOnError)
	dest := fs.String("dest", "/", "目标虚拟目录(如 /文档/子目录)")
	deferUpload := fs.Bool("defer", false, "只加密并入出站箱,不立即上传(择机 outbox push 或手工搬运)")
	expand := fs.Bool("expand", false, "逐文件展开(旧行为:每文件一个加密对象;默认文件夹按叶子目录打包)")
	passStdin := passStdinFlag(fs)
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	paths := fs.Args()
	if len(paths) == 0 {
		return errs.New(errs.BadConfig, "put 需要至少一个文件/文件夹路径")
	}
	pass, err := readPass(*passStdin)
	if err != nil {
		return err
	}
	cfg, store, err := loadStore()
	if err != nil {
		return err
	}
	mk, err := unlockMK(store, pass)
	if err != nil {
		return err
	}
	db, err := openIndex()
	if err != nil {
		return err
	}
	defer db.Close()

	segs, err := splitVirtualPath(*dest)
	if err != nil {
		return err
	}
	folderID := int64(1)
	if len(segs) > 0 {
		if err := db.WithTx(func(tx *sql.Tx) error {
			var err error
			folderID, err = db.EnsureFolderPath(tx, 1, segs)
			return err
		}); err != nil {
			return errs.From(err)
		}
	}
	m := newManager(cfg, store, db, mk)
	var opts []transfer.UploadOptions
	if *expand {
		opts = append(opts, transfer.UploadOptions{Expand: true})
	}
	var n int
	if *deferUpload {
		n, err = m.DeferPaths(context.Background(), paths, folderID, opts...)
	} else {
		n, err = m.UploadPaths(context.Background(), paths, folderID, opts...)
	}
	if err != nil {
		return err
	}
	fmt.Printf("已入队 %d 个文件\n", n)
	if fails := waitAndReport(m); fails > 0 {
		return errs.New(errs.Internal, fmt.Sprintf("%d 个传输失败或被取消", fails))
	}
	return nil
}

// ---- 出站箱(TODO-13)----

func cmdOutbox(args []string) error {
	if len(args) == 0 {
		return errs.New(errs.BadConfig, "用法:kistctl outbox <list|push|verify|discard> ...")
	}
	switch args[0] {
	case "list":
		return cmdOutboxList(args[1:])
	case "push":
		return cmdOutboxPush(args[1:])
	case "verify":
		return cmdOutboxVerify(args[1:])
	case "discard":
		return cmdOutboxDiscard(args[1:])
	default:
		return errs.New(errs.BadConfig, "未知 outbox 子命令 "+args[0]+"(可用:list|push|verify|discard)")
	}
}

func cmdOutboxList(args []string) error {
	db, err := openIndex()
	if err != nil {
		return err
	}
	defer db.Close()
	files, err := db.ListUploading()
	if err != nil {
		return errs.From(err)
	}
	if len(files) == 0 {
		fmt.Println("(出站箱为空)")
		return nil
	}
	total := int64(0)
	for _, f := range files {
		mark := ""
		if st, err := os.Stat(transfer.OutboxArtifactPath(f.BlobName)); err != nil {
			mark = "  [本地产物缺失!]"
		} else {
			total += st.Size()
		}
		fmt.Printf("%s\t%s\t密文 %d 字节%s\n", f.BlobName, filePathOf(db, f), f.CipherSize, mark)
	}
	fmt.Printf("共 %d 个待上传,本地产物占用 %d 字节;手工搬运 = 把产物文件名保持原样上传到远端 %s 后执行 outbox verify\n",
		len(files), total, cfgRootPath())
	return nil
}

func cmdOutboxPush(args []string) error {
	fs := flag.NewFlagSet("outbox push", flag.ContinueOnError)
	all := fs.Bool("all", false, "推送全部待上传对象")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	cfg, store, err := loadStore()
	if err != nil {
		return err
	}
	db, err := openIndex()
	if err != nil {
		return err
	}
	defer db.Close()

	var files []index.FileRow
	if *all {
		if files, err = db.ListUploading(); err != nil {
			return errs.From(err)
		}
	} else {
		if fs.NArg() == 0 {
			return errs.New(errs.BadConfig, "指定 <blob名>(outbox list 可查)或 --all")
		}
		for _, name := range fs.Args() {
			f, err := db.GetFileByBlobName(name)
			if err != nil {
				return errs.From(err)
			}
			if f.State != "uploading" {
				return errs.New(errs.BadConfig, name+" 不是待上传对象(状态 "+f.State+")")
			}
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		fmt.Println("(无待上传对象)")
		return nil
	}
	// push 是纯密文搬运:不解密,MK 恒为未解锁
	m := transfer.NewManager(transfer.Deps{
		Remote:          store,
		DB:              db,
		MK:              func() (crypto.MasterKey, bool) { return crypto.MasterKey{}, false },
		Concurrency:     func() int { return cfg.Settings.Concurrency },
		PushFailDiscard: func() bool { return cfg.Settings.OutboxPushFail == "discard" },
	})
	n := m.PushPending(context.Background(), files)
	fmt.Printf("已入队 %d 个 push\n", n)
	if fails := waitAndReport(m); fails > 0 {
		if cfg.Settings.OutboxPushFail == "discard" {
			return errs.New(errs.Internal,
				fmt.Sprintf("%d 个 push 失败(discard 档):已回滚,可重新 put --defer 后手工搬运", fails))
		}
		return errs.New(errs.Internal,
			fmt.Sprintf("%d 个 push 失败(keep 档):索引与产物保留——可重试 outbox push、手工搬运后 outbox verify,或 outbox discard 放弃", fails))
	}
	return nil
}

func cmdOutboxVerify(args []string) error {
	_, store, err := loadStore()
	if err != nil {
		return err
	}
	db, err := openIndex()
	if err != nil {
		return err
	}
	defer db.Close()
	results, err := transfer.RunOutboxVerify(context.Background(), store, db)
	if err != nil {
		return errs.From(err)
	}
	if len(results) == 0 {
		fmt.Println("(出站箱为空)")
		return nil
	}
	for _, r := range results {
		fmt.Printf("%s\t%s\t%s\n", r.Blob, r.Action, r.Detail)
	}
	return nil
}

func cmdOutboxDiscard(args []string) error {
	if len(args) == 0 {
		return errs.New(errs.BadConfig, "outbox discard 需要 <blob名>...(outbox list 可查)")
	}
	db, err := openIndex()
	if err != nil {
		return err
	}
	defer db.Close()
	for _, name := range args {
		if err := transfer.OutboxDiscard(db, name); err != nil {
			return errs.From(err)
		}
		fmt.Printf("已放弃 %s(索引行与本地产物已删)\n", name)
	}
	return nil
}

func cmdLs(args []string) error {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	path := "/"
	if fs.NArg() > 0 {
		path = fs.Arg(0)
	}
	db, err := openIndex()
	if err != nil {
		return err
	}
	defer db.Close()
	segs, err := splitVirtualPath(path)
	if err != nil {
		return err
	}
	folderID, err := db.ResolveFolderPath(segs)
	if err != nil {
		return errs.From(err)
	}
	entries, err := db.ListFolder(folderID)
	if err != nil {
		return errs.From(err)
	}
	if len(entries) == 0 {
		fmt.Println("(空目录)")
		return nil
	}
	// 目录行附子树摘要(TODO-16):一次建树批量聚合,不逐目录重复载入
	var folderIDs []int64
	for _, e := range entries {
		if e.IsFolder {
			folderIDs = append(folderIDs, e.ID)
		}
	}
	sums := map[int64]index.FolderSummary{}
	if len(folderIDs) > 0 {
		if sums, err = db.FolderSummaries(folderIDs); err != nil {
			return errs.From(err)
		}
	}
	for _, e := range entries {
		if e.IsFolder {
			fmt.Printf("D %s/\t%s\n", e.Name, summaryLine(sums[e.ID]))
			continue
		}
		mark := ""
		if e.State == "uploading" {
			mark = "\t待上传"
		}
		if e.Pack {
			fmt.Printf("P %s/\t%d 字节%s\n", e.Name, e.Size, mark)
			continue
		}
		fmt.Printf("F %s\t%d 字节%s\n", e.Name, e.Size, mark)
	}
	return nil
}

// summaryLine 渲染子树摘要:"12 话 · 8.2GB · ← 08-01,待传 2"。
// 索引层(FolderSummary)只报计数,领域措辞在这一层落定:
// PackCount>0 视为打包作品按"话"措辞,否则"个文件"——对相册/专辑原样适用。
func summaryLine(s index.FolderSummary) string {
	if s.FileCount == 0 {
		return "空"
	}
	var b strings.Builder
	if s.PackCount > 0 {
		fmt.Fprintf(&b, "%d 话", s.PackCount)
		if s.FileCount > s.PackCount {
			fmt.Fprintf(&b, " · %d 个散文件", s.FileCount-s.PackCount)
		}
	} else {
		fmt.Fprintf(&b, "%d 个文件", s.FileCount)
	}
	fmt.Fprintf(&b, " · %s", humanSize(s.TotalSize))
	if s.LatestAt != 0 {
		fmt.Fprintf(&b, " · ← %s", shortTime(s.LatestAt))
	}
	if s.PendingCount > 0 {
		fmt.Fprintf(&b, ",待传 %d", s.PendingCount)
	}
	return b.String()
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// shortTime 一年内省年份,更早带年份——摘要里的"← 08-01"形态。
func shortTime(unix int64) string {
	t := time.Unix(unix, 0)
	if t.Year() == time.Now().Year() {
		return t.Format("01-02")
	}
	return t.Format("2006-01-02")
}

func cmdSearch(args []string) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errs.New(errs.BadConfig, "search 需要关键词")
	}
	db, err := openIndex()
	if err != nil {
		return err
	}
	defer db.Close()
	hits, err := db.Search(fs.Arg(0), 100)
	if err != nil {
		return errs.From(err)
	}
	// 目录命中在前:浏览的决策单元是作品(TODO-16),文件命中跟在后面
	folderHits, err := db.SearchFolders(fs.Arg(0), 100)
	if err != nil {
		return errs.From(err)
	}
	if len(hits) == 0 && len(folderHits) == 0 {
		fmt.Println("(无结果)")
		return nil
	}
	for _, h := range folderHits {
		fmt.Printf("D %d\t%s\t%s\n", h.ID, h.Path, folderMetaLine(h.Note, h.Tags, h.CoverFileID))
	}
	for _, h := range hits {
		note := ""
		if h.Note != "" {
			note = "  备注:" + h.Note
		}
		fmt.Printf("%d\t%s\t%d 字节%s\n", h.ID, h.Path, h.Size, note)
	}
	return nil
}

// folderMetaLine 把目录元数据压成一行(search 目录命中与 meta 共用)。
func folderMetaLine(note string, tags []string, coverFileID int64) string {
	var parts []string
	if len(tags) > 0 {
		parts = append(parts, "tag:"+strings.Join(tags, ","))
	}
	if note != "" {
		parts = append(parts, "备注:"+note)
	}
	if coverFileID != 0 {
		parts = append(parts, fmt.Sprintf("封面:%d", coverFileID))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, "  ")
}

// ---- 目录元数据与移动(TODO-16)----

func cmdMeta(args []string) error {
	if len(args) == 0 {
		return errs.New(errs.BadConfig, "用法:kistctl meta <set <目录路径|文件uuid|id> --note/--tag/--cover(cover 仅目录) | list>")
	}
	switch args[0] {
	case "set":
		return cmdMetaSet(args[1:])
	case "list":
		return cmdMetaList(args[1:])
	default:
		return errs.New(errs.BadConfig, "未知 meta 子命令 "+args[0]+"(可用:set|list)")
	}
}

func cmdMetaSet(args []string) error {
	fs := flag.NewFlagSet("meta set", flag.ContinueOnError)
	note := fs.String("note", "", "备注(空串清除)")
	tag := fs.String("tag", "", "tag 列表,逗号分隔(空串清空全部;全量覆盖语义)")
	cover := fs.String("cover", "", "自定义封面:文件 uuid|id;传 0 清除引用(回退派生拼贴)")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errs.New(errs.BadConfig, "meta set 需要 <目录路径|文件uuid|id>(目录从根写起,如 /作品A)")
	}
	db, err := openIndex()
	if err != nil {
		return err
	}
	defer db.Close()
	// 目标分派(TODO-17):"/" 开头是目录路径,否则视作文件 uuid|id(与 rm/get 同款)
	if target := fs.Arg(0); !strings.HasPrefix(target, "/") {
		return metaSetFile(db, target, fs, note, tag)
	}
	segs, err := splitVirtualPath(fs.Arg(0))
	if err != nil {
		return err
	}
	folderID, err := db.ResolveFolderPath(segs)
	if err != nil {
		return errs.From(err) // 元数据挂在已存在的目录上,不隐式建目录
	}

	// 与 note 命令同款探测:区分"未提供"与"提供了空串"(空串 = 清除)
	var upd index.FolderMetaUpdate
	fs.Visit(func(fl *flag.Flag) {
		switch fl.Name {
		case "note":
			upd.Note = note
		case "tag":
			upd.Tags = parseTagList(*tag)
		}
	})
	if flagProvided(fs, "cover") {
		id, err := resolveCoverRef(db, *cover)
		if err != nil {
			return err
		}
		upd.Cover = &id
	}
	if upd.Note == nil && upd.Tags == nil && upd.Cover == nil {
		// 不带任何 flag:显示当前元数据
		m, err := db.GetFolderMeta(folderID)
		if err != nil {
			return errs.From(err)
		}
		fmt.Printf("%s\t%s\n", virtualPathOf(segs), folderMetaLine(m.Note, m.Tags, m.CoverFileID))
		return nil
	}
	if err := db.UpdateFolderMeta(folderID, upd); err != nil {
		return errs.From(err)
	}
	fmt.Println("已保存")
	return nil
}

// metaSetFile 文件元数据(TODO-17):--note/--tag 语义与目录侧一致;
// --cover 仅目录——文件封面是自身的缩略图行,导入/清除是 GUI 元数据面板的
// 对话框操作,CLI 不设等价 flag(真有需要再加 --cover-file)。
func metaSetFile(db *index.DB, target string, fs *flag.FlagSet, note, tag *string) error {
	f, err := resolveTarget(db, target)
	if err != nil {
		return err
	}
	if f.DeletedAt.Valid {
		return errs.New(errs.NotFound, "文件已删除:"+f.Name)
	}
	if flagProvided(fs, "cover") {
		return errs.New(errs.BadConfig, "--cover 仅用于目录;文件封面的导入/清除请在 GUI 元数据面板操作")
	}
	// 与目录侧同款探测:区分"未提供"与"提供了空串"(空串 = 清除)
	var upd index.FileMetaUpdate
	fs.Visit(func(fl *flag.Flag) {
		switch fl.Name {
		case "note":
			upd.Note = note
		case "tag":
			upd.Tags = parseTagList(*tag)
		}
	})
	if upd.Note == nil && upd.Tags == nil {
		m, err := db.GetFileMeta(f.ID)
		if err != nil {
			return errs.From(err)
		}
		fmt.Printf("%d\t%s\t%s\n", f.ID, f.Name, folderMetaLine(m.Note, m.Tags, 0))
		return nil
	}
	if err := db.UpdateFileMeta(f.ID, upd); err != nil {
		return errs.From(err)
	}
	fmt.Println("已保存")
	return nil
}

func cmdMetaList(args []string) error {
	db, err := openIndex()
	if err != nil {
		return err
	}
	defer db.Close()
	hits, err := db.ListFoldersWithMeta()
	if err != nil {
		return errs.From(err)
	}
	if len(hits) == 0 {
		fmt.Println("(无目录元数据)")
		return nil
	}
	for _, h := range hits {
		fmt.Printf("%d\t%s\t%s\n", h.ID, h.Path, folderMetaLine(h.Note, h.Tags, h.CoverFileID))
	}
	return nil
}

// parseTagList 拆逗号分隔的 tag:去空白、滤空段。
func parseTagList(s string) []string {
	var tags []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			tags = append(tags, t)
		}
	}
	return tags
}

// flagProvided 探测某个 flag 是否被显式提供(--cover 的空值有语义,不能靠默认值判断)。
func flagProvided(fs *flag.FlagSet, name string) bool {
	provided := false
	fs.Visit(func(fl *flag.Flag) {
		if fl.Name == name {
			provided = true
		}
	})
	return provided
}

// resolveCoverRef 把 --cover 的值(uuid|id|0)解析成 files.id;0 表示清除引用。
func resolveCoverRef(db *index.DB, v string) (int64, error) {
	if strings.TrimSpace(v) == "0" {
		return 0, nil
	}
	f, err := resolveTarget(db, v)
	if err != nil {
		return 0, err
	}
	return checkCoverFile(db, f)
}

// checkCoverFile 校验封面引用的文件可用:已软删的拒绝;
// 无缩略图放行但提示——封面第 1 级要求有缩略图,否则渲染端会回退派生拼贴。
func checkCoverFile(db *index.DB, f index.FileRow) (int64, error) {
	if f.DeletedAt.Valid {
		return 0, errs.New(errs.BadConfig, "封面文件已删除:"+f.Name)
	}
	if _, _, err := db.GetThumbnail(f.ID); err != nil {
		fmt.Printf("提示:%s 没有缩略图,封面将回退为派生拼贴\n", f.Name)
	}
	return f.ID, nil
}

// virtualPathOf 由段拼回虚拟路径(splitVirtualPath 的逆,仅展示用)。
func virtualPathOf(segs []string) string {
	if len(segs) == 0 {
		return "/"
	}
	return "/" + strings.Join(segs, "/")
}

func cmdMv(args []string) error {
	fs := flag.NewFlagSet("mv", flag.ContinueOnError)
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return errs.New(errs.BadConfig, "mv 需要 <uuid|id...> <目标目录>(最后一个参数是目标,从根写起)")
	}
	db, err := openIndex()
	if err != nil {
		return err
	}
	defer db.Close()
	targets := fs.Args()[:fs.NArg()-1]
	segs, err := splitVirtualPath(fs.Args()[fs.NArg()-1])
	if err != nil {
		return err
	}
	// 目标目录不存在则逐级建(与 put --dest 同款宽容)
	var destID int64
	if err := db.WithTx(func(tx *sql.Tx) error {
		var err error
		destID, err = db.EnsureFolderPath(tx, 1, segs)
		return err
	}); err != nil {
		return errs.From(err)
	}
	var ids []int64
	for _, t := range targets {
		f, err := resolveTarget(db, t)
		if err != nil {
			return err
		}
		if f.DeletedAt.Valid {
			return errs.New(errs.BadConfig, t+" 已删除,不可移动")
		}
		ids = append(ids, f.ID)
	}
	if err := db.MoveFiles(ids, destID); err != nil {
		return errs.From(err)
	}
	// 重名消解可能改了文件名,重取行打印实际落点
	for _, id := range ids {
		f, err := db.GetFile(id)
		if err != nil {
			return errs.From(err)
		}
		fmt.Printf("已移动 → %s\n", filePathOf(db, f))
	}
	return nil
}

// resolveTarget 把 uuid 或数字 id 解析成索引行。
func resolveTarget(db *index.DB, target string) (index.FileRow, error) {
	if id, err := strconv.ParseInt(target, 10, 64); err == nil {
		f, err := db.GetFile(id)
		if err != nil {
			return index.FileRow{}, errs.From(err)
		}
		return f, nil
	}
	f, err := db.GetFileByUUID(target)
	if err != nil {
		return index.FileRow{}, errs.From(err)
	}
	return f, nil
}

func cmdGet(args []string) error {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	to := fs.String("to", ".", "下载目标目录")
	keepZip := fs.Bool("keep-zip", false, "文件夹条目不解压,直接落 <名>.zip(喂漫画阅读器可改名 .cbz)")
	passStdin := passStdinFlag(fs)
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errs.New(errs.BadConfig, "get 需要 <uuid|id>")
	}
	pass, err := readPass(*passStdin)
	if err != nil {
		return err
	}
	cfg, store, err := loadStore()
	if err != nil {
		return err
	}
	mk, err := unlockMK(store, pass)
	if err != nil {
		return err
	}
	db, err := openIndex()
	if err != nil {
		return err
	}
	defer db.Close()
	f, err := resolveTarget(db, fs.Arg(0))
	if err != nil {
		return err
	}
	if f.State == "uploading" {
		return errs.New(errs.Locked,
			"该文件待上传:先 kistctl outbox push(或手工搬运到远端后 outbox verify)")
	}
	destDir, err := filepath.Abs(*to)
	if err != nil {
		return err
	}
	m := newManager(cfg, store, db, mk)
	var dOpts []transfer.DownloadOptions
	if *keepZip {
		dOpts = append(dOpts, transfer.DownloadOptions{KeepZip: true})
	}
	if _, err := m.DownloadTo(context.Background(), []int64{f.ID}, destDir, dOpts...); err != nil {
		return err
	}
	fmt.Printf("下载到 %s\n", destDir)
	if fails := waitAndReport(m); fails > 0 {
		return errs.New(errs.Internal, "下载失败或被取消")
	}
	return nil
}

func cmdInfo(args []string) error {
	fs := flag.NewFlagSet("info", flag.ContinueOnError)
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errs.New(errs.BadConfig, "info 需要 <uuid|id>")
	}
	db, err := openIndex()
	if err != nil {
		return err
	}
	defer db.Close()
	f, err := resolveTarget(db, fs.Arg(0))
	if err != nil {
		return err
	}
	crumb, err := db.FolderPath(f.FolderID)
	if err != nil {
		return err
	}
	var path strings.Builder
	for _, c := range crumb {
		if c.Name == "" { // 根目录名为空,跳过避免出现 "//"
			continue
		}
		path.WriteString("/")
		path.WriteString(c.Name)
	}
	fmt.Printf("路径:   %s/%s\n", path.String(), f.Name)
	fmt.Printf("UUID:   %s\n", f.UUID)
	fmt.Printf("大小:   %d 字节(密文 %d)\n", f.Size, f.CipherSize)
	fmt.Printf("SHA256: %s\n", f.SHA256)
	fmt.Printf("状态:   %s\n", stateLabel(f.State))
	fmt.Printf("加密于: %s\n", unixOrDash(f.EncryptedAt.Int64, f.EncryptedAt.Valid))
	fmt.Printf("上传于: %s\n", unixOrDash(f.UploadedAt.Int64, f.UploadedAt.Valid))
	fmt.Printf("备注:   %s\n", orDash(f.Note.String, f.Note.Valid))
	if _, _, err := db.GetThumbnail(f.ID); err == nil {
		fmt.Println("缩略图: 有")
	} else {
		fmt.Println("缩略图: 无")
	}
	return nil
}

func unixOrDash(v int64, valid bool) string {
	if !valid || v == 0 {
		return "-"
	}
	return time.Unix(v, 0).Format("2006-01-02 15:04:05")
}

// stateLabel 把文件状态映射为可读标签(TODO-13 的 uploading 首次启用该列)。
func stateLabel(s string) string {
	switch s {
	case "uploading":
		return "待上传"
	case "ready":
		return "就绪"
	case "missing":
		return "远端缺失"
	default:
		return s
	}
}

// filePathOf 拼出文件的完整虚拟路径(供 info 与 outbox list 共用)。
func filePathOf(db *index.DB, f index.FileRow) string {
	crumb, err := db.FolderPath(f.FolderID)
	if err != nil {
		return f.Name
	}
	var b strings.Builder
	for _, c := range crumb {
		if c.Name == "" {
			continue
		}
		b.WriteString("/")
		b.WriteString(c.Name)
	}
	return b.String() + "/" + f.Name
}

func orDash(s string, valid bool) string {
	if !valid || s == "" {
		return "(无)"
	}
	return s
}

func cmdNote(args []string) error {
	fs := flag.NewFlagSet("note", flag.ContinueOnError)
	set := fs.String("set", "", "设置备注(空串清除)")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errs.New(errs.BadConfig, "note 需要 <uuid|id>")
	}
	db, err := openIndex()
	if err != nil {
		return err
	}
	defer db.Close()
	f, err := resolveTarget(db, fs.Arg(0))
	if err != nil {
		return err
	}
	// --set 需区分"未提供"与"提供了空串"(空串 = 清除备注),用 Visit 探测
	setProvided := false
	fs.Visit(func(fl *flag.Flag) {
		if fl.Name == "set" {
			setProvided = true
		}
	})
	if setProvided {
		if err := db.SetNote(f.ID, *set); err != nil {
			return errs.From(err)
		}
		fmt.Println("备注已保存")
		return nil
	}
	fmt.Println(orDash(f.Note.String, f.Note.Valid))
	return nil
}

func cmdRm(args []string) error {
	fs := flag.NewFlagSet("rm", flag.ContinueOnError)
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errs.New(errs.BadConfig, "rm 需要 <uuid|id>...(多个以空格分隔)")
	}
	db, err := openIndex()
	if err != nil {
		return err
	}
	defer db.Close()
	var ids []int64
	var blobNames []string
	for _, target := range fs.Args() {
		f, err := resolveTarget(db, target)
		if err != nil {
			return err
		}
		ids = append(ids, f.ID)
		blobNames = append(blobNames, f.BlobName)
	}
	if err := db.SoftDeleteFiles(ids); err != nil {
		return errs.From(err)
	}
	if err := db.MarkBlobTrash(blobNames); err != nil {
		return errs.From(err)
	}
	fmt.Printf("已软删除 %d 个文件(远端 blob 待 gc 清理)\n", len(ids))
	return nil
}

func cmdGc(args []string) error {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "只报告不删除")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	_, store, err := loadStore()
	if err != nil {
		return err
	}
	db, err := openIndex()
	if err != nil {
		return err
	}
	defer db.Close()
	deleted, orphans, err := transfer.RunGC(context.Background(), store, db, *dryRun)
	if err != nil {
		return errs.From(err)
	}
	if len(deleted) == 0 && len(orphans) == 0 {
		fmt.Println("无需清理")
		return nil
	}
	if *dryRun {
		fmt.Printf("试运行:将删除 %d 个 trash blob,发现 %d 个孤儿\n", len(deleted), len(orphans))
	} else if len(deleted) > 0 {
		fmt.Printf("已删除 %d 个 trash blob\n", len(deleted))
	}
	for _, n := range orphans {
		fmt.Printf("孤儿(远端有、索引无,未删除): %s\n", n)
	}
	return nil
}

func cmdBackup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	passStdin := passStdinFlag(fs)
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	pass, err := readPass(*passStdin)
	if err != nil {
		return err
	}
	_, store, err := loadStore()
	if err != nil {
		return err
	}
	mk, err := unlockMK(store, pass)
	if err != nil {
		return err
	}
	db, err := openIndex()
	if err != nil {
		return err
	}
	defer db.Close()
	info, err := backup.BackupNow(context.Background(), mk, db, store)
	if err != nil {
		return errs.From(err)
	}
	fmt.Printf("备份完成:revision %d,加密后 %d 字节,%s\n",
		info.Revision, info.Size, info.At.Format("2006-01-02 15:04:05"))
	return nil
}

func cmdPull(args []string) error {
	fs := flag.NewFlagSet("pull", flag.ContinueOnError)
	passStdin := passStdinFlag(fs)
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	pass, err := readPass(*passStdin)
	if err != nil {
		return err
	}
	_, store, err := loadStore()
	if err != nil {
		return err
	}
	// 本地无 keyfile 时 unlockMK 内部会自动从远端拉取并缓存(新设备路径)
	mk, err := unlockMK(store, pass)
	if err != nil {
		return err
	}
	db, err := openIndex()
	if err != nil {
		return err
	}
	defer db.Close()
	res, err := backup.PullRemote(context.Background(), mk, store, db)
	if err != nil {
		return errs.From(err)
	}
	switch res.Action {
	case "replaced":
		fmt.Printf("已从远端恢复索引(远端 revision %d > 本地 %d,来自设备 %s);旧库已归档在 backups/ 目录\n",
			res.RemoteRev, res.LocalRev, res.RemoteDevice)
	case "noop":
		fmt.Printf("本地与远端同版本(revision %d),无需恢复\n", res.LocalRev)
	case "local-newer":
		fmt.Printf("本地索引(revision %d)比远端(%d)新,未改动本地;建议先 kistctl backup 推送\n",
			res.LocalRev, res.RemoteRev)
	}
	return nil
}

// cmdMigrate 网盘间纯密文迁移(TODO-12):不解锁、不触碰明文,可断点续跑。
func cmdMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	url := fs.String("url", "", "新网盘 WebDAV 地址(如 https://dav.example.com/dav)")
	user := fs.String("user", "", "新网盘用户名")
	root := fs.String("root", "", "新网盘根目录(默认 /kist)")
	passStdin := fs.Bool("pass-stdin", false, "从 stdin 读一行新网盘的 WebDAV 密码")
	switchTo := fs.Bool("switch", false, "迁移验证通过后把 config 直接切换到新网盘")
	conc := fs.Int("concurrency", 2, "搬运并发(1–4)")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	if *url == "" || *user == "" {
		return errs.New(errs.BadConfig, "--url 与 --user 均为必填")
	}
	var pass string
	if *passStdin {
		pw, err := readPass(true) // 这里的口令是新网盘的 WebDAV 密码
		if err != nil {
			return err
		}
		pass = pw
	}
	dstRoot := *root
	if dstRoot == "" {
		dstRoot = "/kist"
	}
	_, srcStore, err := loadStore()
	if err != nil {
		return err
	}
	dstC, err := dav.New(dav.Config{URL: *url, Username: *user, Password: pass, RootPath: dstRoot})
	if err != nil {
		return errs.Wrap(errs.BadConfig, err)
	}
	res, err := migrate.Run(context.Background(), migrate.Options{
		Src: srcStore, Dst: dstC, DstRoot: dstRoot, Concurrency: *conc,
	})
	if err != nil {
		return errs.Wrap(errs.DavError, err)
	}
	fmt.Printf("迁移完成:复制 %d 个(%.1f MiB),跳过 %d 个(断点已完成),失败 %d 个\n",
		res.Copied, float64(res.Bytes)/(1<<20), res.Skipped, res.Failed)
	for _, f := range res.Failures {
		fmt.Printf("  ✗ %s:%s(重跑 migrate 会自动重试)\n", f.Name, f.Err)
	}
	if res.NoIndexBackup {
		fmt.Println("注意:源端从未 backup(无 index.enc),新端需尽快 backup 一次才能被其他设备 pull")
	}
	if res.Failed > 0 {
		return errs.New(errs.Internal, fmt.Sprintf("%d 个对象迁移失败,keyfile/index.enc 校验未通过,不可 --switch", res.Failed))
	}
	fmt.Println("校验通过:keyfile/index.enc 双端哈希一致")
	if *switchTo {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		cfg.URL = *url
		cfg.Username = *user
		cfg.RootPath = dstRoot
		if pass != "" {
			cfg.Password = pass
			cfg.Settings.RememberPassword = true
		} else {
			// 旧密码属于旧网盘,不能带去新端
			cfg.Password = ""
			cfg.Settings.RememberPassword = false
		}
		if err := config.Save(cfg); err != nil {
			return err
		}
		fmt.Printf("已切换到新网盘(%s,%s)。本地 keyfile 与 index.db 无需变动;建议尽快 backup 一次\n",
			*url, dstRoot)
		return nil
	}
	fmt.Println("未切换:确认无误后可手改 config,或带 --switch 重跑(已完成对象自动跳过,秒级收尾)")
	return nil
}
