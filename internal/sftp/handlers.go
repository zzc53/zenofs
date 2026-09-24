package sftp

import (
	"context"
	"errors"
	"io"
	"os"
	"syscall"
	"time"

	psftp "github.com/pkg/sftp"

	"github.com/zzc53/zenofs/internal/vfs"
)

// handlers 把 zenofs 的 RootFS 适配成 pkg/sftp 的请求处理器（一个连接一份）。
//
// 路径语义直接复用 vfs：SFTP 的根 "/" 就是该用户的 Share 列表，
// "/<share>/..." 是各 Share 内部的路径。
type handlers struct {
	root *vfs.RootFS
}

// 需要实现的能力：读写句柄、命令、列目录，外加几个可选扩展
// （OpenFile 让读写共用句柄、PosixRename 允许覆盖、StatVFS 报配额、
// Lstat/Readlink 走不跟随符号链接的语义）。
var (
	_ psftp.FileReader           = (*handlers)(nil)
	_ psftp.FileWriter           = (*handlers)(nil)
	_ psftp.OpenFileWriter       = (*handlers)(nil)
	_ psftp.FileCmder            = (*handlers)(nil)
	_ psftp.PosixRenameFileCmder = (*handlers)(nil)
	_ psftp.StatVFSFileCmder     = (*handlers)(nil)
	_ psftp.FileLister           = (*handlers)(nil)
	_ psftp.LstatFileLister      = (*handlers)(nil)
	_ psftp.ReadlinkFileLister   = (*handlers)(nil)
)

// defaultFileMode 是新建文件的权限。ZenoFS 只区分"是否可执行"，
// 读写权限由 share_users 决定。
const defaultFileMode vfs.FileMode = 0o644

// ─────────────────────────────────────────────────────────────
// 读写句柄
// ─────────────────────────────────────────────────────────────

// Fileread 服务于 SSH_FXP_GET：返回一个可读句柄（vfs.File 本身就是 ReaderAt）。
func (h *handlers) Fileread(r *psftp.Request) (io.ReaderAt, error) {
	return h.open(r)
}

// Filewrite 服务于 SSH_FXP_PUT：返回一个可写句柄。
func (h *handlers) Filewrite(r *psftp.Request) (io.WriterAt, error) {
	return h.open(r)
}

// OpenFile 服务于 SSH_FXP_OPEN：vfs 的句柄天生读写共用，直接交给库。
func (h *handlers) OpenFile(r *psftp.Request) (psftp.WriterAtReaderAt, error) {
	return h.open(r)
}

// open 把 SSH_FXP_OPEN 的 pflags 翻译成 vfs.OpenFlags。
//
// 返回的 vfs.File 同时满足 io.ReaderAt / io.WriterAt / io.Closer，
// pkg/sftp 会在句柄关闭时调用 Close（vfs 在 Close 里提交新版本）。
func (h *handlers) open(r *psftp.Request) (vfs.File, error) {
	flags := r.Pflags()
	if !flags.Read && !flags.Write {
		// 少数客户端只发 CREAT/TRUNC 不带读写位，按只读处理更安全。
		flags.Read = true
	}
	f, err := h.root.Open(r.Context(), r.Filepath, vfs.OpenFlags{
		Read:      flags.Read,
		Write:     flags.Write,
		Create:    flags.Creat,
		Exclusive: flags.Excl,
		Truncate:  flags.Trunc,
		Append:    flags.Append,
	}, defaultFileMode)
	if err != nil {
		return nil, vfs.Errno(err)
	}
	return f, nil
}

// ─────────────────────────────────────────────────────────────
// 命令
// ─────────────────────────────────────────────────────────────

// Filecmd 处理 Setstat/Rename/Rmdir/Mkdir/Symlink/Remove。
func (h *handlers) Filecmd(r *psftp.Request) error {
	ctx := r.Context()
	switch r.Method {
	case "Setstat":
		attrs, err := attrsOf(r)
		if err != nil {
			return err
		}
		return vfs.Errno(h.root.SetAttr(ctx, r.Filepath, attrs))

	case "Rename":
		// SFTP v3：目标已存在时改名必须失败（覆盖语义只由 posix-rename 扩展提供）。
		if _, err := h.root.Stat(ctx, r.Target); err == nil {
			return syscall.EEXIST
		} else if !errors.Is(err, vfs.ErrNotExist) {
			return vfs.Errno(err)
		}
		return vfs.Errno(h.root.Rename(ctx, r.Filepath, r.Target))

	case "Rmdir", "Remove":
		return vfs.Errno(h.root.Remove(ctx, r.Filepath))

	case "Mkdir":
		return vfs.Errno(h.root.Mkdir(ctx, r.Filepath, modeOf(r)))

	case "Symlink":
		// 与 POSIX 一致：Filepath 是目标内容，Target 是新建的链接路径。
		return vfs.Errno(h.root.Symlink(ctx, r.Filepath, r.Target))

	case "Link":
		// ZenoFS 没有硬链接。
		return syscall.EPERM
	}
	return syscall.ENOSYS
}

// PosixRename 是 posix-rename@openssh.com 扩展：允许覆盖已存在的目标。
func (h *handlers) PosixRename(r *psftp.Request) error {
	return vfs.Errno(h.root.Rename(r.Context(), r.Filepath, r.Target))
}

