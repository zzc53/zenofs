// Package smb 用 jfjallid/go-smb 的 SMB2/3 服务端，把 zenofs 的 Share 暴露给
// Windows / macOS / Linux 的 SMB 客户端。
//
// # 共享模型
//
// zenofs 的每个 Share 注册成一个同名 SMB 共享（\\host\<share>）。
// 真正决定能读写什么的是 internal/vfs 的 ShareFS（绑定"用户 + Share"，
// 权限取自 share_users），而不是 SMB 侧的共享级权限。
//
// # 认证
//
// 客户端用"zenofs 用户名 + access token"当账号密码登录：token 属于 access_tokens
// 表（见 internal/token），库里只有单向摘要，服务端用 NTLMv2 校验（见 auth.go）。
//
// # 这个文件
//
// 这里只做 VFS 适配：把 go-smb 的 server.VFS 契约（CREATE/READ/WRITE/QUERY_DIRECTORY/
// SET_INFO...）翻译成 internal/vfs 的 FileSystem/File 调用，并把 vfs 的 POSIX 语义
// 错误映射成 NTSTATUS。go-smb 的共享是静态注册的（一个共享一个 VFS 实例），
// 而 ShareFS 绑定用户，所以每个请求都按会话登录名重新解析挂载点。
package smb

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	gsmb "github.com/jfjallid/go-smb/smb"
	"github.com/jfjallid/go-smb/smb/server"
	"github.com/jfjallid/go-smb/smb/unicode"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/pool"
	"github.com/zzc53/zenofs/internal/vfs"
)

var _ server.VFS = (*shareVFS)(nil)

// go-smb 的 smb 包没有导出这几个 NTSTATUS，但 Windows 客户端认它们，
// 所以按 MS-ERREF 的值在本包补齐。
const (
	statusMediaWriteProtected uint32 = 0xc00000a2
	statusSharingViolation    uint32 = 0xc0000043
	statusDiskFull            uint32 = 0xc000007f
	statusNameTooLong         uint32 = 0xc0000106
	statusNotSameDevice       uint32 = 0xc000017d
)

// 新建文件/目录的默认权限。ZenoFS 的文件模式只区分"是否可执行"，
// 读写权限由 share_users 决定，所以这里固定用不可执行的 0644。
const defaultFileMode vfs.FileMode = 0o644

// SMB2 访问掩码里与"写"相关的位：客户端以只读方式打开时这些位都是 0，
// 据此决定要不要向 vfs 申请写权限（只读挂载上的只读打开必须成功）。
const (
	accessWriteData       = 0x00000002
	accessAppendData      = 0x00000004
	accessWriteEA         = 0x00000010
	accessWriteAttributes = 0x00000100
	accessDelete          = 0x00010000
	accessGenericWrite    = 0x40000000
	accessGenericAll      = 0x10000000
	accessMaximumAllowed  = 0x02000000
)

// ─────────────────────────────────────────────────────────────
// 会话 → 用户 / 挂载点
// ─────────────────────────────────────────────────────────────

// userCache 把 SMB 会话上的登录名映射成 zenofs 用户 id，并按用户缓存 RootFS。
//
// 登录名→id 由 NTLM 认证成功时写入；缓存未命中（例如服务重启前建立的会话）
// 再回查 users 表，查不到就视为未授权。
type userCache struct {
	pm    *pool.PoolManager
	mu    sync.Mutex
	ids   map[string]int64
	roots map[int64]*vfs.RootFS
}

func newUserCache(pm *pool.PoolManager) *userCache {
	return &userCache{pm: pm, ids: make(map[string]int64), roots: make(map[int64]*vfs.RootFS)}
}

// remember 记下"登录名 → 用户 id"（大小写不敏感，与 NTLM 的用户名语义一致）。
func (c *userCache) remember(username string, userID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ids[strings.ToLower(strings.TrimSpace(username))] = userID
}

// lookup 返回登录名对应的用户 id。
func (c *userCache) lookup(username string) (int64, bool) {
	key := strings.ToLower(strings.TrimSpace(username))
	if key == "" {
		return 0, false
	}
	c.mu.Lock()
	id, ok := c.ids[key]
	c.mu.Unlock()
	if ok {
		return id, true
	}
	var user db.User
	if err := c.pm.DbManager.DB.Where("lower(username) = ?", key).First(&user).Error; err != nil {
		return 0, false
	}
	c.remember(key, user.Id)
	return user.Id, true
}

