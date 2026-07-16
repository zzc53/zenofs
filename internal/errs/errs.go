package errs

import "fmt"

const (
	ECODE_DB_BAD_DSN   = 1
	ECODE_DB_BAD_CONN  = 2
	ECODE_DB_BAD_QUERY = 3

	ECODE_POOL_BAD_NAME    = 4
	ECODE_POOL_BAD         = 5
	ECODE_DISK_BAD_BACKEND = 6
	ECODE_DISK_BAD_TYPE    = 7
	ECODE_DISK_OFFLINE     = 8
	ECODE_POOL_OFFLINE     = 9
	ECODE_STRIPE_FULL      = 10

	ECODE_CRYPTO_ERROR = 11
	ECODE_FILE_WRITE   = 12

	ECODE_CHUNK_EMPTY       = 13
	ECODE_CHUNK_SIZE_EXCEED = 14
	ECODE_CHUNK_NOT_FOUND   = 15
)

const (
	ESTR_DB_BAD_DSN   = "DB_BAD_DSN"
	ESTR_DB_BAD_CONN  = "DB_BAD_CONN"
	ESTR_DB_BAD_QUERY = "DB_BAD_QUERY"

	ESTR_POOL_BAD_NAME    = "POOL_BAD_NAME"
	ESTR_POOL_BAD         = "POOL_BAD_NAME"
	ESTR_DISK_BAD_BACKEND = "DISK_BAD_BACKEND"
	ESTR_DISK_BAD_TYPE    = "DISK_BAD_TYPE"
	ESTR_DISK_OFFLINE     = "DISK_OFFLINE"
	ESTR_POOL_OFFLINE     = "POOL_OFFLINE"
	ESTR_STRIPE_FULL      = "STRIPE_FULL"

	ESTR_CRYPTO_ERROR = "CRYPTO_ERROR"
	ESTR_FILE_WRITE   = "FILE_WRITE"

	ESTR_CHUNK_EMPTY       = "CHUNK_ALLOC"
	ESTR_CHUNK_SIZE_EXCEED = "CHUNK_SIZE_EXCEED"
	ESTR_CHUNK_NOT_FOUND   = "CHUNK_NOT_FOUND"
)

type ZenoError struct {
	Code     int
	StrCode  string
	InnerErr error
	Message  string
	Value    string
}

func FromError(e error, code int, strCode string) *ZenoError {
	return &ZenoError{Code: code, StrCode: strCode, InnerErr: e}
}

func New(code int, strCode string, msg string, val string) *ZenoError {
	return &ZenoError{Code: code, StrCode: strCode, Message: msg, Value: val}
}
func (e *ZenoError) Error() string {
	if e.InnerErr != nil {
		return fmt.Sprintf("%s", e.InnerErr)
	}
	if e.Value != "" {
		return fmt.Sprintf("%s: %s", e.Message, e.Value)
	}
	return e.Message
}
