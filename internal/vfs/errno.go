package vfs

import (
	"errors"
	"syscall"
)

// Errno 把 vfs 的 POSIX 语义哨兵错误翻成对应的 syscall.Errno。
//
// 供基于 POSIX errno 的协议层使用：SFTP 用它推出 SSH_FX_* 状态码，
// WebDAV 用它喂给 x/net/webdav（那里大量用 os.IsNotExist / os.IsExist 判定，
// 而 syscall.ENOENT / EEXIST 都能被这些函数识别）。
//
// 认不出来的错误原样返回，调用方按"未知错误"处理（SFTP 回 SSH_FX_FAILURE、
// WebDAV 回 500）。
func Errno(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNotExist):
		return syscall.ENOENT
	case errors.Is(err, ErrExist):
		return syscall.EEXIST
	case errors.Is(err, ErrPermission), errors.Is(err, ErrEncrypted):
		return syscall.EACCES
	case errors.Is(err, ErrReadOnly):
		return syscall.EROFS
	case errors.Is(err, ErrNotEmpty):
		return syscall.ENOTEMPTY
	case errors.Is(err, ErrIsDir):
		return syscall.EISDIR
	case errors.Is(err, ErrNotDir):
		return syscall.ENOTDIR
	case errors.Is(err, ErrNoSpace):
		return syscall.ENOSPC
	case errors.Is(err, ErrNotSupported):
		return syscall.ENOSYS
	case errors.Is(err, ErrBusy):
		return syscall.EBUSY
	case errors.Is(err, ErrNameTooLong):
		return syscall.ENAMETOOLONG
	case errors.Is(err, ErrLoop):
		return syscall.ELOOP
	case errors.Is(err, ErrCrossDevice):
		return syscall.EXDEV
	case errors.Is(err, ErrInvalid):
		return syscall.EINVAL
	default:
		return err
	}
}
