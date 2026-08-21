// pack.go 实现目录打包(TODO-15):put 遇文件夹默认压 zip 成单个加密
// 对象,get 解压还原成文件夹。粒度规则唯一判据是"有无子目录",不看
// 内容、不看名字:
//   - put 根自身是叶子(无子目录且有文件)→ 整根一个 pack,直挂 dest;
//   - 含子目录 → 逐层下降,任何叶子目录(无子目录)各自成 pack;
//   - 混杂层(散文件 + 子目录并存)的散文件按普通文件各自入库;
//   - 空目录保留为虚拟目录(与 --expand 行为一致)。
//
// "整部一个包"是被否决的粒度(补一话即重传整部),规则保证不会发生。
package transfer

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"kist/internal/errs"
)

// packPlan 是 put 根目录的解析结果。
type packPlan struct {
	RootIsPack  bool       // 根自身即叶子:整根一个 pack,直挂 dest
	RootPack    *packRoot  // RootIsPack 时的根 pack(RelSegs 为空)
	Packs       []packRoot // 根以下的叶子目录,各成一个 pack
	Loose       []looseFile
	VirtualDirs [][]string // 需在索引建的虚拟目录(相对 put 根,不含根名)
}

type packRoot struct {
	AbsDir     string
	Name       string
	RelSegs    []string // 相对 put 根的自身目录段;根 pack 为空
	Files      int
	Bytes      int64
	Mtime      int64
	FirstImage string // 词法序第一张图(封面候选);无图为空
}

type looseFile struct {
	AbsPath string
	Name    string
	RelSegs []string // 相对 put 根,末段为文件名
	Mtime   int64
	Bytes   int64
}

// parentSegs 返回目录段(末段是条目自身时取其父)。
func parentSegs(segs []string) []string {
	if len(segs) <= 1 {
		return nil
	}
	return segs[:len(segs)-1]
}

// planPackTree 解析 put 根,产出打包计划。校验遍先收齐全部问题再统一
// 报错,整次 put 拒绝——存储器不静默丢东西/改东西,也不留半套索引:
//   - 非 UTF-8 文件名(如日漫 Shift-JIS 原始档,跨系统解压必乱码);
//   - symlink / FIFO / socket / 设备文件(zip 存不下,静默跳过即丢数据)。
func planPackTree(root string) (packPlan, error) {
	var plan packPlan
	type dirStat struct {
		mtime   int64
		hasSub  bool
		hasFile bool
		isPack  bool
	}
	dirs := map[string]*dirStat{".": {}}
	type fileStat struct {
		abs   string
		rel   string // slash 相对路径
		mtime int64
		bytes int64
	}
	var files []fileStat
	var bad []string

	if fi, err := os.Stat(root); err != nil {
		return plan, err
	} else {
		dirs["."].mtime = fi.ModTime().Unix()
	}

	err := filepath.WalkDir(root, func(fp string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, fp)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if !utf8.ValidString(d.Name()) {
			bad = append(bad, fmt.Sprintf("%s:文件名不是合法 UTF-8(可先 convmv -f <编码> -t utf8 -r --notest 转码)", fp))
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		slash := filepath.ToSlash(rel)
		if d.IsDir() {
			dirs[slash] = &dirStat{mtime: fi.ModTime().Unix()}
			dirs[path.Dir(slash)].hasSub = true
			return nil
		}
		if !fi.Mode().IsRegular() {
			bad = append(bad, fmt.Sprintf("%s:非常规文件(%s);symlink/特殊文件不打包", fp, fi.Mode().Type()))
			return nil
		}
		dirs[path.Dir(slash)].hasFile = true
		files = append(files, fileStat{abs: fp, rel: slash, mtime: fi.ModTime().Unix(), bytes: fi.Size()})
		return nil
	})
	if err != nil {
		return plan, fmt.Errorf("transfer: 解析 %s: %w", root, err)
	}
	if len(bad) > 0 {
		return plan, errs.New(errs.BadConfig,
			"put 目录包含不支持的条目,整次拒绝:\n  "+strings.Join(bad, "\n  "))
	}

	for rel, di := range dirs {
		if rel != "." && !di.hasSub && di.hasFile {
			di.isPack = true
		}
	}
	if !dirs["."].hasSub && dirs["."].hasFile {
		plan.RootIsPack = true
		rp := &packRoot{AbsDir: root, Name: filepath.Base(root), Mtime: dirs["."].mtime}
		dirs["."].isPack = true
		plan.RootPack = rp
	}

	// 文件归属:直接父目录是 pack 根 → 计入该 pack;否则为散文件
	packByDir := map[string]*packRoot{}
	for rel, di := range dirs {
		if !di.isPack {
			continue
		}
		packByDir[rel] = &packRoot{AbsDir: filepath.Join(root, filepath.FromSlash(rel)),
			Name: path.Base(rel), RelSegs: splitSegments(rel), Mtime: di.mtime}
	}
	if plan.RootPack != nil {
		packByDir["."] = plan.RootPack
	}
	for _, f := range files {
		if pr, ok := packByDir[path.Dir(f.rel)]; ok {
			pr.Files++
			pr.Bytes += f.bytes
			if pr.FirstImage == "" && isImagePath(f.rel) {
				pr.FirstImage = f.abs
			}
			continue
		}
		plan.Loose = append(plan.Loose, looseFile{AbsPath: f.abs, Name: path.Base(f.rel),
			RelSegs: splitSegments(f.rel), Mtime: f.mtime, Bytes: f.bytes})
	}
	for rel, pr := range packByDir {
		if rel == "." {
			continue
		}
		plan.Packs = append(plan.Packs, *pr)
	}

	// 非叶子目录全部建虚拟目录(pack 根自身除外;空目录也在其中,得以保留)
	for rel, di := range dirs {
		if rel == "." || di.isPack {
			continue
		}
		plan.VirtualDirs = append(plan.VirtualDirs, splitSegments(rel))
	}

	sortPackPlan(&plan)
	return plan, nil
}

