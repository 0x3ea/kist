# Phase 1 — 加密核心 internal/crypto

> 状态:未开始
> 前置:Phase 0
> 产出:`internal/crypto/`(format.go、keys.go、keyfile.go、blob.go + *_test.go)

## 目标

实现与网络、数据库、GUI 完全解耦的加密核心:

1. **密钥体系**:口令 → Argon2id → KEK,用 KEK 包装/解包随机主密钥 MK(keyfile 机制)
2. **blob 流式分块加解密**(BlobWriter / BlobReader):任意大小文件,内存占用恒定(≈ 一个块 + 缓冲)

本包是整个系统正确性的根基。所有格式常量集中在此,后续任何包不得重复定义。

## 要做什么(任务清单)

- [ ] `go get golang.org/x/crypto`(argon2 / chacha20poly1305 / hkdf)
- [ ] `format.go`:格式常量、`Meta` 结构(定长 100 字节编解码)
- [ ] `keys.go`:`MasterKey` 类型、HKDF 密钥派生
- [ ] `keyfile.go`:创建 / 解析 / 解锁 / Rewrap
- [ ] `blob.go`:BlobWriter / BlobReader
- [ ] 错误哨兵:`ErrBadPassphrase`、`ErrBadKeyFile`、`ErrCorruptBlob`、`ErrWrongKey`
- [ ] 完整单测(清单见验收标准)

## 设计说明

### 密钥派生(keys.go)

```go
type MasterKey [32]byte
func GenerateMasterKey() (MasterKey, error)                 // crypto/rand
func DeriveFileKey(mk MasterKey, fileID [16]byte) [32]byte  // HKDF-SHA256(ikm=mk, salt=fileID,  info="kist/v1/blob")
func DeriveMetaKey(mk MasterKey) [32]byte                   // HKDF-SHA256(ikm=mk, salt="kist/v1", info="kist/v1/meta")
```

- MK 随机生成一次,所有加密数据共用(需求"所有加密文件共用一个密钥")
- 每文件独立 fileKey(MK + fileID 派生):任意两文件不可能出现"同密钥+同 nonce"组合
- 索引备份(index.enc)与普通 blob 用同一套格式

### keyfile 字节布局(110 字节)

本地 `~/.config/kist/keyfile` 与远端 `/kist/keyfile` 存同一份字节。

```
偏移 长度 字段
0    8    magic "KISTKEY1"
8    2    version u16le (=1)
10   2    flags u16le (=0)
12   16   argon2 salt(随机)
28   4    argon2 time u32le
32   4    argon2 memKiB u32le
36   1    argon2 threads u8
37   1    keyLen u8 (=32)
38   24   wrapNonce(随机)
62   48   wrappedMK = XChaCha20-Poly1305(key=KEK, pt=MK[32], aad=bytes[0..62))
          (32B 密文 + 16B tag)
```

- `KEK = argon2.IDKey(pass, salt, time, memKiB, threads, 32)`
- 解锁时 AEAD tag 校验失败 = 口令错误(或文件损坏),返回 `ErrBadPassphrase`——不需要额外 verifier 块
- `Rewrap(oldPass, newPass)`:解出 MK → 新 Argon2 参数 + 新 salt/nonce 重新包装。**只重写这 110 字节,网盘上的 blob 全部不动**

```go
type Argon2Params struct{ Time uint32; MemoryKiB uint32; Threads uint8 }
func DefaultArgon2Params() Argon2Params // {Time:1, MemoryKiB:65536, Threads:4}

func CreateKeyFile(passphrase string, p Argon2Params) (*KeyFile, MasterKey, error)
func ParseKeyFile(b []byte) (*KeyFile, error)
func (k *KeyFile) Unlock(passphrase string) (MasterKey, error)
func (k *KeyFile) Bytes() []byte
func (k *KeyFile) Rewrap(oldPass, newPass string) error
```

`CreateKeyFile` 接受自定义 Argon2 参数——**测试用小参数加速**(如 MemoryKiB=1024),生产路径一律 `DefaultArgon2Params()`。

### blob 字节布局(头部 156 字节 + 块区)

```
偏移 长度 字段
0    8    magic "KISTBLB1"
8    2    version u16le (=1)
10   2    flags u16le (=0)
12   4    metaLen u32le (=116)
16   24   metaNonce(随机)
40   116  sealedMeta = XChaCha20-Poly1305(key=metaKey, pt=Meta[100], aad=bytes[0..40))

Meta 明文(100B,整体加密):
  fileID[16] | origSize u64 | chunkSize u32 | noncePrefix[16] | plainSHA256[32]
  | mtime u64 | revision u64 | deviceID[8]     ← 后三字段普通文件恒为 0,索引备份专用

块区(自偏移 156 起,连续排列,无块头):
  第 i 块密文 = XChaCha20-Poly1305(key=fileKey, nonce=noncePrefix||u64be(i),
                aad="KISTBLB1"||fileID||u64be(i)||finalFlag)
              ‖ 明文块 + 16B tag
```