// root 返回某个用户的聚合挂载点（RootFS 里缓存了 Share 视图，按用户复用）。
func (c *userCache) root(userID int64) *vfs.RootFS {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.roots[userID]
	if !ok {
		r = vfs.NewRootFS(c.pm, userID)
		c.roots[userID] = r
	}
	return r
}

// ─────────────────────────────────────────────────────────────
// 共享级 VFS
// ─────────────────────────────────────────────────────────────

// shareVFS 是"一个 zenofs Share"在 SMB 侧的挂载点。
type shareVFS struct {
	share db.Share
	pm    *pool.PoolManager
	users *userCache
}

// mount 解析出当前会话对应的 ShareFS。会话未认证、用户不存在、
// 或该用户对 Share 没有授权，都返回 STATUS_ACCESS_DENIED。
func (v *shareVFS) mount(sess *server.Session) (*vfs.ShareFS, uint32) {
	if sess == nil || sess.Username == "" {
		return nil, gsmb.StatusAccessDenied
	}
	userID, ok := v.users.lookup(sess.Username)
	if !ok {
		return nil, gsmb.StatusAccessDenied
	}
	fs, err := v.users.root(userID).Mount(v.share.Name)
	if err != nil {
		return nil, statusOf(err)
	}
	return fs, gsmb.StatusOk
}

// Create 实现 server.VFS：把 CREATE 的 disposition/options 翻译成 vfs.Open / vfs.Mkdir。
func (v *shareVFS) Create(ctx context.Context, sess *server.Session, req server.CreateRequest) (server.CreateResult, uint32, error) {
	fs, status := v.mount(sess)
	if status != gsmb.StatusOk {
		return server.CreateResult{}, status, nil
	}
	p, ok := vfsPath(req.Path)
	if !ok {
		return server.CreateResult{}, gsmb.StatusObjectNameInvalid, nil
	}

	// 打开意图：先看目标当前是否存在（disposition 的合法性依赖它）。
	st, statErr := fs.Stat(ctx, p)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, vfs.ErrNotExist) {
		return server.CreateResult{}, statusOf(statErr), nil
	}

	writable := desiredWrite(req.DesiredAccess)
	create, exclusive, truncate := false, false, false
	var action uint32

	switch req.CreateDisposition {
	case gsmb.FileSupersede:
		// 存在就覆盖、不存在就创建
		create, writable, truncate = true, true, exists
		action = fileAction(exists, gsmb.FileSuperseded)
	case gsmb.FileOpen:
		if !exists {
			return server.CreateResult{}, gsmb.StatusObjectNameNotFound, nil
		}
		action = gsmb.FileOpened
	case gsmb.FileCreate:
		if exists {
			return server.CreateResult{}, gsmb.StatusObjectNameCollision, nil
		}
		create, exclusive, writable = true, true, true
		action = gsmb.FileCreated
	case gsmb.FileOpenIf:
		create = true
		action = fileAction(exists, gsmb.FileOpened)
	case gsmb.FileOverwrite:
		if !exists {
			return server.CreateResult{}, gsmb.StatusObjectNameNotFound, nil
		}
		writable, truncate = true, true
		action = gsmb.FileOverwritten
	case gsmb.FileOverwriteIf:
		create, writable, truncate = true, true, exists
		action = fileAction(exists, gsmb.FileOverwritten)
	default:
		return server.CreateResult{}, gsmb.StatusInvalidParameter, nil
	}

	wantDir := req.CreateOptions&gsmb.FileDirectoryFile != 0
	wantFile := req.CreateOptions&gsmb.FileNonDirectoryFile != 0
	if exists {
		if wantDir && st.Kind != vfs.KindDir {
			return server.CreateResult{}, gsmb.StatusNotADirectory, nil
		}
		if wantFile && st.Kind == vfs.KindDir {
			return server.CreateResult{}, gsmb.StatusFileIsADirectory, nil
		}
	}

	// 目录：只建/只开，不产生文件句柄（SMB 的目录句柄靠 ReadDir 服务）。
	if wantDir || (exists && st.Kind == vfs.KindDir) {
		if !exists {
			if err := fs.Mkdir(ctx, p, vfs.FileMode(0o755)); err != nil {
				return server.CreateResult{}, statusOf(err), nil
			}
		}
		info, err := fs.Stat(ctx, p)
		if err != nil {
			return server.CreateResult{}, statusOf(err), nil
		}
		h := &handle{fs: fs, path: req.Path, isDir: true}
		if req.CreateOptions&gsmb.FileDeleteOnClose != 0 {
			// 目录的 delete-on-close 由 SET_INFO(FileDispositionInformation) 之外的
			// 路径触发；这里先记下，Close 时再尝试删除。
			h.deletePending = true
		}
		return server.CreateResult{Handle: h, CreateAction: action, Info: toServerInfo(baseName(req.Path), info)}, gsmb.StatusOk, nil
	}

	file, err := fs.Open(ctx, p, vfs.OpenFlags{
		Read:      true,
		Write:     writable,
		Create:    create,
		Exclusive: exclusive,
		Truncate:  truncate,
	}, defaultFileMode)
	if err != nil {
		return server.CreateResult{}, statusOf(err), nil
	}

	h := &handle{fs: fs, file: file, path: req.Path}
	if req.CreateOptions&gsmb.FileDeleteOnClose != 0 {
		h.deletePending = true
	}
	info, err := file.Stat()
	if err != nil {
		h.close(ctx)
		return server.CreateResult{}, statusOf(err), nil
	}
	return server.CreateResult{Handle: h, CreateAction: action, Info: toServerInfo(baseName(req.Path), info)}, gsmb.StatusOk, nil
}

