package errs

import (
	"database/sql"
	"errors"

	"kist/internal/crypto"
)

// 应用错误码:CLI 与 GUI 共用,前端按 Code 映射可读文案。
const (
	BadConfig     = "BAD_CONFIG"     // 配置缺失或非法
	NotConfigured = "NOT_CONFIGURED" // 尚未执行 config set / init
	DavError      = "DAV_ERROR"      // WebDAV 通信失败
	AuthFailed    = "AUTH_FAILED"    // 口令错误
	NotFound      = "NOT_FOUND"      // 索引里找不到目标
	Corrupt       = "CORRUPT"        // 数据/密钥文件损坏或密钥不符
	Locked        = "LOCKED"         // 未解锁就发起传输
	Internal      = "INTERNAL"       // 其余内部错误
)

// AppError 实现 error 与 Unwrap:保留原始错误链(errors.Is/As 仍可用)。
type AppError struct {
	Code  string
	Msg   string
	cause error
}

func (e *AppError) Error() string { return e.Msg }
func (e *AppError) Unwrap() error { return e.cause }

func New(code, msg string) error { return &AppError{Code: code, Msg: msg} }

func Wrap(code string, err error) error {
	return &AppError{Code: code, Msg: err.Error(), cause: err}
}

// From 把任意错误归类为 AppError:已知哨兵映射到稳定错误码,
// 已经是 AppError 的原样返回,其余归 INTERNAL。
func From(err error) error {
	if err == nil {
		return nil
	}
	var ae *AppError
	if errors.As(err, &ae) {
		return err
	}
	switch {
	case errors.Is(err, crypto.ErrBadPassphrase):
		return Wrap(AuthFailed, err)
	case errors.Is(err, crypto.ErrBadKeyFile),
		errors.Is(err, crypto.ErrWrongKey),
		errors.Is(err, crypto.ErrCorruptBlob):
		return Wrap(Corrupt, err)
	case errors.Is(err, sql.ErrNoRows):
		return Wrap(NotFound, err)
	default:
		return Wrap(Internal, err)
	}
}
