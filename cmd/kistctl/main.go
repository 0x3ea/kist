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
	"kist/internal/remote"
	"kist/internal/transfer"
)

const usageText = `用法:kistctl <子命令> [参数]

子命令:
  config set --url <URL> --user <用户名> [--root /kist] [--pass-stdin]  配置网盘
  init    --pass-stdin                  建账户:主密钥+keyfile+远端初始化
  unlock  --pass-stdin                  校验口令(本地 keyfile 优先,无则拉远端)
  put     <路径...> [--dest /目录] --pass-stdin   加密上传(文件夹递归)
  ls      [/路径]                        列虚拟目录
  search  <关键词>                       搜索文件名与备注
  get     <uuid|id> --to <目录> --pass-stdin      下载解密
  info    <uuid|id>                      查看明细(时间/备注/缩略图)
  note    <id> [--set 文本]              查看/设置备注
  rm      <id...>                        软删除文件
  gc      [--dry-run]                    清理 trash blob、报告孤儿
  backup  --pass-stdin                   加密备份索引到远端 index.enc
  pull    --pass-stdin                   从远端恢复索引(新设备/多设备同步)`

func main() {
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
	case "ls":
		err = cmdLs(rest)
	case "search":
		err = cmdSearch(rest)
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

// splitVirtualPath 把 "/a/b" 拆成 ["a","b"];"/" 与 "" 返回空。
func splitVirtualPath(p string) []string {
	var segs []string
	for _, s := range strings.Split(strings.TrimSpace(p), "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	return segs
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
		Emit:        emit,
	})
}

// waitAndReport 等全部传输结束并汇总;返回失败/取消数。
func waitAndReport(m *transfer.Manager) int {
	for !m.Idle() {
		time.Sleep(100 * time.Millisecond)
	}
	fails := 0
	for _, tr := range m.Snapshot() {
		switch tr.Phase {
		case transfer.PhaseDone:
			fmt.Printf("  ✓ %s\n", tr.Name)
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

	segs := splitVirtualPath(*dest)
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
	n, err := m.UploadPaths(context.Background(), paths, folderID)
	if err != nil {
		return err
	}
	fmt.Printf("已入队 %d 个文件\n", n)
	if fails := waitAndReport(m); fails > 0 {
		return errs.New(errs.Internal, fmt.Sprintf("%d 个传输失败或被取消", fails))
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
	folderID, err := db.ResolveFolderPath(splitVirtualPath(path))
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
	for _, e := range entries {
		if e.IsFolder {
			fmt.Printf("D %s/\n", e.Name)
		} else {
			fmt.Printf("F %s\t%d 字节\n", e.Name, e.Size)
		}
	}
	return nil
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
	if len(hits) == 0 {
		fmt.Println("(无结果)")
		return nil
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
	destDir, err := filepath.Abs(*to)
	if err != nil {
		return err
	}
	m := newManager(cfg, store, db, mk)
	if _, err := m.DownloadTo(context.Background(), []int64{f.ID}, destDir); err != nil {
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
	fmt.Printf("状态:   %s\n", f.State)
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
