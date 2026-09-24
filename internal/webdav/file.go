package webdav

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"path"
	"syscall"
	"time"

	"golang.org/x/net/webdav"

	"github.com/zzc53/zenofs/internal/vfs"
)

var (
	_ webdav.File = (*davFile)(nil)
	_ webdav.File = (*dirFile)(nil)
)

// fileInfo 把 vfs 的元数据适配成 os.FileInfo。
//
// 顺带实现 x/net/webdav 的两个可选接口：ETag（PROPFIND 的 getetag 与
// If-Match 条件请求都靠它）和 ContentType（省掉"为了猜类型而读文件内容"）。
type fileInfo struct{ vfs.FileInfo }

func (f fileInfo) Name() string       { return f.FileInfo.Name }
func (f fileInfo) Size() int64        { return f.FileInfo.Size }
func (f fileInfo) Mode() fs.FileMode  { return f.FileInfo.Mode } // 已含类型位
func (f fileInfo) ModTime() time.Time { return f.FileInfo.Mtime }
func (f fileInfo) IsDir() bool        { return f.FileInfo.Kind == vfs.KindDir }
func (f fileInfo) Sys() any           { return nil }

// ETag 由 inode id + 修改时间合成：内容或元数据一变就变，且不同资源互不相同。
func (f fileInfo) ETag(context.Context) (string, error) {
	return fmt.Sprintf(`"%x-%x"`, f.FileInfo.Id, f.FileInfo.Mtime.Unix()), nil
}

// ContentType 按扩展名给类型；认不出来时回空串，由 http.ServeContent 按名字猜。
func (f fileInfo) ContentType(context.Context) (string, error) {
	if f.FileInfo.Kind == vfs.KindDir {
		return "httpd/unix-directory", nil
	}
	return mime.TypeByExtension(path.Ext(f.FileInfo.Name)), nil
}

// davFile 是普通文件的 webdav.File：读写、Seek、Stat 全部转发给 vfs.File。
type davFile struct {
	file vfs.File
	info fileInfo
}

func (f *davFile) Read(p []byte) (int, error) { return f.file.Read(p) }

func (f *davFile) Write(p []byte) (int, error) { return f.file.Write(p) }

func (f *davFile) Seek(offset int64, whence int) (int64, error) {
	return f.file.Seek(offset, whence)
}

// Close 关闭句柄（vfs 在这里提交新版本）并顺手刷新一次读者看到的元数据。
func (f *davFile) Close() error { return vfs.Errno(f.file.Close()) }

// Stat 返回最新元数据（写入后大小会变，x/net 在 PUT 结束时会读它）。
func (f *davFile) Stat() (fs.FileInfo, error) {
	info, err := f.file.Stat()
	if err != nil {
		return nil, vfs.Errno(err)
	}
	f.info = fileInfo{info}
	return f.info, nil
}

// Readdir 对文件没有意义（http.File 的约定）。
func (f *davFile) Readdir(int) ([]fs.FileInfo, error) { return nil, syscall.ENOTDIR }

// dirFile 是目录的 webdav.File：只支持 Readdir / Stat，
// 读写与 Seek 一律拒绝（与 POSIX 对目录的行为一致）。
type dirFile struct {
	fs   vfs.FileSystem
	name string
	info fileInfo

	entries []fs.FileInfo
	loaded  bool
	offset  int
}

func (d *dirFile) Read([]byte) (int, error)       { return 0, syscall.EISDIR }
func (d *dirFile) Write([]byte) (int, error)      { return 0, syscall.EISDIR }
func (d *dirFile) Seek(int64, int) (int64, error) { return 0, syscall.EISDIR }
func (d *dirFile) Close() error                   { return nil }
func (d *dirFile) Stat() (fs.FileInfo, error)     { return d.info, nil }

// Readdir 按 http.File 的约定分页：
//   - count <= 0：一次性返回全部剩余条目（读到底返回 nil error）；
//   - count > 0：至多 count 条，没有更多时返回 io.EOF。
//
// 这里用 Background context：句柄在请求内被消费，而列目录本身很快，
// 不值得为它把请求 context 的生命周期绑进来（x/net 会在下一次请求重新打开）。
func (d *dirFile) Readdir(count int) ([]fs.FileInfo, error) {
	if !d.loaded {
		entries, err := d.fs.ReadDir(context.Background(), d.name)
		if err != nil {
			return nil, vfs.Errno(err)
		}
		d.entries = make([]fs.FileInfo, 0, len(entries))
		for _, e := range entries {
			d.entries = append(d.entries, fileInfo{e})
		}
		d.loaded = true
	}

	remaining := len(d.entries) - d.offset
	if count <= 0 {
		out := make([]fs.FileInfo, remaining)
		copy(out, d.entries[d.offset:])
		d.offset = len(d.entries)
		return out, nil
	}
	if remaining == 0 {
		return nil, io.EOF
	}
	if count > remaining {
		count = remaining
	}
	out := make([]fs.FileInfo, count)
	copy(out, d.entries[d.offset:d.offset+count])
	d.offset += count
	return out, nil
}
