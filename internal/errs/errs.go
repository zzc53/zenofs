// Package errs 定义系统统一的错误码和错误类型。
//
// 所有通过 API 返回的错误都封装为 ZenoError，包含数值码、
// 字符串码和可读消息，便于客户端解析和定位。
package errs

import "fmt"

// ── 错误码（数值） ──

const (
	ECODE_DB_BAD_DSN   = 1  // 数据库连接串格式不支持
	ECODE_DB_BAD_CONN  = 2  // 数据库连接失败
	ECODE_DB_BAD_QUERY = 3  // 数据库查询执行错误

	ECODE_POOL_BAD_NAME    = 4  // Pool 名称重复
	ECODE_POOL_BAD         = 5  // Pool 操作参数错误
	ECODE_DISK_BAD_BACKEND = 6  // 不支持的磁盘后端类型
	ECODE_DISK_BAD_TYPE    = 7  // 不支持的磁盘角色类型
	ECODE_DISK_OFFLINE     = 8  // 磁盘不在线
	ECODE_POOL_OFFLINE     = 9  // Pool 不在线

	ECODE_CRYPTO_ERROR = 10 // 随机数生成或加密失败
	ECODE_FILE_WRITE   = 11 // 文件读写错误

	ECODE_CHUNK_EMPTY       = 12 // 写入空数据
	ECODE_CHUNK_SIZE_EXCEED = 13 // chunk 数据超过大小限制
	ECODE_CHUNK_NOT_FOUND   = 14 // chunk 不存在或不属于指定 pool
)

// ── 错误码（字符串） ──

const (
	ESTR_DB_BAD_DSN   = "DB_BAD_DSN"
	ESTR_DB_BAD_CONN  = "DB_BAD_CONN"
	ESTR_DB_BAD_QUERY = "DB_BAD_QUERY"

	ESTR_POOL_BAD_NAME    = "POOL_BAD_NAME"
	ESTR_POOL_BAD         = "POOL_BAD"
	ESTR_DISK_BAD_BACKEND = "DISK_BAD_BACKEND"
	ESTR_DISK_BAD_TYPE    = "DISK_BAD_TYPE"
	ESTR_DISK_OFFLINE     = "DISK_OFFLINE"
	ESTR_POOL_OFFLINE     = "POOL_OFFLINE"

	ESTR_CRYPTO_ERROR = "CRYPTO_ERROR"
	ESTR_FILE_WRITE   = "FILE_WRITE"

	ESTR_CHUNK_EMPTY       = "CHUNK_ALLOC"
	ESTR_CHUNK_SIZE_EXCEED = "CHUNK_SIZE_EXCEED"
	ESTR_CHUNK_NOT_FOUND   = "CHUNK_NOT_FOUND"
)

// ZenoError 是系统的标准错误类型，包含数值码、字符串码和上下文。
type ZenoError struct {
	Code     int    // 数值错误码，适合程序判断
	StrCode  string // 字符串错误码，适合日志/API 响应
	InnerErr error  // 内部原始错误（可为 nil）
	Message  string // 人类可读的错误描述
	Value    string // 关联的上下文值（如出错的 ID）
}

// FromError 包装一个已有 error 为 ZenoError。
// 用于将标准库或第三方错误转换为系统统一错误。
func FromError(e error, code int, strCode string) *ZenoError {
	return &ZenoError{Code: code, StrCode: strCode, InnerErr: e}
}

// New 创建一个新的 ZenoError。
// msg 是描述信息，val 是可选的上下文值。
func New(code int, strCode string, msg string, val string) *ZenoError {
	return &ZenoError{Code: code, StrCode: strCode, Message: msg, Value: val}
}

// Error 实现 error 接口。
func (e *ZenoError) Error() string {
	if e.InnerErr != nil {
		return fmt.Sprintf("%s", e.InnerErr)
	}
	if e.Value != "" {
		return fmt.Sprintf("%s: %s", e.Message, e.Value)
	}
	return e.Message
}
