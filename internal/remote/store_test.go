package remote

// TODO-10 封面命名空间的往返测试:真起一个内存 WebDAV(x/net/webdav),
// 验证 /kist/covers/ 子目录的上传/探测/下载/删除/枚举与主命名空间互不干扰。

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/net/webdav"

	"kist/internal/dav"
)

const testHexName = "0123456789abcdef0123456789abcdef"

func newTestStore(t *testing.T) *Store {
	t.Helper()
	h := &webdav.Handler{
		FileSystem: webdav.Dir(t.TempDir()),
		LockSystem: webdav.NewMemLS(),
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := dav.New(dav.Config{URL: srv.URL, Username: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	s := NewStore(c, "")
	if err := s.EnsureReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func tempBlob(t *testing.T, data []byte) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "kist-blob-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestCoversNamespaceRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	data := []byte("cover-bytes-测试")

	// 主命名空间放一个文件 blob,验证两命名空间互不串
	if err := s.PutBlob(ctx, testHexName, tempBlob(t, []byte("file-bytes")), nil); err != nil {
		t.Fatal(err)
	}
	// covers 目录尚不存在时 ListCoverBlobs 应视为空,而非报错
	if names, err := s.ListCoverBlobs(ctx); err != nil || len(names) != 0 {
		t.Fatalf("无 covers 目录应返回空: %v %v", names, err)
	}

	if err := s.PutCoverBlob(ctx, testHexName, tempBlob(t, data), nil); err != nil {
		t.Fatal(err)
	}
	// 记忆化后的第二次上传不再 MKCOL,应照常成功
	if err := s.PutCoverBlob(ctx, testHexName+"0", tempBlob(t, data), nil); err != nil {
		t.Fatal(err)
	}

	main, err := s.ListBlobs(ctx)
	if err != nil || len(main) != 1 || main[0] != testHexName {
		t.Fatalf("主命名空间不应含封面: %v %v", main, err)
	}
	covers, err := s.ListCoverBlobs(ctx)
	if err != nil || len(covers) != 2 {
		t.Fatalf("covers 枚举: %+v %v", covers, err)
	}

	found, size, err := s.ProbeCoverBlob(ctx, testHexName)
	if err != nil || !found || size != int64(len(data)) {
		t.Fatalf("ProbeCoverBlob: %v %d %v", found, size, err)
	}

	dst := filepath.Join(t.TempDir(), "cover")
	if err := s.GetCoverBlob(ctx, testHexName, dst, nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("封面下载往返: %v %v", got, err)
	}

	if err := s.DeleteCoverBlob(ctx, testHexName); err != nil {
		t.Fatal(err)
	}
	if found, _, err := s.ProbeCoverBlob(ctx, testHexName); err != nil || found {
		t.Fatalf("删除后应不存在: %v %v", found, err)
	}
}

// TestListBlobsSkipsDirectoryEntries covers/ 目录条目不得混进主命名空间
// (否则 gc 会把它当成"名字不是 32hex 的孤儿"、migrate 会对目录发 PUT)。
func TestListBlobsSkipsDirectoryEntries(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.PutBlob(ctx, testHexName, tempBlob(t, []byte("x")), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.PutCoverBlob(ctx, testHexName, tempBlob(t, []byte("y")), nil); err != nil {
		t.Fatal(err)
	}
	main, err := s.ListBlobs(ctx)
	if err != nil || len(main) != 1 {
		t.Fatalf("主命名空间只应有 1 个 blob: %v %v", main, err)
	}
	// ListAll 面向 migrate:必须携带 IsDir 让调用方跳过目录
	all, err := s.ListAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sawDir := false
	for _, o := range all {
		if o.Name == CoversDir && o.IsDir {
			sawDir = true
		}
		if o.Name == CoversDir && !o.IsDir {
			t.Fatal("covers 应标记为目录")
		}
	}
	if !sawDir {
		t.Fatal("ListAll 应包含 covers 目录条目(带 IsDir)")
	}
}