// Close 关闭句柄：先提交文件（vfs 在 Close 里落版本），再处理 delete-on-close。
func (v *shareVFS) Close(ctx context.Context, h server.Handle) error {
	hd, ok := h.(*handle)
	if !ok {
		return nil
	}
	return hd.close(ctx)
}

// Read 实现 server.VFS。
func (v *shareVFS) Read(ctx context.Context, h server.Handle, offset int64, buf []byte) (int, uint32, error) {
	hd, ok := h.(*handle)
	if !ok {
		return 0, gsmb.StatusInvalidParameter, nil
	}
	if hd.isDir {
		return 0, gsmb.StatusFileIsADirectory, nil
	}
	if hd.file == nil {
		return 0, gsmb.StatusFileClosed, nil
	}
	if offset < 0 {
		return 0, gsmb.StatusInvalidParameter, nil
	}
	n, err := hd.file.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, statusOf(err), nil
	}
	return n, gsmb.StatusOk, nil
}

// Write 实现 server.VFS。
func (v *shareVFS) Write(ctx context.Context, h server.Handle, offset int64, data []byte) (int, uint32, error) {
	hd, ok := h.(*handle)
	if !ok {
		return 0, gsmb.StatusInvalidParameter, nil
	}
	if hd.isDir {
		return 0, gsmb.StatusFileIsADirectory, nil
	}
	if hd.file == nil {
		return 0, gsmb.StatusFileClosed, nil
	}
	if offset < 0 {
		return 0, gsmb.StatusInvalidParameter, nil
	}
	n, err := hd.file.WriteAt(data, offset)
	if err != nil {
		return n, statusOf(err), nil
	}
	return n, gsmb.StatusOk, nil
}

// Flush 实现 server.VFS（对应 vfs 的 Sync：把写入刷到稳定存储）。
func (v *shareVFS) Flush(ctx context.Context, h server.Handle) (uint32, error) {
	hd, ok := h.(*handle)
	if !ok {
		return gsmb.StatusInvalidParameter, nil
	}
	if hd.file == nil {
		return gsmb.StatusOk, nil
	}
	if err := hd.file.Sync(); err != nil {
		return statusOf(err), nil
	}
	return gsmb.StatusOk, nil
}

// QueryFileInfo 交给服务端的默认实现：它用 Handle.Stat() 组装
// FILE_BASIC/STANDARD/INTERNAL/ALL_INFORMATION，我们只需保证 Stat 是对的。
func (v *shareVFS) QueryFileInfo(ctx context.Context, h server.Handle, infoClass byte) (any, uint32, error) {
	return nil, gsmb.StatusNotSupported, nil
}

