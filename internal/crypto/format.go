package crypto

import "encoding/binary"

// ---- keyfile 格式常量(110 字节,布局见 docs/phase-1-crypto.md)----
const (
	keyFileMagic     = "KISTKEY1" // 8B
	keyFileVersion   = 1
	keyFileFixedSize = 110 // 整个 keyfile 的固定长度
)

// ---- blob 格式常量 ----
const (
	blobMagic   = "KISTBLB1" // 8B
	blobVersion = 1

	// DefaultChunkSize 默认分块 4MiB;块大小是上限,末块可以更小
	DefaultChunkSize uint32 = 4 << 20

	blobPlainHeaderSize = 40                                   // 明文头:magic+version+flags+metaLen+metaNonce
	metaPlainSize       = 100                                  // Meta 编码后的定长
	tagSize             = 16                                   // Poly1305 认证标签
	metaSealedSize      = metaPlainSize + tagSize              // 116
	blobHeaderSize      = blobPlainHeaderSize + metaSealedSize // 156
	nonceSize           = 24                                   // XChaCha20 nonce 长度
	noncePrefixSize     = 16                                   // 随机前缀,后接 8B 块序号拼成 nonce
	fileIDSize          = 16
)

// Meta 是 blob 头部里被加密的元数据;所有字段都不出现在明文区,
// 网盘侧无法借此获知文件大小、摘要等任何信息(密文总长除外)。
type Meta struct {
	FileID      [fileIDSize]byte // 文件唯一 ID,hex 后即索引里的 uuid
	OrigSize    uint64           // 明文大小
	ChunkSize   uint32           // 分块大小
	NoncePrefix [noncePrefixSize]byte
	PlainSHA    [32]byte // 明文 SHA-256(终检用)
	Mtime       uint64   // 以下三字段仅索引备份(index.enc)使用,普通文件恒为 0
	Revision    uint64
	DeviceID    [8]byte
}

// encodeMeta 按固定小端布局编码为 100 字节。
func encodeMeta(m Meta) [metaPlainSize]byte {
	var b [metaPlainSize]byte
	copy(b[0:], m.FileID[:])
	binary.LittleEndian.PutUint64(b[16:], m.OrigSize)
	binary.LittleEndian.PutUint32(b[24:], m.ChunkSize)
	copy(b[28:], m.NoncePrefix[:])
	copy(b[44:], m.PlainSHA[:])
	binary.LittleEndian.PutUint64(b[76:], m.Mtime)
	binary.LittleEndian.PutUint64(b[84:], m.Revision)
	copy(b[92:], m.DeviceID[:])
	return b
}

// decodeMeta 严格按定长解码,长度不符视为损坏。
func decodeMeta(b []byte) (Meta, error) {
	var m Meta
	if len(b) != metaPlainSize {
		return m, ErrCorruptBlob
	}
	copy(m.FileID[:], b[0:])
	m.OrigSize = binary.LittleEndian.Uint64(b[16:])
	m.ChunkSize = binary.LittleEndian.Uint32(b[24:])
	copy(m.NoncePrefix[:], b[28:])
	copy(m.PlainSHA[:], b[44:])
	m.Mtime = binary.LittleEndian.Uint64(b[76:])
	m.Revision = binary.LittleEndian.Uint64(b[84:])
	copy(m.DeviceID[:], b[92:])
	return m, nil
}

// plainHeader 构造 40 字节明文头;writer 写出、reader 解析与头部 AAD 共用同一布局。
func plainHeader(metaNonce [nonceSize]byte) [blobPlainHeaderSize]byte {
	var h [blobPlainHeaderSize]byte
	copy(h[:8], blobMagic)
	binary.LittleEndian.PutUint16(h[8:], blobVersion)
	binary.LittleEndian.PutUint16(h[10:], 0) // flags 保留
	binary.LittleEndian.PutUint32(h[12:], metaSealedSize)
	copy(h[16:], metaNonce[:])
	return h
}

// chunkNonce 拼出第 i 块的 nonce:NoncePrefix(16B) || u64be(i)。
// 同文件按序号不重复、跨文件因随机前缀不重复,构造上杜绝 nonce 复用。
func chunkNonce(prefix [noncePrefixSize]byte, i uint64) [nonceSize]byte {
	var n [nonceSize]byte
	copy(n[:noncePrefixSize], prefix[:])
	binary.BigEndian.PutUint64(n[noncePrefixSize:], i)
	return n
}

// chunkAAD 生成块认证数据:绑定 magic、fileID、块序号与 final 标志。
// 由此防止:块被重排、跨文件拼接、截断后把中间块冒充末块。
func chunkAAD(fileID [fileIDSize]byte, i uint64, final bool) []byte {
	var idx [8]byte
	binary.BigEndian.PutUint64(idx[:], i)
	aad := make([]byte, 0, len(blobMagic)+fileIDSize+8+1)
	aad = append(aad, blobMagic...)
	aad = append(aad, fileID[:]...)
	aad = append(aad, idx[:]...)
	if final {
		aad = append(aad, 1)
	} else {
		aad = append(aad, 0)
	}
	return aad
}

// numChunks 统一块数规则:n = max(1, ceil(size/chunk))。
// 空文件也有 1 个零长度末块,保证 final 标记在任何文件中都存在。
// 与之配套的写侧规则见 BlobWriter.Write:整块只有在"缓冲已满且还有后续数据"
// 时才封出,否则留到 Close 以 final 标记封出。
func numChunks(size uint64, chunk uint32) uint64 {
	if chunk == 0 {
		chunk = DefaultChunkSize
	}
	if size == 0 {
		return 1
	}
	c := uint64(chunk)
	return (size + c - 1) / c
}