- nonce = 16B 随机前缀 + 8B 块序号(BE),拼成 XChaCha20 的 24B nonce:同文件按序号不重复,跨文件因前缀不同不重复,构造上杜绝 nonce 复用
- finalFlag:0x01 表示末块,否则 0x00——进入 AAD,防"截断后把中间块冒充末块"
- 文件名、大小、SHA 全在 sealedMeta 内,blob 本身不泄露任何元数据

### 分块规则(注意 off-by-one)

**块大小是上限,不是固定尺寸**:每个数据块明文 = `min(chunkSize, 剩余字节)`。

写出侧关键规则:**只有当缓冲超过一个块大小时,才封一个完整块写出,余量留在缓冲**;`Close()` 把剩余(可能为 0 字节)封为末块。由此:

| 明文大小 | 块数 n |
|---|---|
| 0 | 1(一个 0 字节空末块,保证 final 标记永远存在) |
| 0 < size ≤ chunk | 1 |
| size = k×chunk(整倍数) | k(最后那个整块在 Close 时才封,带 final) |
| 其他 | ⌈size/chunk⌉ |

统一为 **n = max(1, ⌈size/chunkSize⌉)**,期望密文总长 = `156 + 16n + origSize`。

### BlobWriter / BlobReader

```go
const DefaultChunkSize uint32 = 4 << 20 // 4 MiB

type EncryptOptions struct {
    ChunkSize  uint32   // 0 → DefaultChunkSize
    Mtime      uint64   // 普通文件 0;索引备份用
    Revision   uint64   // 普通文件 0;索引备份用
    DeviceID   [8]byte  // 普通文件 0;索引备份用
}

func NewBlobWriter(w io.WriteSeeker, mk MasterKey, opt EncryptOptions) (*BlobWriter, error)
func (bw *BlobWriter) Write(p []byte) (int, error)
func (bw *BlobWriter) Close() error
func (bw *BlobWriter) Meta() Meta   // Close 之后可用:上层取 fileID/plainSHA/origSize 写索引

func NewBlobReader(r io.Reader, size int64, mk MasterKey) (*BlobReader, error)
func (br *BlobReader) Meta() Meta
func (br *BlobReader) Read(p []byte) (int, error)
```

实现要点:

- **Writer**:构造时随机生成 fileID、metaNonce、noncePrefix,写出 156B 占位头(sealedMeta 区先写零)→ 持续 Write(源文件单遍读)→ Close:封末块 → Seek 回偏移 40 回填 sealedMeta。因为需要 Seek,目标是临时 `*os.File`;接口用 `io.WriteSeeker` 便于测试
- **Reader**:读 156B 头,校验 magic/version/metaLen → open sealedMeta(失败:metaKey 不对 → `ErrWrongKey`;格式坏 → `ErrCorruptBlob`)。逐块:按 `min(chunk, 剩余明文)+16B` 读密文 → open(AAD 的 finalFlag 取 `i == n-1`)。EOF 时执行终检:底层无多余字节、累计明文大小 == origSize、流式 SHA-256 == plainSHA256
- Reader 的 `size` 参数是密文总长(临时文件 stat 或 Content-Length),用于预先计算 n 并做总长校验

## 预期结果

`internal/crypto` 包可独立编译,不 import 项目内其他任何包;`go doc kist/internal/crypto` 能看到上述完整 API。

## 验收标准

1. `go build ./... && go vet ./...` 通过
2. `go test ./internal/crypto/ -count=1` 全绿,用例至少覆盖:
   - **keyfile**:往返(Bytes→Parse→Unlock 得同一 MK)/ 错口令返回 `ErrBadPassphrase` / 篡改头部与 wrapped 区各任一字节 → 解析或解锁失败 / Rewrap 后旧口令失效、新口令解锁、MK 不变
   - **blob 往返**:size ∈ {0, 1, chunk−1, chunk, chunk+1, 2×chunk, 3×chunk+123};解密字节与源一致;Meta 的 origSize/plainSHA/fileID 正确;**size = k×chunk 时块数恰为 k**(回归 off-by-one)
   - **篡改检测**:头部/meta 区/首块/中间块/末块/任一 tag 各翻 1 字节 → 失败
   - **截断检测**:去掉末尾 1 字节 / 去掉整个末块 / 去掉中间整块 → 失败
   - **结构攻击**:交换两个中间块 → 失败;末尾追加垃圾 → 失败;手工构造"中间块冒充末块" → 失败
   - **错误 MK**:NewBlobReader 直接失败
   - **流式**:≥ 40MB 文件加密+解密,`runtime.MemStats` HeapInuse 增量 < 64MiB
3. `go test ./internal/crypto/ -race -count=1` 通过
