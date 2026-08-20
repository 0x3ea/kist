package crypto

import (
	"bytes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"io"
)

// EncryptOptions 控制加密输出;普通文件只用 ChunkSize,
// 索引备份(index.enc)还会填 Mtime/Revision/DeviceID 三个字段。
type EncryptOptions struct {
	ChunkSize uint32 // 0 表示使用 DefaultChunkSize
	// NoPadding 关闭大小量化(写出 v1 格式);零值 false = 默认量化到
	// 档位(v2,TODO-08)。流量计费网盘可在设置里关(size_padding=off)。
	NoPadding bool
	Mtime     uint64
	Revision  uint64
	DeviceID  [8]byte
}

// BlobWriter 流式分块加密器:Write 喂入明文,Close 封末块并回填加密头。
// 目标必须是可 Seek 的(实践中是临时 *os.File);全程内存 ≈ 一个块。
type BlobWriter struct {
	w         io.WriteSeeker
	fileKey   [32]byte
	metaKey   [32]byte
	aead      cipher.AEAD
	metaNonce [nonceSize]byte
	meta      Meta
	version   uint16 // blobVersion1|2,由 EncryptOptions.NoPadding 决定
	buf       []byte // 已收满但尚未封出的明文缓冲
	chunks    uint64 // 已封出的块数
	sha       hash.Hash
	closed    bool
}

// NewBlobWriter 生成随机 fileID/noncePrefix/metaNonce 并写出 156 字节占位头。
// sealedMeta 区先写零,Close 时 Seek 回偏移 40 回填——源文件只需单遍读取。
func NewBlobWriter(w io.WriteSeeker, mk MasterKey, opt EncryptOptions) (*BlobWriter, error) {
	if opt.ChunkSize == 0 {
		opt.ChunkSize = DefaultChunkSize
	}
	bw := &BlobWriter{
		w:   w,
		buf: make([]byte, 0, opt.ChunkSize),
		sha: sha256.New(),
	}
	if opt.NoPadding {
		bw.version = blobVersion1
	} else {
		bw.version = blobVersion2
	}
	bw.meta.ChunkSize = opt.ChunkSize
	bw.meta.Mtime = opt.Mtime
	bw.meta.Revision = opt.Revision
	bw.meta.DeviceID = opt.DeviceID
	if _, err := rand.Read(bw.meta.FileID[:]); err != nil {
		return nil, err
	}
	if _, err := rand.Read(bw.meta.NoncePrefix[:]); err != nil {
		return nil, err
	}
	if _, err := rand.Read(bw.metaNonce[:]); err != nil {
		return nil, err
	}
	bw.fileKey = DeriveFileKey(mk, bw.meta.FileID)
	bw.metaKey = DeriveMetaKey(mk)
	aead, err := newXChaCha(bw.fileKey)
	if err != nil {
		return nil, err
	}
	bw.aead = aead

	head := plainHeader(bw.metaNonce, bw.version)
	var zero [metaSealedSize]byte
	if _, err := w.Write(head[:]); err != nil {
		return nil, err
	}
	if _, err := w.Write(zero[:]); err != nil {
		return nil, err
	}
	return bw, nil
}

// Write 追加明文。关键规则:只有当缓冲收满一个块"且还有后续数据"时才封出
// 该块,否则留在缓冲——这保证恰好等于块大小整数倍的文件不会多出一个空末块
// (块数统一为 n = max(1, ceil(size/chunk)),见 numChunks)。
func (bw *BlobWriter) Write(p []byte) (int, error) {
	if bw.closed {
		return 0, fmt.Errorf("crypto: BlobWriter 已关闭")
	}
	orig := p
	chunk := int(bw.meta.ChunkSize)
	for len(p) > 0 {
		if len(bw.buf) < chunk {
			n := min(chunk-len(bw.buf), len(p))
			bw.buf = append(bw.buf, p[:n]...)
			p = p[n:]
		}
		if len(bw.buf) == chunk && len(p) > 0 {
			if err := bw.sealChunk(false); err != nil {
				return len(orig) - len(p), err
			}
		}
	}
	bw.sha.Write(orig)
	bw.meta.OrigSize += uint64(len(orig))
	return len(orig), nil
}

