// Package errs 定义系统统一的错误码和错误类型。
//
// 所有通过 API 返回的错误都封装为 ZenoError，包含数值码、
// 字符串码和可读消息，便于客户端解析和定位。
package errs

// ── 错误码（数值） ──

const (
	ECODE_DB_BAD_DSN   = 1 // 数据库连接串格式不支持
	ECODE_DB_BAD_CONN  = 2 // 数据库连接失败
	ECODE_DB_BAD_QUERY = 3 // 数据库查询执行错误

	ECODE_POOL_BAD_NAME    = 4 // Pool 名称重复
	ECODE_POOL_BAD         = 5 // Pool 操作参数错误
	ECODE_DISK_BAD_BACKEND = 6 // 不支持的磁盘后端类型
	ECODE_DISK_BAD_TYPE    = 7 // 不支持的磁盘角色类型
	ECODE_DISK_OFFLINE     = 8 // 磁盘不在线
	ECODE_POOL_OFFLINE     = 9 // Pool 不在线

	ECODE_CRYPTO_ERROR = 10 // 随机数生成或加密失败
	ECODE_FILE_WRITE   = 11 // 文件读写错误

	ECODE_CHUNK_EMPTY       = 12 // 写入空数据
	ECODE_CHUNK_SIZE_EXCEED = 13 // chunk 数据超过大小限制
	ECODE_CHUNK_NOT_FOUND   = 14 // chunk 不存在或不属于指定 pool

	// vfs 的 POSIX 语义错误（internal/vfs 的 ErrNotExist 等哨兵用它）。
	// 协议层（SFTP / SMB / WebDAV）可以按 Code 直接回各自的状态码，不必解析错误文本。
	ECODE_VFS_NOT_FOUND     = 15 // 路径或条目不存在
	ECODE_VFS_EXIST         = 16 // 目标已存在
	ECODE_VFS_PERMISSION    = 17 // 权限不足（只读挂载点上的写操作等）
	ECODE_VFS_NOT_EMPTY     = 18 // 目录非空
	ECODE_VFS_IS_DIR        = 19 // 目标（或源）是目录
	ECODE_VFS_NOT_DIR       = 20 // 目标不是目录
	ECODE_VFS_READ_ONLY     = 21 // 只读文件系统
	ECODE_VFS_NO_SPACE      = 22 // 空间不足（超出 Share 配额）
	ECODE_VFS_INVALID       = 23 // 参数非法（路径格式、偏移、名称等）
	ECODE_VFS_NOT_SUPPORTED = 24 // 该挂载点不支持的操作
	ECODE_VFS_BUSY          = 25 // 资源忙（如锁冲突）
	ECODE_VFS_NAME_TOO_LONG = 26 // 名称超过长度上限
	ECODE_VFS_LOOP          = 27 // 符号链接层数过多
	ECODE_VFS_CROSS_DEVICE  = 28 // 跨挂载点操作（跨 Share 的 Rename/Copy）
	ECODE_VFS_ENCRYPTED     = 29 // Share 启用了加密，但会话里没有可用密钥（要用口令打开）

	// 访问凭证（internal/token）的错误。
	ECODE_TOKEN_INVALID   = 30 // 凭证无效（token 摘要对不上）
	ECODE_TOKEN_EXPIRED   = 31 // 凭证已过期
	ECODE_TOKEN_NOT_FOUND = 32 // 凭证不存在
	ECODE_TOKEN_BAD_KEY   = 33 // SSH 公钥格式非法
	ECODE_TOKEN_BAD_USER  = 34 // 归属用户不存在
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

	ESTR_CHUNK_EMPTY       = "CHUNK_EMPTY"
	ESTR_CHUNK_SIZE_EXCEED = "CHUNK_SIZE_EXCEED"
	ESTR_CHUNK_NOT_FOUND   = "CHUNK_NOT_FOUND"

	// vfs 的 POSIX 语义错误（与 ECODE_VFS_* 一一对应）
	ESTR_VFS_NOT_FOUND     = "VFS_NOT_FOUND"
	ESTR_VFS_EXIST         = "VFS_EXIST"
	ESTR_VFS_PERMISSION    = "VFS_PERMISSION"
	ESTR_VFS_NOT_EMPTY     = "VFS_NOT_EMPTY"
	ESTR_VFS_IS_DIR        = "VFS_IS_DIR"
	ESTR_VFS_NOT_DIR       = "VFS_NOT_DIR"
	ESTR_VFS_READ_ONLY     = "VFS_READ_ONLY"
	ESTR_VFS_NO_SPACE      = "VFS_NO_SPACE"
	ESTR_VFS_INVALID       = "VFS_INVALID"
	ESTR_VFS_NOT_SUPPORTED = "VFS_NOT_SUPPORTED"
	ESTR_VFS_BUSY          = "VFS_BUSY"
	ESTR_VFS_NAME_TOO_LONG = "VFS_NAME_TOO_LONG"
	ESTR_VFS_LOOP          = "VFS_LOOP"
	ESTR_VFS_CROSS_DEVICE  = "VFS_CROSS_DEVICE"
	ESTR_VFS_ENCRYPTED     = "VFS_ENCRYPTED"

	// 访问凭证（与 ECODE_TOKEN_* 一一对应）
	ESTR_TOKEN_INVALID   = "TOKEN_INVALID"
	ESTR_TOKEN_EXPIRED   = "TOKEN_EXPIRED"
	ESTR_TOKEN_NOT_FOUND = "TOKEN_NOT_FOUND"
	ESTR_TOKEN_BAD_KEY   = "TOKEN_BAD_KEY"
	ESTR_TOKEN_BAD_USER  = "TOKEN_BAD_USER"
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

// DBQuery 把数据库查询/写入错误包装成统一的 ZenoError（ECODE_DB_BAD_QUERY）。
func DBQuery(err error) *ZenoError {
	return FromError(err, ECODE_DB_BAD_QUERY, ESTR_DB_BAD_QUERY)
}

// Error 实现 error 接口。
// 优先暴露内部原始错误；否则拼上上下文值，便于定位。
func (e *ZenoError) Error() string {
	if e.InnerErr != nil {
		return e.InnerErr.Error()
	}
	if e.Value != "" {
		return e.Message + ": " + e.Value
	}
	return e.Message
}

// Unwrap 暴露被包装的原始错误，让 errors.Is / errors.As 能穿透到它。
// 例：vfs 把 POSIX 哨兵错误包成 ZenoError 后，调用方仍可用 errors.Is 判定。
func (e *ZenoError) Unwrap() error { return e.InnerErr }

// Is 让 errors.Is 按错误码比较两个 ZenoError：同码即视为同一类错误。
//
// 这样 FromError 包装出来的 ZenoError（带原始错误）与 errs.New 造的哨兵实例
// 也能匹配上：errors.Is(FromError(err, ECODE_TOKEN_BAD_KEY, ...), token.ErrBadKey) 为真。
// 按码比较是刻意的——错误码就是这个包对外的契约，实例指针不是。
func (e *ZenoError) Is(target error) bool {
	t, ok := target.(*ZenoError)
	if !ok {
		return false
	}
	return e.Code == t.Code
}