// sortPackPlan 让输出次序确定(map 迭代序随机),测试与目录创建都依赖。
func sortPackPlan(p *packPlan) {
	sort.Slice(p.Packs, func(i, j int) bool {
		return joinSegments(p.Packs[i].RelSegs) < joinSegments(p.Packs[j].RelSegs)
	})
	sort.Slice(p.Loose, func(i, j int) bool {
		return joinSegments(p.Loose[i].RelSegs) < joinSegments(p.Loose[j].RelSegs)
	})
	sort.Slice(p.VirtualDirs, func(i, j int) bool {
		return joinSegments(p.VirtualDirs[i]) < joinSegments(p.VirtualDirs[j])
	})
}

// isImagePath 按扩展名挑封面候选(makeThumbnail 会再嗅探内容真伪)。
func isImagePath(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".bmp", ".webp":
		return true
	}
	return false
}

// packUserMeta 序列化 pack 条目的原目录规模(user_meta 扩展位)。
func packUserMeta(origBytes int64, entries int) string {
	b, err := json.Marshal(map[string]any{"orig_size": origBytes, "entries": entries})
	if err != nil { // int/int64 的 Marshal 不可能失败,防御性兜底
		return "{}"
	}
	return string(b)
}

// writePackZip 把 dir 整体以 zip 写入 w:zip.NewWriter 直挂 BlobWriter,
// 单遍流式,内存 ≈ 一个块。条目全部用 Store(主载体 jpg/png 已压缩,
// 无收益,省 CPU);每个目录写显式条目,空目录得以往返保留。
// 返回(文件数, 原始字节, 首张图片路径)。取消点在每个读取块。
func writePackZip(ctx context.Context, w io.Writer, dir string, prog func(int64)) (int, int64, string, error) {
	zw := zip.NewWriter(w)
	var entries int
	var bytes int64
	firstImg := ""
	err := filepath.WalkDir(dir, func(fp string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, fp)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if d.IsDir() {
			h := &zip.FileHeader{Name: name + "/", Method: zip.Store, Modified: fi.ModTime()}
			_, err := zw.CreateHeader(h)
			return err
		}
		if !fi.Mode().IsRegular() {
			// planPackTree 已拒绝非常规文件,此处防御性再拦
			return fmt.Errorf("pack: %s 非常规文件", fp)
		}
		h := &zip.FileHeader{Name: name, Method: zip.Store, Modified: fi.ModTime()}
		ew, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		entries++
		if firstImg == "" && isImagePath(name) {
			firstImg = fp
		}
		src, err := os.Open(fp)
		if err != nil {
			return err
		}
		defer src.Close()
		buf := make([]byte, 1<<20)
		for {
			if err := ctx.Err(); err != nil { // 取消点:每 MiB
				return err
			}
			n, rerr := src.Read(buf)
			if n > 0 {
				if _, werr := ew.Write(buf[:n]); werr != nil {
					return werr
				}
				bytes += int64(n)
				if prog != nil {
					prog(bytes)
				}
			}
			if rerr == io.EOF {
				return nil
			}
			if rerr != nil {
				return rerr
			}
		}
	})
	if err != nil {
		return 0, 0, "", err
	}
	if err := zw.Close(); err != nil {
		return 0, 0, "", err
	}
	return entries, bytes, firstImg, nil
}

// extractZipTree 把 zipPath 的内容解压到 targetDir(须已存在)。
// 断言(纵深防御,自家 zip 不应触发,触发即报错):
//   - 条目名必须合法 UTF-8(解压到 Windows 按代码页猜必乱码,源头拒绝);
//   - 条目不得逃逸 targetDir(zip-slip 同套检查)。
//
// 条目 mtime 尽力还原(os.Chtimes,失败不阻断);目录自身 mtime 不保证。
// 取消点:每条目、每读取块。失败时调用方负责清理已解出的半截目录。
func extractZipTree(ctx context.Context, zipPath, targetDir string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("pack: 读取 zip 失败: %w", err)
	}
	defer zr.Close()
	for _, zf := range zr.File {
		if err := ctx.Err(); err != nil { // 取消点:每条目
			return err
		}
		name := zf.Name
		if !utf8.ValidString(name) {
			return fmt.Errorf("pack: zip 条目名不是合法 UTF-8:%q", name)
		}
		clean := filepath.Clean(filepath.FromSlash(name))
		if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) {
			return fmt.Errorf("pack: zip 条目逃逸目标目录:%q", name)
		}
		target := filepath.Join(targetDir, clean)
		if zf.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		if err := copyZipEntry(ctx, zf, target); err != nil {
			return err
		}
		if mt := zf.Modified; !mt.IsZero() {
			_ = os.Chtimes(target, mt, mt) // mtime 尽力还原
		}
	}
	return nil
}

// copyZipEntry 解压单个文件条目到 target(0600,同 .kistpart 的保守权限)。
func copyZipEntry(ctx context.Context, zf *zip.File, target string) (err error) {
	rc, err := zf.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	buf := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil { // 取消点:每 MiB
			return err
		}
		n, rerr := rc.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}
