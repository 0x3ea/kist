package crypto

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"math/rand"
	"os"
	"runtime"
	"testing"
)

// ---- 测试辅助 ----

func fixedKey(s string) MasterKey {
	return MasterKey(sha256.Sum256([]byte(s)))
}

func pseudoBytes(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + int(seed)) // 确定性伪随机填充
	}
	return b
}

// memSeeker 是测试用的内存版 io.WriteSeeker(生产路径目标是临时 *os.File)。
type memSeeker struct {
	b   []byte
	pos int64
}

func (m *memSeeker) Write(p []byte) (int, error) {
	end := m.pos + int64(len(p))
	if end > int64(len(m.b)) {
		m.b = append(m.b, make([]byte, end-int64(len(m.b)))...)
	}
	copy(m.b[m.pos:], p)
	m.pos = end
	return len(p), nil
}

func (m *memSeeker) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		m.pos = off
	case io.SeekCurrent:
		m.pos += off
	case io.SeekEnd:
		m.pos = int64(len(m.b)) + off
	default:
		return 0, errors.New("memSeeker: 非法 whence")
	}
	if m.pos < 0 {
		return 0, errors.New("memSeeker: 负偏移")
	}
	return m.pos, nil
}

func (m *memSeeker) Bytes() []byte { return m.b }

func encryptBytes(t *testing.T, mk MasterKey, data []byte, chunk uint32) ([]byte, Meta) {
	t.Helper()
	return encryptBytesOpt(t, mk, data, chunk, EncryptOptions{})
}

func encryptBytesOpt(t *testing.T, mk MasterKey, data []byte, chunk uint32, opt EncryptOptions) ([]byte, Meta) {
	t.Helper()
	opt.ChunkSize = chunk
	ms := &memSeeker{}
	bw, err := NewBlobWriter(ms, mk, opt)
	if err != nil {
		t.Fatalf("NewBlobWriter: %v", err)
	}
	if _, err := bw.Write(data); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := bw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return ms.Bytes(), bw.Meta()
}

func decryptBytes(t *testing.T, mk MasterKey, blob []byte) ([]byte, Meta, error) {
	t.Helper()
	br, err := NewBlobReader(bytes.NewReader(blob), int64(len(blob)), mk)
	if err != nil {
		return nil, Meta{}, err
	}
	got, err := io.ReadAll(br)
	return got, br.Meta(), err
}

// patternReader 按种子确定性地产出 n 字节流,可重置再生——
// 供大文件流式测试两遍比对,全程不在内存里持有整个文件。
type patternReader struct {
	rnd       *rand.Rand
	remaining int64
}

func newPatternReader(seed int64, n int64) *patternReader {
	return &patternReader{rnd: rand.New(rand.NewSource(seed)), remaining: n}
}

func (p *patternReader) Read(b []byte) (int, error) {
	if p.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(b)) > p.remaining {
		b = b[:p.remaining]
	}
	p.rnd.Read(b) // (*rand.Rand).Read 确定性且不失败
	n := len(b)
	p.remaining -= int64(n)
	return n, nil
}

// ---- 用例 ----

func TestMetaEncodeDecode(t *testing.T) {
	var m Meta
	copy(m.FileID[:], pseudoBytes(16, 1))
	m.OrigSize = 0xdeadbeef
	m.ChunkSize = 4096
	copy(m.NoncePrefix[:], pseudoBytes(16, 2))
	copy(m.PlainSHA[:], pseudoBytes(32, 3))
	m.Mtime = 1234567890
	m.Revision = 42
	copy(m.DeviceID[:], pseudoBytes(8, 4))
	enc := encodeMeta(m)
	got, err := decodeMeta(enc[:])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if encodeMeta(got) != encodeMeta(m) {
		t.Fatal("Meta 编解码往返不一致")
	}
	if _, err := decodeMeta(make([]byte, 99)); !errors.Is(err, ErrCorruptBlob) {
		t.Fatalf("长度不符应报 ErrCorruptBlob,得到 %v", err)
	}
}