// Close 封出末块(剩余缓冲可能为 0 字节,空文件即此情形),
// 然后回填加密头 sealedMeta。v2 先补零到量化档位——补零并入末块
// (或续出整块)一起加密,块认证顺带保护填充完整性。
func (bw *BlobWriter) Close() error {
	if bw.closed {
		return nil
	}
	if bw.version == blobVersion2 {
		if err := bw.padTo(bucketSize(bw.meta.OrigSize)); err != nil {
			return err
		}
	}
	if err := bw.sealChunk(true); err != nil {
		return err
	}
	copy(bw.meta.PlainSHA[:], bw.sha.Sum(nil))
	metaAEAD, err := newXChaCha(bw.metaKey)
	if err != nil {
		return err
	}
	aad := plainHeader(bw.metaNonce, bw.version)
	em := encodeMeta(bw.meta)
	sealed := metaAEAD.Seal(nil, bw.metaNonce[:], em[:], aad[:])
	if _, err := bw.w.Seek(blobPlainHeaderSize, io.SeekStart); err != nil {
		return err
	}
	if _, err := bw.w.Write(sealed); err != nil {
		return err
	}
	bw.closed = true
	return nil
}

// Meta 返回头部元数据;FileID/OrigSize/PlainSHA 在 Close 之后才是最终值,
// 供上层写入索引。
func (bw *BlobWriter) Meta() Meta { return bw.meta }

func (bw *BlobWriter) sealChunk(final bool) error {
	nonce := chunkNonce(bw.meta.NoncePrefix, bw.chunks)
	aad := chunkAAD(bw.meta.FileID, bw.chunks, final)
	sealed := bw.aead.Seal(nil, nonce[:], bw.buf, aad)
	if _, err := bw.w.Write(sealed); err != nil {
		return err
	}
	bw.chunks++
	bw.buf = bw.buf[:0]
	return nil
}

// padTo 把明文流补零到 target(v2 量化档位)。补零走与真实数据完全相同的
// 分块/认证路径(可能封出多个整块),但绝不计入 OrigSize 与 SHA——
// 二者只属于真实明文,读侧据此截断交付。
// 流位置 = 已封块数×chunkSize + 缓冲长度:非末块恒为整块,该式即明文流
// 的真实偏移(不能用 OrigSize,它只计真实字节,补零封块后不增长——
// 测试驱动发现:以 OrigSize 为基准会原地死循环)。
func (bw *BlobWriter) padTo(target uint64) error {
	var zeros [64 << 10]byte
	chunk := uint64(bw.meta.ChunkSize)
	for {
		pos := bw.chunks*chunk + uint64(len(bw.buf))
		if pos >= target {
			return nil
		}
		room := chunk - uint64(len(bw.buf))
		n := min(min(room, target-pos), uint64(len(zeros)))
		bw.buf = append(bw.buf, zeros[:n]...)
		// 与 Write 同规则:缓冲收满且后续还有补零才封为非末块;
		// 恰好补到档位则留在缓冲,交给 Close 以 final 标志封出
		if uint64(len(bw.buf)) == chunk && bw.chunks*chunk+chunk < target {
			if err := bw.sealChunk(false); err != nil {
				return err
			}
		}
	}
}

// BlobReader 流式解密器:NewBlobReader 解析并认证头部,Read 逐块解密,
// EOF 时执行终检(底层无剩余字节 + 明文 SHA-256 与头部声明一致)。
type BlobReader struct {
	r          io.Reader
	aead       cipher.AEAD
	meta       Meta
	padded     bool   // v2:明文区含量化补零,只交付前 OrigSize 字节
	plainTotal uint64 // 明文区总长(v1=OrigSize;v2=bucketSize(OrigSize))
	delivered  uint64 // 已交付(参与 SHA)的真实明文字节数
	chunks     uint64 // 已读完的块数
	nChunks    uint64 // 总块数
	sha        hash.Hash
	buf        []byte // 当前块已解密、尚未被读走的部分
	off        int    // buf 内偏移
	verified   bool
}

