package crypto

import "errors"

// 包级错误哨兵;上层(internal/errs)据此映射用户可读文案。
var (
	// ErrBadPassphrase 口令错误(keyfile 解包认证失败,也可能是 keyfile 损坏)
	ErrBadPassphrase = errors.New("crypto: 口令错误或密钥文件损坏")
	// ErrBadKeyFile keyfile 长度/magic/版本等静态校验不通过
	ErrBadKeyFile = errors.New("crypto: 密钥文件格式不合法")
	// ErrWrongKey 主密钥不符(最常见于指向了另一个账户的网盘),也可能是头部损坏
	ErrWrongKey = errors.New("crypto: 主密钥不符或文件头部损坏")
	// ErrCorruptBlob blob 数据损坏:认证失败、长度不符、终校验不过
	ErrCorruptBlob = errors.New("crypto: 加密文件损坏")
)
