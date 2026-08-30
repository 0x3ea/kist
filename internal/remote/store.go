package remote

import (
	"context"
	"io"
	"os"
	"sync/atomic"

	"kist/internal/dav"
)

// 远端保留名:根目录下除随机名 blob 外仅有的两个对象。
const (
	KeyFileName = "keyfile"
	IndexName   = "index.enc"
)

// CoversDir 是封面 blob 的子命名空间(TODO-10):/kist/covers/<32hex>。
// 有意破例于"根目录永远扁平"——封面把远端对象数翻倍,单列主命名空间
// 会让 gc/migrate 的全列成本跟着翻倍;分目录后主命名空间全列不变,且
// covers 目录里的孤儿可判定为"封面孤儿"单独标注。目录名与 keyfile/
// index.enc 同为语义名,对象名仍随机 32hex,不泄露任何明文信息。
const CoversDir = "covers"

// Store 赋予远端对象语义:网盘根目录(/kist)下只有随机名(32hex)的
// 加密 blob 与 keyfile、index.enc 两个保留名,加上 covers/ 封面子命名
// 空间(TODO-10),不携带任何明文信息。
type Store struct {
	c             dav.Client
	root          string
	coversEnsured atomic.Bool // EnsureCoversRoot 记忆化:省掉每张封面一次 MKCOL
}

// NewStore 构造远端语义层;rootPath 为 dav 层同款根目录(默认 /kist)。
func NewStore(c dav.Client, rootPath string) *Store {
	if rootPath == "" {
		rootPath = "/kist"
	}
	return &Store{c: c, root: rootPath}
}

func (s *Store) path(name string) string { return s.root + "/" + name }

func (s *Store) coverPath(name string) string { return s.root + "/" + CoversDir + "/" + name }

// EnsureReady 初始化远端根目录(MKCOL,幂等)。首次使用时调用。
func (s *Store) EnsureReady(ctx context.Context) error { return s.c.EnsureRoot(ctx) }

// EnsureCoversRoot 初始化封面命名空间;进程内记忆化(成功一次后不再发
// MKCOL)。SetRemote 换的是新 Store 实例,记忆位自然重置。
// 先确保父根:部分服务端对"父目录缺失"的 MKCOL 回 409 而非自建。
func (s *Store) EnsureCoversRoot(ctx context.Context) error {
	if s.coversEnsured.Load() {
		return nil
	}
	if err := s.c.EnsureDir(ctx, s.root); err != nil {
		return err
	}
	if err := s.c.EnsureDir(ctx, s.root+"/"+CoversDir); err != nil {
		return err
	}
	s.coversEnsured.Store(true)
	return nil
}

// Ping 验证地址与凭据可用(GUI"测试连接"与 config set 共用)。
func (s *Store) Ping(ctx context.Context) error { return s.c.Ping(ctx) }

// KeyFileExists 检查远端是否已初始化(有 keyfile 即视为已建账户)。
// PROPFIND Depth 0 精确探测,代价 O(1):原实现列整个 /kist 再查成员,
// 处在 init/新设备检测路径上,代价随库规模线性增长(TODO-11)。
func (s *Store) KeyFileExists(ctx context.Context) (bool, error) {
	found, _, err := s.c.Probe(ctx, s.path(KeyFileName))
	return found, err
}

// ProbeBlob 探测远端 blob 是否存在与服务器报告的大小;size 为 -1 表示
// 服务器未提供(outbox verify 据此决定是否核对大小)。
func (s *Store) ProbeBlob(ctx context.Context, name string) (bool, int64, error) {
	return s.c.Probe(ctx, s.path(name))
}

// PutKeyFile 上传 keyfile(110 字节小文件,走临时文件以复用定长 PUT)。
func (s *Store) PutKeyFile(ctx context.Context, b []byte) error {
	tmp, err := os.CreateTemp("", "kist-keyfile-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	return s.c.PutFile(ctx, s.path(KeyFileName), f, nil)
}

// GetKeyFile 拉取远端 keyfile(新设备恢复入口)。
func (s *Store) GetKeyFile(ctx context.Context) ([]byte, error) {
	tmp, err := os.CreateTemp("", "kist-keyfile-*")
	if err != nil {
		return nil, err
	}
	name := tmp.Name()
	tmp.Close()
	defer os.Remove(name)
	if _, err := s.c.GetToFile(ctx, s.path(KeyFileName), name, nil); err != nil {
		return nil, err
	}
	return os.ReadFile(name)
}

// PutBlob 上传随机名加密 blob;prog 以密文累计字节回调。
func (s *Store) PutBlob(ctx context.Context, name string, f *os.File, prog func(int64)) error {
	return s.c.PutFile(ctx, s.path(name), f, prog)
}

// GetBlob 下载 blob 到本地临时路径。
func (s *Store) GetBlob(ctx context.Context, name, tmpPath string, prog func(int64)) error {
	_, err := s.c.GetToFile(ctx, s.path(name), tmpPath, prog)
	return err
}

// ListBlobs 列出远端主命名空间全部数据 blob(排除 keyfile、index.enc 与
// covers/ 目录条目——封面命名空间由 ListCoverBlobs 单独枚举)。
func (s *Store) ListBlobs(ctx context.Context) ([]string, error) {
	objs, err := s.c.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(objs))
	for _, o := range objs {
		if o.IsDir || o.Name == KeyFileName || o.Name == IndexName {
			continue
		}
		out = append(out, o.Name)
	}
	return out, nil
}

