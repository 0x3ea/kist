package crypto

import (
	"crypto/rand"
	"encoding/binary"

	"golang.org/x/crypto/argon2"
)

// Argon2Params 口令派生参数;随 keyfile 明文存储,未来可无痛提升强度。
type Argon2Params struct {
	Time      uint32 // 迭代次数
	MemoryKiB uint32 // 内存用量(KiB)
	Threads   uint8  // 并行度
}

// DefaultArgon2Params 生产默认:64MiB 内存、1 次迭代、4 线程。
// 测试请自行构造小参数加速,不要绕过本函数之外又自造生产参数。
func DefaultArgon2Params() Argon2Params {
	return Argon2Params{Time: 1, MemoryKiB: 64 * 1024, Threads: 4}
}

// keyfile 字节布局(共 110 字节,详见 docs/phase-1-crypto.md):
//
//	偏移 长度 字段
//	0    8    magic "KISTKEY1"
//	8    2    version u16le
//	10   2    flags u16le
//	12   16   argon2 salt(随机)
//	28   4    argon2 time u32le
//	32   4    argon2 memKiB u32le
//	36   1    argon2 threads u8
//	37   1    keyLen u8(=32)
//	38   24   wrapNonce(随机)
//	62   48   wrappedMK = XChaCha20-Poly1305(KEK, MK, aad=bytes[0..62))
//
// 解锁时 AEAD tag 校验失败即口令错误(或 keyfile 损坏),无需额外 verifier 块。
type KeyFile struct {
	version   uint16
	flags     uint16
	salt      [16]byte
	params    Argon2Params
	keyLen    uint8
	wrapNonce [nonceSize]byte
	wrapped   [48]byte // 32B 密文 + 16B tag
}

// CreateKeyFile 生成随机主密钥,并按给定 Argon2 参数用口令包装。
func CreateKeyFile(passphrase string, p Argon2Params) (*KeyFile, MasterKey, error) {
	mk, err := GenerateMasterKey()
	if err != nil {
		return nil, mk, err
	}
	kf, err := wrapMasterKey(mk, passphrase, p)
	if err != nil {
		return nil, mk, err
	}
	return kf, mk, nil
}

// wrapMasterKey 用口令派生 KEK 包装 MK;创建与 Rewrap 共用此路径。
func wrapMasterKey(mk MasterKey, passphrase string, p Argon2Params) (*KeyFile, error) {
	kf := &KeyFile{version: keyFileVersion, params: p, keyLen: 32}
	if _, err := rand.Read(kf.salt[:]); err != nil {
		return nil, err
	}
	if _, err := rand.Read(kf.wrapNonce[:]); err != nil {
		return nil, err
	}
	kek := deriveKEK(passphrase, kf.salt, p)
	aead, err := newXChaCha(kek)
	if err != nil {
		return nil, err
	}
	// AAD 取序列化后的前 62 字节(恰好到 wrapNonce 为止,不含密文区)
	header := kf.marshal()
	copy(kf.wrapped[:], aead.Seal(nil, kf.wrapNonce[:], mk[:], header[:62]))
	return kf, nil
}

func deriveKEK(passphrase string, salt [16]byte, p Argon2Params) [32]byte {
	var kek [32]byte
	copy(kek[:], argon2.IDKey([]byte(passphrase), salt[:], p.Time, p.MemoryKiB, p.Threads, 32))
	return kek
}

// ParseKeyFile 解析并做静态校验(magic/长度/版本/keyLen),不做任何解密。
func ParseKeyFile(b []byte) (*KeyFile, error) {
	if len(b) != keyFileFixedSize {
		return nil, ErrBadKeyFile
	}
	var kf KeyFile
	if string(b[:8]) != keyFileMagic {
		return nil, ErrBadKeyFile
	}
	kf.version = binary.LittleEndian.Uint16(b[8:])
	if kf.version != keyFileVersion {
		return nil, ErrBadKeyFile
	}
	kf.flags = binary.LittleEndian.Uint16(b[10:])
	copy(kf.salt[:], b[12:])
	kf.params.Time = binary.LittleEndian.Uint32(b[28:])
	kf.params.MemoryKiB = binary.LittleEndian.Uint32(b[32:])
	kf.params.Threads = b[36]
	kf.keyLen = b[37]
	if kf.keyLen != 32 {
		return nil, ErrBadKeyFile
	}
	// Argon2 参数合理范围上界:被篡改的 keyfile 若携带天文数字参数,
	// 解锁会试图分配 GiB 级内存——这是资源耗尽攻击向量,必须在解析期拒绝。
	// 上限取默认值(64MiB/1 次/4 线程)的宽裕超集,兼容未来适度提升。
	if kf.params.Time < 1 || kf.params.Time > 16 ||
		kf.params.Threads < 1 || kf.params.Threads > 64 ||
		kf.params.MemoryKiB < 8*uint32(kf.params.Threads) || kf.params.MemoryKiB > 1<<20 {
		return nil, ErrBadKeyFile
	}
	copy(kf.wrapNonce[:], b[38:])
	copy(kf.wrapped[:], b[62:])
	return &kf, nil
}

// Unlock 用口令解包主密钥;认证失败返回 ErrBadPassphrase。
func (k *KeyFile) Unlock(passphrase string) (MasterKey, error) {
	kek := deriveKEK(passphrase, k.salt, k.params)
	aead, err := newXChaCha(kek)
	if err != nil {
		return MasterKey{}, err
	}
	header := k.marshal()
	pt, err := aead.Open(nil, k.wrapNonce[:], k.wrapped[:], header[:62])
	if err != nil {
		return MasterKey{}, ErrBadPassphrase
	}
	var mk MasterKey
	copy(mk[:], pt)
	return mk, nil
}

// Bytes 序列化 keyfile;本地缓存与上传远端的是同一份字节。
func (k *KeyFile) Bytes() []byte {
	b := k.marshal()
	copy(b[62:], k.wrapped[:])
	return b[:]
}

func (k *KeyFile) marshal() [keyFileFixedSize]byte {
	var b [keyFileFixedSize]byte
	copy(b[:8], keyFileMagic)
	binary.LittleEndian.PutUint16(b[8:], k.version)
	binary.LittleEndian.PutUint16(b[10:], k.flags)
	copy(b[12:], k.salt[:])
	binary.LittleEndian.PutUint32(b[28:], k.params.Time)
	binary.LittleEndian.PutUint32(b[32:], k.params.MemoryKiB)
	b[36] = k.params.Threads
	b[37] = k.keyLen
	copy(b[38:], k.wrapNonce[:])
	// b[62:110] 为 wrapped 区,由 Bytes()/包装流程填充
	return b
}

// Rewrap 改口令:旧口令解出 MK,再以新口令与默认参数重新包装。
// 只重写这 110 字节,网盘上的所有 blob 完全不受影响。
func (k *KeyFile) Rewrap(oldPass, newPass string) error {
	mk, err := k.Unlock(oldPass)
	if err != nil {
		return err
	}
	nk, err := wrapMasterKey(mk, newPass, DefaultArgon2Params())
	if err != nil {
		return err
	}
	*k = *nk
	return nil
}
