package e2e

// 进度口径回归(GUI 实测反馈的修复验收):BytesTotal 只计网络字节(密文),
// 本地加密/解密不进预算。旧口径"明文+密文"让传输大小显示为实际的两倍。

import (
	"context"
	"testing"
)

// TestE2EProgressSingleCopyAccounting 上传与下载各自对齐 files.cipher_size;
// 派生封面不进预算(非图片源无封面,天然隔离)。
func TestE2EProgressSingleCopyAccounting(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	src := makeFile(t, t.TempDir(), "进度.bin", 100_000)

	if n, err := e.m.UploadPaths(ctx, []string{src}, 1); err != nil || n != 1 {
		t.Fatalf("入队: n=%d err=%v", n, err)
	}
	trs := waitIdle(t, e.m)
	allDone(t, trs)
	hits, _ := e.db.Search("进度.bin", 5)
	if len(hits) != 1 {
		t.Fatalf("搜索: %+v", hits)
	}
	f, err := e.db.GetFile(hits[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if trs[0].BytesTotal != f.CipherSize {
		t.Fatalf("上传 total 应只计密文 %d,得到 %d(旧口径为 %d)",
			f.CipherSize, trs[0].BytesTotal, f.Size+f.CipherSize)
	}
	if trs[0].BytesDone != f.CipherSize {
		t.Fatalf("上传 done 应收满密文: %d / %d", trs[0].BytesDone, f.CipherSize)
	}

	// 下载同口径:total = 密文,GET 收满即 100%,解密不再叠加明文
	out := t.TempDir()
	if _, err := e.m.DownloadTo(ctx, []int64{f.ID}, out); err != nil {
		t.Fatal(err)
	}
	trs = waitIdle(t, e.m)
	allDone(t, trs)
	dl := trs[len(trs)-1]
	if dl.Kind != "download" || dl.BytesTotal != f.CipherSize || dl.BytesDone != f.CipherSize {
		t.Fatalf("下载口径: kind=%s %d/%d (want %d/%d)",
			dl.Kind, dl.BytesDone, dl.BytesTotal, f.CipherSize, f.CipherSize)
	}
}