func TestBlobRoundTrip(t *testing.T) {
	const chunk = 1024
	mk := fixedKey("roundtrip")
	// 尺寸刻意覆盖:空文件、单字节、块边界±1、整块倍数(防 off-by-one 回归)、多块带余数
	sizes := []int{0, 1, chunk - 1, chunk, chunk + 1, 2 * chunk, 3*chunk + 123}
	modes := []struct {
		name string
		v1   bool // true = 关闭量化(v1),false = 默认量化(v2)
	}{
		{"v1-无填充", true},
		{"v2-量化填充", false},
	}
	for _, mode := range modes {
		for _, size := range sizes {
			data := pseudoBytes(size, byte(size%251))
			blob, meta := encryptBytesOpt(t, mk, data, chunk,
				EncryptOptions{NoPadding: mode.v1})

			// 长度公式:v1 = 156+16n+orig;v2 = 156+16n+bucket(orig)
			plainTotal := uint64(size)
			wantVersion := uint16(blobVersion2)
			if mode.v1 {
				wantVersion = blobVersion1
			} else {
				plainTotal = bucketSize(uint64(size))
			}
			if v := binary.LittleEndian.Uint16(blob[8:]); v != wantVersion {
				t.Fatalf("%s size=%d: 版本位 = %d,期望 %d", mode.name, size, v, wantVersion)
			}
			n := numChunks(plainTotal, chunk)
			wantLen := blobHeaderSize + int(n)*tagSize + int(plainTotal)
			if len(blob) != wantLen {
				t.Fatalf("%s size=%d: 密文长 %d,期望 %d(n=%d)", mode.name, size, len(blob), wantLen, n)
			}
			got, gotMeta, err := decryptBytes(t, mk, blob)
			if err != nil {
				t.Fatalf("%s size=%d: 解密: %v", mode.name, size, err)
			}
			if !bytes.Equal(got, data) {
				t.Fatalf("%s size=%d: 明文不一致", mode.name, size)
			}
			if gotMeta.OrigSize != uint64(size) || gotMeta.ChunkSize != chunk {
				t.Fatalf("%s size=%d: meta 尺寸字段不符: %+v", mode.name, size, gotMeta)
			}
			sum := sha256.Sum256(data)
			if gotMeta.PlainSHA != sum {
				t.Fatalf("%s size=%d: PlainSHA 不符", mode.name, size)
			}
			if meta.FileID != gotMeta.FileID {
				t.Fatalf("%s size=%d: 写读两侧 FileID 不一致", mode.name, size)
			}
		}
	}
}

// TestBucketSize 档位函数是格式规范:精确边界与开销上界逐一钉死。
func TestBucketSize(t *testing.T) {
	cases := []struct{ orig, want uint64 }{
		{0, 4 << 10}, // 空文件归最小档,不再可识别(v1 时空文件仅 172B)
		{1, 4 << 10},
		{4 << 10, 4 << 10},   // 恰在档位,零填充
		{4<<10 + 1, 8 << 10}, // 越档进下一档
		{1<<20 - 1, 1 << 20}, // 小区上界
		{1 << 20, 1 << 20},   // 1MiB 含在小区(恰整档)
	}
	for _, c := range cases {
		if got := bucketSize(c.orig); got != c.want {
			t.Fatalf("bucketSize(%d) = %d,期望 %d", c.orig, got, c.want)
		}
	}
	// 性质:小区开销 ≤4KiB 且恒非零;大区开销 ≤10%
	for s := uint64(0); s <= 1<<20; s += 1234 {
		if b := bucketSize(s); b < s || b == 0 || b-s > 4<<10 {
			t.Fatalf("小区 bucketSize(%d) = %d 越界", s, b)
		}
	}
	for _, s := range []uint64{1<<20 + 1, 5<<20 + 123, 100<<20 + 7, 1<<30 + 99, 7 << 30} {
		if b := bucketSize(s); b < s || b-s > s/10+1 {
			t.Fatalf("大区 bucketSize(%d) = %d,开销超 10%%", s, b)
		}
	}
}