// SetFileInfo 处理截断/删除标记/时间戳/改名。
func (v *shareVFS) SetFileInfo(ctx context.Context, h server.Handle, infoClass byte, raw []byte) (uint32, error) {
	hd, ok := h.(*handle)
	if !ok {
		return gsmb.StatusInvalidParameter, nil
	}
	p, ok := vfsPath(hd.path)
	if !ok {
		return gsmb.StatusObjectNameInvalid, nil
	}

	switch infoClass {
	case gsmb.FileEndOfFileInformation: // 0x14：截断/扩展
		if len(raw) < 8 {
			return gsmb.StatusInfoLengthMismatch, nil
		}
		size := int64(binary.LittleEndian.Uint64(raw[:8]))
		if size < 0 {
			return gsmb.StatusInvalidParameter, nil
		}
		if hd.file == nil {
			return gsmb.StatusInvalidParameter, nil
		}
		if err := hd.file.Truncate(size); err != nil {
			return statusOf(err), nil
		}
		return gsmb.StatusOk, nil

	case gsmb.FileAllocationInformation: // 0x13：预分配，ZenoFS 不做预留，直接接受
		return gsmb.StatusOk, nil

	case gsmb.FileDispositionInformation: // 0x0d：标记删除
		if len(raw) < 1 {
			return gsmb.StatusInfoLengthMismatch, nil
		}
		want := raw[0] != 0
		if want {
			st, err := hd.statInfo(ctx)
			if err != nil {
				return statusOf(err), nil
			}
			if st.Kind == vfs.KindDir {
				entries, err := hd.fs.ReadDir(ctx, p)
				if err != nil {
					return statusOf(err), nil
				}
				if len(entries) > 0 {
					return gsmb.StatusDirectoryNotEmpty, nil
				}
			}
		}
		hd.deletePending = want
		return gsmb.StatusOk, nil

	case gsmb.FileBasicInformation: // 0x04：时间戳
		if len(raw) < 36 {
			return gsmb.StatusInfoLengthMismatch, nil
		}
		var attrs vfs.Attrs
		applyTime(raw, 8, &attrs.Atime)
		applyTime(raw, 16, &attrs.Mtime)
		if err := hd.fs.SetAttr(ctx, p, attrs); err != nil {
			return statusOf(err), nil
		}
		return gsmb.StatusOk, nil

	case gsmb.FileRenameInformation: // 0x0a：改名/移动（同一 Share 内）
		// 布局：ReplaceIfExists(1) + Reserved(7) + RootDirectory(8) + FileNameLength(4) + FileName
		if len(raw) < 20 {
			return gsmb.StatusInfoLengthMismatch, nil
		}
		replace := raw[0] != 0
		nameLen := int(binary.LittleEndian.Uint32(raw[16:20]))
		if nameLen <= 0 || 20+nameLen > len(raw) {
			return gsmb.StatusInfoLengthMismatch, nil
		}
		target, err := unicode.FromUnicodeString(raw[20 : 20+nameLen])
		if err != nil {
			return gsmb.StatusInvalidParameter, nil
		}
		newPath, ok := vfsPath(target)
		if !ok {
			return gsmb.StatusObjectNameInvalid, nil
		}
		if !replace {
			if _, err := hd.fs.Stat(ctx, newPath); err == nil {
				return gsmb.StatusObjectNameCollision, nil
			} else if !errors.Is(err, vfs.ErrNotExist) {
				return statusOf(err), nil
			}
		}
		if err := hd.fs.Rename(ctx, p, newPath); err != nil {
			return statusOf(err), nil
		}
		hd.path = target
		return gsmb.StatusOk, nil

	case gsmb.FilePositionInformation, gsmb.FileModeInformation:
		// 句柄是偏移无关的（READ/WRITE 都带 offset），这两个类接受即可。
		return gsmb.StatusOk, nil
	}
	return gsmb.StatusNotSupported, nil
}