// ListAll 列出根目录下全部对象,含 keyfile 与 index.enc 两个保留名
// (migrate 枚举用;ListBlobs 排除保留名,面向 gc)。
func (s *Store) ListAll(ctx context.Context) ([]dav.RemoteObject, error) {
	return s.c.List(ctx)
}

// GetBlobBody 流式打开远端对象的 GET 响应体;调用方负责 Close。
// 单次尝试不重试,重传由调用方在整对象粒度发起(migrate)。
func (s *Store) GetBlobBody(ctx context.Context, name string) (io.ReadCloser, error) {
	return s.c.GetBody(ctx, s.path(name))
}

// DeleteBlob 删除远端 blob(孤儿清理/trash 落地时调用)。
func (s *Store) DeleteBlob(ctx context.Context, name string) error {
	return s.c.Delete(ctx, s.path(name))
}

// ---- 封面命名空间(TODO-10):/kist/covers/<32hex>,与主命名空间同语义 ----

// PutCoverBlob 上传封面 blob。首次调用前确保 covers 目录存在;目录被外力
// 删除(网盘侧整理)导致的 409/404 类冲突,清记忆位后重试一次。
func (s *Store) PutCoverBlob(ctx context.Context, name string, f *os.File, prog func(int64)) error {
	if err := s.EnsureCoversRoot(ctx); err != nil {
		return err
	}
	err := s.c.PutFile(ctx, s.coverPath(name), f, prog)
	if err == nil {
		return nil
	}
	s.coversEnsured.Store(false)
	if err2 := s.EnsureCoversRoot(ctx); err2 != nil {
		return err
	}
	return s.c.PutFile(ctx, s.coverPath(name), f, prog)
}

// GetCoverBlob 下载封面 blob 到本地临时路径。
func (s *Store) GetCoverBlob(ctx context.Context, name, tmpPath string, prog func(int64)) error {
	_, err := s.c.GetToFile(ctx, s.coverPath(name), tmpPath, prog)
	return err
}

// ProbeCoverBlob 探测封面 blob 是否存在与大小(verify 收账用)。
func (s *Store) ProbeCoverBlob(ctx context.Context, name string) (bool, int64, error) {
	return s.c.Probe(ctx, s.coverPath(name))
}

// DeleteCoverBlob 删除封面 blob(gc trash 落地时调用)。
func (s *Store) DeleteCoverBlob(ctx context.Context, name string) error {
	return s.c.Delete(ctx, s.coverPath(name))
}

// ListCoverBlobs 列出封面命名空间全部对象(含大小,migrate 断点跳过用);
// 目录不存在视为空(尚未上传过封面的库,gc 不应为此报错)。
func (s *Store) ListCoverBlobs(ctx context.Context) ([]dav.RemoteObject, error) {
	found, _, err := s.c.Probe(ctx, s.root+"/"+CoversDir)
	if err != nil {
		return nil, err
	}
	if !found {
		return []dav.RemoteObject{}, nil
	}
	objs, err := s.c.ListDir(ctx, s.root+"/"+CoversDir)
	if err != nil {
		return nil, err
	}
	out := make([]dav.RemoteObject, 0, len(objs))
	for _, o := range objs {
		if o.IsDir {
			continue
		}
		out = append(out, o)
	}
	return out, nil
}

// GetCoverBlobBody 流式打开封面 blob 的 GET 响应体;语义同 GetBlobBody
// (单次尝试不重试,migrate 整对象重试)。
func (s *Store) GetCoverBlobBody(ctx context.Context, name string) (io.ReadCloser, error) {
	return s.c.GetBody(ctx, s.coverPath(name))
}

// PutIndexBlob 上传加密索引备份(远端固定名 index.enc)。
func (s *Store) PutIndexBlob(ctx context.Context, f *os.File) error {
	return s.c.PutFile(ctx, s.path(IndexName), f, nil)
}

// GetIndexBlob 下载索引备份到本地路径;从未备份过时返回 404 类错误。
func (s *Store) GetIndexBlob(ctx context.Context, tmpPath string) error {
	_, err := s.c.GetToFile(ctx, s.path(IndexName), tmpPath, nil)
	return err
}
