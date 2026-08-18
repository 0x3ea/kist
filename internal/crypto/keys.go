package crypto

import (
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// MasterKey 是所有加密数据共用的 32 字节主密钥,明文永不落盘
// (仅以 Argon2id 包装后的形态存于 keyfile)。
type MasterKey [32]byte

// GenerateMasterKey 用系统 CSPRNG 生成一把全新主密钥(建账户时调用一次)。
func GenerateMasterKey() (MasterKey, error) {
	var mk MasterKey
	if _, err := rand.Read(mk[:]); err != nil {
		return mk, err
	}
	return mk, nil
}

// Wipe 尽力清零密钥内容(锁定/换密时调用)。
func (m *MasterKey) Wipe() {
	for i := range m {
		m[i] = 0
	}
}

// DeriveFileKey 为单个文件派生独立子密钥(HKDF-SHA256,salt=fileID)。
// 任意两个文件因此不可能出现"相同密钥+相同 nonce"的致命组合。
func DeriveFileKey(mk MasterKey, fileID [fileIDSize]byte) [32]byte {
	return hkdfSHA256(mk[:], fileID[:], []byte("kist/v1/blob"))
}

// DeriveMetaKey 派生用于加密 blob 头部 Meta 的子密钥。
func DeriveMetaKey(mk MasterKey) [32]byte {
	return hkdfSHA256(mk[:], []byte("kist/v1"), []byte("kist/v1/meta"))
}

func hkdfSHA256(ikm, salt, info []byte) [32]byte {
	var out [32]byte
	// 32 字节输出对 HKDF-SHA256 永远可行,失败只可能是实现级错误
	if _, err := io.ReadFull(hkdf.New(sha256.New, ikm, salt, info), out[:]); err != nil {
		panic("crypto: hkdf 派生失败: " + err.Error())
	}
	return out
}

// newXChaCha 构造 XChaCha20-Poly1305 AEAD。
// 选型理由:纯 Go 实现无需 AES-NI 也稳定高性能;192bit nonce 支持
// "随机前缀+计数器"的 STREAM 式派生,免去 nonce 管理负担。
func newXChaCha(key [32]byte) (cipher.AEAD, error) {
	return chacha20poly1305.NewX(key[:])
}
