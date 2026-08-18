package remote

import (
	"context"
	"os"

	"kist/internal/dav"
)

// 远端保留名:根目录下除随机名 blob 外仅有的两个对象。
const (
	KeyFileName = "keyfile"
	IndexName   = "index.enc"
)

// Store 赋予远端对象语义:网盘根目录(/kist)下只有随机名(32hex)的
// 加密 blob 与 keyfile、index.enc 两个保留名,不携带任何明文信息。
type Store struct {
	c    dav.Client
	root string
}

// NewStore 构造远端语义层;rootPath 为 dav 层同款根目录(默认 /kist)。
func NewStore(c dav.Client, rootPath string) *Store {
	if rootPath == "" {
		rootPath = "/kist"
	}
	return &Store{c: c, root: rootPath}
}

func (s *Store) path(name string) string { return s.root + "/" + name }

// EnsureReady 初始化远端根目录(MKCOL,幂等)。首次使用时调用。
func (s *Store) EnsureReady(ctx context.Context) error { return s.c.EnsureRoot(ctx) }

// Ping 验证地址与凭据可用(GUI"测试连接"与 config set 共用)。
func (s *Store) Ping(ctx context.Context) error { return s.c.Ping(ctx) }

// KeyFileExists 检查远端是否已初始化(有 keyfile 即视为已建账户)。
func (s *Store) KeyFileExists(ctx context.Context) (bool, error) {
	objs, err := s.c.List(ctx)
	if err != nil {
		return false, err
	}
	for _, o := range objs {
		if o.Name == KeyFileName {
			return true, nil
		}
	}
	return false, nil
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

// ListBlobs 列出远端全部数据 blob(排除 keyfile 与 index.enc)。
func (s *Store) ListBlobs(ctx context.Context) ([]string, error) {
	objs, err := s.c.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(objs))
	for _, o := range objs {
		if o.Name == KeyFileName || o.Name == IndexName {
			continue
		}
		out = append(out, o.Name)
	}
	return out, nil
}

// DeleteBlob 删除远端 blob(孤儿清理/trash 落地时调用)。
func (s *Store) DeleteBlob(ctx context.Context, name string) error {
	return s.c.Delete(ctx, s.path(name))
}
