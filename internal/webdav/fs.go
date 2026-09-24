package webdav

import (
	"context"
	"os"
	"path"
	"strings"
	"syscall"

	"golang.org/x/net/webdav"

	"github.com/zzc53/zenofs/internal/vfs"
)

var _ webdav.FileSystem = (*davFS)(nil)

// maxRemoveDepth 是递归删除的最大层数，兜住异常数据形成的环。
const maxRemoveDepth = 64

// davFS 把 vfs.RootFS 适配成 x/net/webdav 的 FileSystem。
//
// 每个方法都从 context 里取认证后的用户 id（由 Handler.ServeHTTP 注入）：
// 取不到就直接返回权限错误，碰不到任何数据。
//
// 返回的错误统一经过 vfs.Errno 翻成 POSIX errno——x/net 大量用
// os.IsNotExist / os.IsExist 判定，而 syscall.ENOENT / EEXIST 都能被识别。
type davFS struct{ h *Handler }

// resolve 取出该用户在当前请求下的挂载点，并校验、规范化路径。
func (fs *davFS) resolve(ctx context.Context, name string) (*vfs.RootFS, string, error) {
	userID, ok := ctx.Value(userIDKey{}).(int64)
	if !ok {
		return nil, "", syscall.EACCES
	}
	clean, err := cleanPath(name)
	if err != nil {
		return nil, "", err
	}
	return fs.h.root(userID), clean, nil
}

// cleanPath 校验 WebDAV 路径：必须是绝对路径，且不含 "."/".." 段或 NUL。
// 客户端（或中间设备）不该发这种路径，发了就是越权尝试，直接拒绝。
//
// 空串来自"请求的正是挂载点本身"（x/net 剥掉 "/dav" 前缀后为空），按根处理。
func cleanPath(name string) (string, error) {
	if name == "" {
		return "/", nil
	}
	if !strings.HasPrefix(name, "/") {
		return "", syscall.EINVAL
	}
	if strings.ContainsRune(name, '\x00') {
		return "", syscall.EINVAL
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "." || seg == ".." {
			return "", syscall.EINVAL
		}
	}
	clean := path.Clean(name)
	if clean == "." {
		clean = "/"
	}
	return clean, nil
}

// Mkdir 实现 webdav.FileSystem（MKCOL）。perm 里只有可执行位对 ZenoFS 有意义。
func (fs *davFS) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	root, clean, err := fs.resolve(ctx, name)
	if err != nil {
		return err
	}
	return vfs.Errno(root.Mkdir(ctx, clean, perm))
}

// OpenFile 实现 webdav.FileSystem（GET/PUT 与列目录都走它）。
func (fs *davFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	root, clean, err := fs.resolve(ctx, name)
	if err != nil {
		return nil, err
	}

	// 目录：x/net 用 O_RDONLY 打开目录再 Readdir 列条目；对目录提写要求是错的。
	if info, statErr := root.Stat(ctx, clean); statErr == nil && info.Kind == vfs.KindDir {
		if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC) != 0 {
			return nil, syscall.EISDIR
		}
		return &dirFile{fs: root, name: clean, info: fileInfo{info}}, nil
	}

	f, err := root.Open(ctx, clean, openFlags(flag), perm)
	if err != nil {
		return nil, vfs.Errno(err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, vfs.Errno(err)
	}
	return &davFile{file: f, info: fileInfo{info}}, nil
}

// RemoveAll 实现 webdav.FileSystem：递归删除（DELETE 带 Depth: infinity 用它）。
func (fs *davFS) RemoveAll(ctx context.Context, name string) error {
	root, clean, err := fs.resolve(ctx, name)
	if err != nil {
		return err
	}
	if clean == "/" {
		// 根是合成的只读视图，删不得——也避免一条 DELETE 清空所有 Share。
		return syscall.EACCES
	}
	return vfs.Errno(removeAll(ctx, root, clean, 0))
}

// removeAll 递归删除目录树。用 Lstat 判类型：符号链接只删链接本身，不跟进目标。
func removeAll(ctx context.Context, root *vfs.RootFS, name string, depth int) error {
	if depth > maxRemoveDepth {
		return vfs.ErrLoop
	}
	info, err := root.Lstat(ctx, name)
	if err != nil {
		return err
	}
	if info.Kind != vfs.KindDir {
		return root.Remove(ctx, name)
	}
	entries, err := root.ReadDir(ctx, name)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := removeAll(ctx, root, path.Join(name, e.Name), depth+1); err != nil {
			return err
		}
	}
	return root.Remove(ctx, name)
}

// Rename 实现 webdav.FileSystem（MOVE）。跨 Share 返回 ErrCrossDevice → 403。
func (fs *davFS) Rename(ctx context.Context, oldName, newName string) error {
	root, oldClean, err := fs.resolve(ctx, oldName)
	if err != nil {
		return err
	}
	newClean, err := cleanPath(newName)
	if err != nil {
		return err
	}
	return vfs.Errno(root.Rename(ctx, oldClean, newClean))
}

// Stat 实现 webdav.FileSystem。
func (fs *davFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	root, clean, err := fs.resolve(ctx, name)
	if err != nil {
		return nil, err
	}
	info, err := root.Stat(ctx, clean)
	if err != nil {
		return nil, vfs.Errno(err)
	}
	return fileInfo{info}, nil
}

// openFlags 把 os.O_* 翻成 vfs 的打开意图。
func openFlags(flag int) vfs.OpenFlags {
	return vfs.OpenFlags{
		Read:      flag&os.O_WRONLY == 0, // O_RDONLY / O_RDWR 都能读
		Write:     flag&(os.O_WRONLY|os.O_RDWR) != 0,
		Create:    flag&os.O_CREATE != 0,
		Exclusive: flag&os.O_EXCL != 0,
		Truncate:  flag&os.O_TRUNC != 0,
		Append:    flag&os.O_APPEND != 0,
	}
}