// StatVFS 是 statvfs@openssh.com 扩展：报 Share 的配额。
func (h *handlers) StatVFS(r *psftp.Request) (*psftp.StatVFS, error) {
	info, err := h.root.StatFS(r.Context(), r.Filepath)
	if err != nil {
		return nil, vfs.Errno(err)
	}
	const bsize uint64 = 4096
	blocks := info.TotalBytes / bsize
	bfree := info.FreeBytes / bsize
	return &psftp.StatVFS{
		Bsize:   bsize,
		Frsize:  bsize,
		Blocks:  blocks,
		Bfree:   bfree,
		Bavail:  bfree,
		Files:   info.TotalFiles,
		Ffree:   info.FreeFiles,
		Favail:  info.FreeFiles,
		Namemax: 255,
	}, nil
}

// ─────────────────────────────────────────────────────────────
// 列目录与属性
// ─────────────────────────────────────────────────────────────

// Filelist 处理 List 与 Stat。
func (h *handlers) Filelist(r *psftp.Request) (psftp.ListerAt, error) {
	ctx := r.Context()
	switch r.Method {
	case "List":
		entries, err := h.root.ReadDir(ctx, r.Filepath)
		if err != nil {
			return nil, vfs.Errno(err)
		}
		out := make(listerAt, 0, len(entries))
		for _, e := range entries {
			out = append(out, fileInfo{e})
		}
		return out, nil

	case "Stat":
		fi, err := h.root.Stat(ctx, r.Filepath)
		if err != nil {
			return nil, vfs.Errno(err)
		}
		return listerAt{fileInfo{fi}}, nil

	case "Readlink":
		fi, err := h.root.Lstat(ctx, r.Filepath)
		if err != nil {
			return nil, vfs.Errno(err)
		}
		return listerAt{fileInfo{fi}}, nil
	}
	return nil, syscall.ENOSYS
}

// Lstat 服务于 SSH_FXP_LSTAT：不跟随末端的符号链接。
func (h *handlers) Lstat(r *psftp.Request) (psftp.ListerAt, error) {
	fi, err := h.root.Lstat(r.Context(), r.Filepath)
	if err != nil {
		return nil, vfs.Errno(err)
	}
	return listerAt{fileInfo{fi}}, nil
}

// Readlink 服务于 SSH_FXP_READLINK：返回链接目标的原始字符串。
func (h *handlers) Readlink(path string) (string, error) {
	target, err := h.root.Readlink(context.Background(), path)
	if err != nil {
		return "", vfs.Errno(err)
	}
	return target, nil
}

// ─────────────────────────────────────────────────────────────
// 适配类型
// ─────────────────────────────────────────────────────────────

// listerAt 是 pkg/sftp 的分页列目录接口的简单实现。
type listerAt []os.FileInfo

func (l listerAt) ListAt(dst []os.FileInfo, offset int64) (int, error) {
	if offset >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(dst, l[offset:])
	if n < len(dst) {
		return n, io.EOF
	}
	return n, nil
}

// fileInfo 把 vfs 的元数据适配成 os.FileInfo。
// Uid/Gid 通过 pkg/sftp 的 FileInfoUidGid 约定暴露。
type fileInfo struct {
	vfs.FileInfo
}

func (f fileInfo) Name() string       { return f.FileInfo.Name }
func (f fileInfo) Size() int64        { return f.FileInfo.Size }
func (f fileInfo) Mode() os.FileMode  { return f.FileInfo.Mode } // 已含类型位
func (f fileInfo) ModTime() time.Time { return f.FileInfo.Mtime }
func (f fileInfo) IsDir() bool        { return f.FileInfo.Kind == vfs.KindDir }
func (f fileInfo) Sys() any           { return nil }
func (f fileInfo) Uid() uint32        { return f.FileInfo.Uid }
func (f fileInfo) Gid() uint32        { return f.FileInfo.Gid }

// ─────────────────────────────────────────────────────────────
// 转换
// ─────────────────────────────────────────────────────────────

// attrsOf 把 SSH_FXP_SETSTAT 的属性翻译成 vfs.Attrs（只取设置了的字段）。
func attrsOf(r *psftp.Request) (vfs.Attrs, error) {
	var out vfs.Attrs
	flags := r.AttrFlags()
	attrs := r.Attributes()
	if flags.Size {
		size := int64(attrs.Size)
		out.Size = &size
	}
	if flags.Permissions {
		mode := vfs.FileMode(attrs.Mode & 0o777)
		out.Mode = &mode
	}
	if flags.Acmodtime {
		atime := time.Unix(int64(attrs.Atime), 0)
		mtime := time.Unix(int64(attrs.Mtime), 0)
		out.Atime = &atime
		out.Mtime = &mtime
	}
	return out, nil
}

// modeOf 取 SSH_FXP_MKDIR 带的权限，默认 0755。
func modeOf(r *psftp.Request) vfs.FileMode {
	if r.AttrFlags().Permissions {
		if perm := r.Attributes().Mode & 0o777; perm != 0 {
			return vfs.FileMode(perm)
		}
	}
	return 0o755
}