// TestBlobV2PaddingIntegrity 补零与真实数据走同一认证路径:
// 篡改补零区必须被拒绝——填充不是明文区外的裸字节。
func TestBlobV2PaddingIntegrity(t *testing.T) {
	const chunk = 1024
	mk := fixedKey("padtamper")
	data := pseudoBytes(100, 0x66) // bucket=4KiB,真实数据全在第 1 块,补零跨块
	blob, _ := encryptBytes(t, mk, data, chunk)
	bad := bytes.Clone(blob)
	bad[blobHeaderSize+3*(chunk+tagSize)+500] ^= 0xFF // 纯补零区内
	if _, _, err := decryptBytes(t, mk, bad); err == nil {
		t.Fatal("补零区被篡改必须被认证拒绝")
	}

	// 删掉补零的最后一块 → 长度校验拒绝
	if _, _, err := decryptBytes(t, mk, blob[:len(blob)-(chunk+tagSize)]); err == nil {
		t.Fatal("删补零末块必须被长度校验拒绝")
	}
}

func TestBlobDetectsCorruption(t *testing.T) {
	const chunk = 1024
	mk := fixedKey("corrupt")
	data := pseudoBytes(3*chunk+7, 0x5A)
	blob, _ := encryptBytes(t, mk, data, chunk)

	// 覆盖:magic、metaNonce(头部 AAD)、sealedMeta、各数据块、末块 tag
	positions := []int{
		0,
		15,
		45,
		blobHeaderSize + 5,
		blobHeaderSize + (chunk + tagSize) + 3,
		blobHeaderSize + 2*(chunk+tagSize) + 2,
		len(blob) - 1,
	}
	for _, pos := range positions {
		bad := bytes.Clone(blob)
		bad[pos] ^= 0xFF
		if _, _, err := decryptBytes(t, mk, bad); err == nil {
			t.Fatalf("偏移 %d 被篡改后解密仍然成功", pos)
		}
	}
}

func TestBlobDetectsTruncation(t *testing.T) {
	const chunk = 1024
	mk := fixedKey("truncate")
	data := pseudoBytes(3*chunk+7, 0x33)
	blob, _ := encryptBytes(t, mk, data, chunk)
	stride := chunk + tagSize

	cases := map[string][]byte{
		"少 1 字节":     blob[:len(blob)-1],
		"去掉整个末块":     blob[:len(blob)-stride],
		"去掉中间整块(截断)": blob[:blobHeaderSize+stride+chunk],
	}
	for name, bad := range cases {
		if _, _, err := decryptBytes(t, mk, bad); err == nil {
			t.Fatalf("%s 未被检出", name)
		}
	}
}

func TestBlobDetectsReorder(t *testing.T) {
	const chunk = 1024
	mk := fixedKey("reorder")
	data := pseudoBytes(3*chunk+11, 0x77)
	blob, _ := encryptBytes(t, mk, data, chunk)
	stride := chunk + tagSize

	bad := bytes.Clone(blob)
	// 交换第 0 块与第 1 块(连同各自的认证标签)
	copy(bad[blobHeaderSize:], blob[blobHeaderSize+stride:blobHeaderSize+2*stride])
	copy(bad[blobHeaderSize+stride:], blob[blobHeaderSize:blobHeaderSize+stride])
	if _, _, err := decryptBytes(t, mk, bad); err == nil {
		t.Fatal("块重排未被检出")
	}
}

