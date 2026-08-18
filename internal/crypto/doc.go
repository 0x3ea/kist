// Package crypto 实现 kist 的加密核心。
//
// 两部分:
//   - keyfile:用户口令 → Argon2id → KEK,用于包装/解包随机主密钥 MK(所有加密数据共用);
//   - blob:流式分块加解密(XChaCha20-Poly1305,默认 4MiB 一块),内存占用与文件大小无关,
//     并能检测篡改/重排/截断/追加。
//
// 本包不依赖项目内任何其他包;所有格式常量仅在此定义。
// 设计文档:docs/phase-1-crypto.md
package crypto