// NewBlobReader 解析头部并做长度总校验,任何不符立即失败:
// 期望密文总长 = 156 + 16*n + 明文区总长(v1=origSize,v2=bucketSize),
// 以此拦截截断与尾部追加。
func NewBlobReader(r io.Reader, size int64, mk MasterKey) (*BlobReader, error) {
	if size < blobHeaderSize {
		return nil, fmt.Errorf("%w: 文件过小(%d 字节)", ErrCorruptBlob, size)
	}
	var head [blobHeaderSize]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return nil, fmt.Errorf("%w: 读取头部失败: %v", ErrCorruptBlob, err)
	}
	if string(head[:8]) != blobMagic {
		return nil, fmt.Errorf("%w: magic 不符", ErrCorruptBlob)
	}
	v := binary.LittleEndian.Uint16(head[8:])
	if v != blobVersion1 && v != blobVersion2 {
		return nil, fmt.Errorf("%w: 不支持的版本 %d", ErrCorruptBlob, v)
	}
	if ml := binary.LittleEndian.Uint32(head[12:]); ml != metaSealedSize {
		return nil, fmt.Errorf("%w: metaLen 异常 %d", ErrCorruptBlob, ml)
	}
	var metaNonce [nonceSize]byte
	copy(metaNonce[:], head[16:])

	metaAEAD, err := newXChaCha(DeriveMetaKey(mk))
	if err != nil {
		return nil, err
	}
	pt, err := metaAEAD.Open(nil, metaNonce[:], head[blobPlainHeaderSize:], head[:blobPlainHeaderSize])
	if err != nil {
		// 头部认证失败:最大可能是主密钥不符(比如指向了另一个账户的网盘),
		// 也可能是头部字节损坏——两种情况在此无法区分
		return nil, ErrWrongKey
	}
	meta, err := decodeMeta(pt)
	if err != nil {
		return nil, err
	}
	if meta.ChunkSize == 0 {
		return nil, fmt.Errorf("%w: chunkSize 为 0", ErrCorruptBlob)
	}
	padded := v == blobVersion2
	plainTotal := meta.OrigSize
	if padded {
		plainTotal = bucketSize(meta.OrigSize)
	}
	n := numChunks(plainTotal, meta.ChunkSize)
	expected := uint64(blobHeaderSize) + n*tagSize + plainTotal
	if uint64(size) != expected {
		return nil, fmt.Errorf("%w: 长度不符(期望 %d,实际 %d),疑似截断或被追加", ErrCorruptBlob, expected, size)
	}

	aead, err := newXChaCha(DeriveFileKey(mk, meta.FileID))
	if err != nil {
		return nil, err
	}
	return &BlobReader{
		r:          r,
		aead:       aead,
		meta:       meta,
		padded:     padded,
		plainTotal: plainTotal,
		sha:        sha256.New(),
		nChunks:    n,
	}, nil
}

// Meta 返回已解出的头部元数据,NewBlobReader 成功后即可用。
func (br *BlobReader) Meta() Meta { return br.meta }

// Read 逐块解密交付;所有块(含空文件的零长度末块)读完后做终检再返回 io.EOF。
// 结束判定必须按块计数而不是剩余明文字节数:空末块本身也需要被读取并认证,
// 否则它的 tag 会残留在流里,终检会误报"末块之后仍有剩余数据"。
func (br *BlobReader) Read(p []byte) (int, error) {
	for br.off >= len(br.buf) {
		if br.chunks == br.nChunks {
			return 0, br.finish()
		}
		if err := br.readChunk(); err != nil {
			return 0, err
		}
	}
	n := copy(p, br.buf[br.off:])
	br.off += n
	return n, nil
}

func (br *BlobReader) readChunk() error {
	i := br.chunks
	final := i == br.nChunks-1
	plainLen := uint64(br.meta.ChunkSize)
	if final {
		plainLen = br.plainTotal - i*uint64(br.meta.ChunkSize)
	}
	buf := make([]byte, int(plainLen)+tagSize)
	if _, err := io.ReadFull(br.r, buf); err != nil {
		return fmt.Errorf("%w: 第 %d 块读取不完整", ErrCorruptBlob, i)
	}
	nonce := chunkNonce(br.meta.NoncePrefix, i)
	aad := chunkAAD(br.meta.FileID, i, final)
	pt, err := br.aead.Open(nil, nonce[:], buf, aad)
	if err != nil {
		// 走到这里说明头部认证通过但块认证失败:数据被篡改/重排,
		// 或末块标记不符(截断后拿中间块冒充)
		return fmt.Errorf("%w: 第 %d 块认证失败", ErrCorruptBlob, i)
	}
	br.chunks = i + 1
	if br.padded {
		// v2:交付与 SHA 只覆盖前 OrigSize 字节;跨过边界的块截前段,
		// 纯补零块认证后整块丢弃(仍须读完整块,末块 tag 不能残留)
		if keep := br.meta.OrigSize - br.delivered; keep < uint64(len(pt)) {
			pt = pt[:int(keep)]
		}
		br.delivered += uint64(len(pt))
	}
	br.sha.Write(pt)
	br.buf = pt
	br.off = 0
	return nil
}

// finish 终检:底层必须恰好在末块后结束,且明文 SHA-256 与头部声明一致。
func (br *BlobReader) finish() error {
	if br.verified {
		return io.EOF
	}
	br.verified = true
	var probe [1]byte
	if n, err := br.r.Read(probe[:]); n > 0 || err != io.EOF {
		return fmt.Errorf("%w: 末块之后仍有剩余数据", ErrCorruptBlob)
	}
	if !bytes.Equal(br.sha.Sum(nil), br.meta.PlainSHA[:]) {
		return fmt.Errorf("%w: 明文 SHA-256 校验失败", ErrCorruptBlob)
	}
	return io.EOF
}