// QueryDirectory 列出目录的直接子项。
//
// go-smb 在服务端自己维护分页游标：VFS 一次返回全部条目，之后同一句柄的重复调用
// 返回空（restart 置位时才重新列）。
func (v *shareVFS) QueryDirectory(ctx context.Context, h server.Handle, pattern string, restart bool) ([]server.DirEntry, uint32, error) {
	hd, ok := h.(*handle)
	if !ok {
		return nil, gsmb.StatusInvalidParameter, nil
	}
	if !hd.isDir {
		return nil, gsmb.StatusNotADirectory, nil
	}
	if restart {
		hd.dirScanned = false
	}
	if hd.dirScanned {
		return nil, gsmb.StatusOk, nil
	}
	hd.dirScanned = true

	p, ok := vfsPath(hd.path)
	if !ok {
		return nil, gsmb.StatusObjectNameInvalid, nil
	}
	self, err := hd.fs.Stat(ctx, p)
	if err != nil {
		return nil, statusOf(err), nil
	}
	entries, err := hd.fs.ReadDir(ctx, p)
	if err != nil {
		return nil, statusOf(err), nil
	}

	out := make([]server.DirEntry, 0, len(entries)+2)
	// Windows 客户端依赖 "." 与 ".."，二者都指向本目录。
	if matchPattern(".", pattern) {
		out = append(out, server.DirEntry{FileInfo: toServerInfo(".", self)})
	}
	if matchPattern("..", pattern) {
		out = append(out, server.DirEntry{FileInfo: toServerInfo("..", self)})
	}
	for _, e := range entries {
		if !matchPattern(e.Name, pattern) {
			continue
		}
		out = append(out, server.DirEntry{FileInfo: toServerInfo(e.Name, e)})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, gsmb.StatusOk, nil
}

// QueryFSInfo 交给服务端的默认实现（按共享名报通用的容量信息）。
func (v *shareVFS) QueryFSInfo(ctx context.Context, infoClass byte) (any, uint32, error) {
	return nil, gsmb.StatusNotSupported, nil
}

// QuerySecurity 交给服务端的默认实现（Everyone 可读的安全描述符）。
func (v *shareVFS) QuerySecurity(ctx context.Context, h server.Handle, addInfo uint32) ([]byte, uint32, error) {
	return nil, gsmb.StatusNotSupported, nil
}

// Ioctl 未实现（FSCTL_* 一律不支持）。
func (v *shareVFS) Ioctl(ctx context.Context, h server.Handle, code uint32, in []byte, maxOut uint32) ([]byte, uint32, error) {
	return nil, gsmb.StatusNotSupported, nil
}

// ─────────────────────────────────────────────────────────────
// 句柄
// ─────────────────────────────────────────────────────────────

// handle 是一个已打开的 SMB 对象（文件或目录）。
type handle struct {
	fs    *vfs.ShareFS
	file  vfs.File // 目录为 nil
	path  string   // SMB 风格路径（"\" 分隔，share 相对）
	isDir bool

	deletePending bool
	dirScanned    bool
	closed        bool
}

func (h *handle) Path() server.Path { return h.path }
func (h *handle) IsDir() bool       { return h.isDir }

// Stat 返回最新元数据：文件走句柄（能看到自己未提交的写入大小），目录走路径。
func (h *handle) Stat() (server.FileInfo, error) {
	info, err := h.statInfo(context.Background())
	if err != nil {
		return server.FileInfo{}, err
	}
	return toServerInfo(baseName(h.path), info), nil
}

// statInfo 是 Stat 的 vfs 版本。
func (h *handle) statInfo(ctx context.Context) (vfs.FileInfo, error) {
	if h.file != nil {
		if info, err := h.file.Stat(); err == nil {
			return info, nil
		}
	}
	p, ok := vfsPath(h.path)
	if !ok {
		return vfs.FileInfo{}, vfs.ErrInvalid
	}
	return h.fs.Stat(ctx, p)
}

// close 关闭句柄：先关文件（vfs 在这里提交新版本），再执行 delete-on-close。
func (h *handle) close(ctx context.Context) error {
	if h.closed {
		return nil
	}
	h.closed = true
	var firstErr error
	if h.file != nil {
		if err := h.file.Close(); err != nil {
			firstErr = err
		}
		h.file = nil
	}
	if h.deletePending {
		if p, ok := vfsPath(h.path); ok {
			if err := h.fs.Remove(ctx, p); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// ─────────────────────────────────────────────────────────────
// 转换与映射
// ─────────────────────────────────────────────────────────────

// vfsPath 把 SMB 的 "\dir\name" 转成 vfs 的 "/dir/name"。
// 含 ".."、空段、内嵌分隔符或 NUL 的路径一律拒绝（客户端不该发，发了就是越权尝试）。
func vfsPath(p server.Path) (string, bool) {
	clean := strings.Trim(p, "\\")
	if clean == "" {
		return "/", true
	}
	segs := make([]string, 0, 8)
	for _, s := range strings.Split(clean, "\\") {
		switch {
		case s == ".":
			continue
		case s == "" || s == ".." || strings.ContainsAny(s, "/\\\x00"):
			return "", false
		}
		segs = append(segs, s)
	}
	if len(segs) == 0 {
		return "/", true
	}
	return "/" + strings.Join(segs, "/"), true
}

// baseName 取 SMB 路径的最后一段（挂载根为 "\"）。
func baseName(p server.Path) string {
	clean := strings.Trim(p, "\\")
	if clean == "" {
		return "/"
	}
	if i := strings.LastIndex(clean, "\\"); i >= 0 {
		return clean[i+1:]
	}
	return clean
}

// desiredWrite 判断客户端是否想要写。
func desiredWrite(access uint32) bool {
	const writeBits = accessWriteData | accessAppendData | accessWriteEA |
		accessWriteAttributes | accessDelete | accessGenericWrite |
		accessGenericAll | accessMaximumAllowed
	return access&writeBits != 0
}

// fileAction 按"目标是否已存在"选择 CREATE 动作码。
func fileAction(exists bool, ifExists uint32) uint32 {
	if exists {
		return ifExists
	}
	return gsmb.FileCreated
}

// matchPattern 做 SMB 的通配匹配（"*" 任意、"?" 单字符），大小写不敏感。
func matchPattern(name, pattern string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	ok, err := path.Match(strings.ToLower(pattern), strings.ToLower(name))
	if err != nil {
		return strings.EqualFold(name, pattern)
	}
	return ok
}

// applyTime 从 FILETIME 槽位解析时间写入 attrs；槽位为 0 或全 1 表示"不改"。
func applyTime(raw []byte, off int, dst **time.Time) {
	if off+8 > len(raw) {
		return
	}
	ft := binary.LittleEndian.Uint64(raw[off:])
	if ft == 0 || ft == 0xFFFFFFFFFFFFFFFF {
		return
	}
	t := fileTimeToTime(ft)
	*dst = &t
}

// fileTimeToTime 把 Windows FILETIME（1601 年起的 100ns 计数）转成 time.Time。
func fileTimeToTime(ft uint64) time.Time {
	const epochDelta = 116444736000000000 // Unix 纪元对应的 FILETIME 计数
	if ft < epochDelta {
		return time.Time{}
	}
	nano := int64(ft-epochDelta) * 100
	return time.Unix(nano/1e9, nano%1e9).UTC()
}

// toServerInfo 把 vfs 的元数据转成 go-smb 的 FileInfo。
func toServerInfo(name string, info vfs.FileInfo) server.FileInfo {
	attrs := uint32(0x00000080) // FILE_ATTRIBUTE_NORMAL
	switch info.Kind {
	case vfs.KindDir:
		attrs = 0x00000010 // FILE_ATTRIBUTE_DIRECTORY
	case vfs.KindSymlink:
		attrs = 0x00000400 // FILE_ATTRIBUTE_REPARSE_POINT
	}
	if name == "" {
		name = info.Name
	}
	return server.FileInfo{
		Name:           name,
		Size:           info.Size,
		AllocationSize: info.Size,
		Attributes:     attrs,
		CreationTime:   info.Ctime,
		LastAccessTime: info.Atime,
		LastWriteTime:  info.Mtime,
		ChangeTime:     info.Ctime,
		FileID:         uint64(info.Id),
	}
}

// statusOf 把 vfs 的 POSIX 语义错误映射成 NTSTATUS。
func statusOf(err error) uint32 {
	switch {
	case err == nil:
		return gsmb.StatusOk
	case errors.Is(err, vfs.ErrNotExist):
		return gsmb.StatusObjectNameNotFound
	case errors.Is(err, vfs.ErrExist):
		return gsmb.StatusObjectNameCollision
	case errors.Is(err, vfs.ErrNotEmpty):
		return gsmb.StatusDirectoryNotEmpty
	case errors.Is(err, vfs.ErrIsDir):
		return gsmb.StatusFileIsADirectory
	case errors.Is(err, vfs.ErrNotDir):
		return gsmb.StatusNotADirectory
	case errors.Is(err, vfs.ErrReadOnly):
		return statusMediaWriteProtected
	case errors.Is(err, vfs.ErrNoSpace):
		return statusDiskFull
	case errors.Is(err, vfs.ErrNotSupported):
		return gsmb.StatusNotSupported
	case errors.Is(err, vfs.ErrBusy):
		return statusSharingViolation
	case errors.Is(err, vfs.ErrNameTooLong):
		return statusNameTooLong
	case errors.Is(err, vfs.ErrCrossDevice):
		return statusNotSameDevice
	case errors.Is(err, vfs.ErrPermission), errors.Is(err, vfs.ErrEncrypted):
		// 加密的 Share 在 SMB 侧没有提供口令的通道，等同无权访问。
		return gsmb.StatusAccessDenied
	default:
		return gsmb.StatusInvalidParameter
	}
}
