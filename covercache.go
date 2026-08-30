package main

// 封面磁盘缓存(TODO-10):解密后的封面图片按 <file uuid>.<ext> 存于
// KIST_HOME/covers/,GetCover 命中即零网络返回。键用 uuid 而非数字
// fileID——TODO-21 多盘共一个 KIST_HOME,不同盘的 fileID 会撞。
// 预算 Settings.CoverCacheMB,超限按 mtime 淘汰(刚写入的最新,天然最后)。
// 与索引库同处 HomeDir(0700):封面明文本就是索引数据的一部分,出库前
// 住在 index.db 里,安全语义不变。

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"kist/internal/config"
	"kist/internal/crypto"
	"kist/internal/remote"
)

// coverMu 串行化缓存写与 LRU 扫描:GetCover 会被网格并发触发,
// tmp+rename 写入本身原子,这里只防并发淘汰互相踩。
var coverMu sync.Mutex

// extForMime 由 mime 定缓存扩展名;未知类型落 .bin(内容不透明,仅取 mime
// 时以 covers 行为准,扩展名只是缓存文件的可读性点缀)。
func extForMime(mime string) string {
	switch mime {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	default:
		return ".bin"
	}
}

// fetchCoverBlob 从 covers 命名空间拉取封面并解密:BlobReader 打开即做
// 长度总校验(cipherSize 取 covers 行记录的密文大小),EOF 终检 + SHA 由
// ReadAll 走完即通过——校验不过不落缓存。
func fetchCoverBlob(ctx context.Context, store *remote.Store, mk crypto.MasterKey,
	blobName string, cipherSize int64) ([]byte, error) {

	tmp, err := os.CreateTemp("", "kist-getcover-*")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpName)
	if err := store.GetCoverBlob(ctx, blobName, tmpName, nil); err != nil {
		return nil, err
	}
	f, err := os.Open(tmpName)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	br, err := crypto.NewBlobReader(f, cipherSize, mk)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(br)
}

// readCoverCache 读缓存条目;未命中返回错误(调用方走远端)。
func readCoverCache(key string) ([]byte, error) {
	coverMu.Lock()
	defer coverMu.Unlock()
	p := filepath.Join(config.CoversCacheDir(), key)
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	// 访问即 touch:更新 mtime 供 LRU 判"最近使用"
	now := time.Now()
	_ = os.Chtimes(p, now, now)
	return data, nil
}

// writeCoverCache 原子写入(tmp + rename),目录按需创建。
func writeCoverCache(key string, data []byte) {
	coverMu.Lock()
	defer coverMu.Unlock()
	dir := config.CoversCacheDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	final := filepath.Join(dir, key)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	_ = os.Rename(tmp.Name(), final)
}

// removeCoverCache 删除文件的封面缓存条目(任意扩展名;换封面后防旧图复活)。
func removeCoverCache(uuid string) {
	coverMu.Lock()
	defer coverMu.Unlock()
	ents, err := os.ReadDir(config.CoversCacheDir())
	if err != nil {
		return
	}
	for _, e := range ents {
		if filepath.Ext(e.Name()) != "" && trimExt(e.Name()) == uuid {
			_ = os.Remove(filepath.Join(config.CoversCacheDir(), e.Name()))
		}
	}
}

func trimExt(name string) string {
	ext := filepath.Ext(name)
	return name[:len(name)-len(ext)]
}

// enforceCoverCacheLRU 超预算按 mtime 升序淘汰(最旧先删)。
// 配置读取须经 a.mu(GetCover 不持 a.mu 调用本函数,无锁序问题)。
func (a *App) enforceCoverCacheLRU() {
	budget := int64(512) << 20
	a.mu.Lock()
	if a.cfg != nil && a.cfg.Settings.CoverCacheMB > 0 {
		budget = int64(a.cfg.Settings.CoverCacheMB) << 20
	}
	a.mu.Unlock()

	coverMu.Lock()
	defer coverMu.Unlock()
	dir := config.CoversCacheDir()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type entry struct {
		path  string
		size  int64
		mtime int64
	}
	var files []entry
	var total int64
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, entry{filepath.Join(dir, e.Name()), info.Size(), info.ModTime().UnixNano()})
		total += info.Size()
	}
	if total <= budget {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mtime < files[j].mtime })
	for _, f := range files {
		if total <= budget {
			return
		}
		if os.Remove(f.path) == nil {
			total -= f.size
		}
	}
}