func TestBlobDetectsAppend(t *testing.T) {
	const chunk = 1024
	mk := fixedKey("append")
	data := pseudoBytes(chunk+5, 0x99)
	blob, _ := encryptBytes(t, mk, data, chunk)

	for name, extra := range map[string][]byte{
		"追加 5 字节": {1, 2, 3, 4, 5},
		"追加一整块零":  make([]byte, chunk+tagSize),
	} {
		bad := append(bytes.Clone(blob), extra...)
		if _, _, err := decryptBytes(t, mk, bad); err == nil {
			t.Fatalf("%s 未被检出", name)
		}
	}
}

// TestBlobFinalFlagSpoof 手工把"恰好一块"文件的唯一数据块按非末块标志重封,
// 验证 AAD 中 final 标志的绑定:长度校验对此无能为力,只有认证能拦住。
func TestBlobFinalFlagSpoof(t *testing.T) {
	const chunk = 1024
	mk := fixedKey("spoof")
	data := pseudoBytes(chunk, 0xCD)
	// 固定 v1(无填充):保证"恰好一块"前提——v2 会把 1KiB 量化到 4KiB(4 块),
	// 本测试针对的"唯一数据块冒充非末块"场景就不成立了
	blob, meta := encryptBytesOpt(t, mk, data, chunk, EncryptOptions{NoPadding: true})

	fileKey := DeriveFileKey(mk, meta.FileID)
	aead, err := newXChaCha(fileKey)
	if err != nil {
		t.Fatal(err)
	}
	nonce := chunkNonce(meta.NoncePrefix, 0)
	aad := chunkAAD(meta.FileID, 0, false) // 冒充:声明自己不是末块
	forged := bytes.Clone(blob)
	copy(forged[blobHeaderSize:], aead.Seal(nil, nonce[:], data, aad))

	if _, _, err := decryptBytes(t, mk, forged); err == nil {
		t.Fatal("final 标志伪造(非末块冒充末块)未被检出")
	}
}

func TestBlobWrongKey(t *testing.T) {
	const chunk = 1024
	mk := fixedKey("对的密钥")
	blob, _ := encryptBytes(t, mk, pseudoBytes(2*chunk+1, 0x11), chunk)

	_, _, err := decryptBytes(t, fixedKey("错的密钥"), blob)
	if !errors.Is(err, ErrWrongKey) {
		t.Fatalf("期望 ErrWrongKey,得到 %v", err)
	}
}

// TestBlobStreamingMemory 用 40MB(默认 4MiB 分块)验证流式性质:
// 全程不在内存持有整个文件,堆增长必须远小于文件大小。
func TestBlobStreamingMemory(t *testing.T) {
	const total = 40 << 20
	mk := fixedKey("memory")

	f, err := os.CreateTemp("", "kist-blob-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	defer f.Close()

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	// 第一遍:流式加密
	bw, err := NewBlobWriter(f, mk, EncryptOptions{}) // DefaultChunkSize
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(bw, newPatternReader(1, total)); err != nil {
		t.Fatalf("加密写入: %v", err)
	}
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	size := st.Size()

	// 第二遍:流式解密并与重新生成的同一数据流比对
	br, err := NewBlobReader(f, size, mk)
	if err != nil {
		t.Fatal(err)
	}
	want := newPatternReader(1, total)
	var got, exp []byte
	var delivered int64
	for {
		got = make([]byte, 1<<20)
		n, rerr := br.Read(got)
		exp = make([]byte, n)
		_, _ = io.ReadFull(want, exp)
		if !bytes.Equal(got[:n], exp) {
			t.Fatalf("偏移 %d 处明文不一致", delivered)
		}
		delivered += int64(n)
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			t.Fatalf("解密读取: %v", rerr)
		}
	}
	if delivered != total {
		t.Fatalf("共解出 %d 字节,期望 %d", delivered, total)
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if after.HeapInuse > before.HeapInuse && after.HeapInuse-before.HeapInuse > 64<<20 {
		t.Fatalf("堆增长 %d MiB,超过 64MiB 限制", (after.HeapInuse-before.HeapInuse)>>20)
	}
}
