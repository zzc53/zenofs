// Package vfs 定义 ZenoFS 的虚拟文件系统接口。
//
// 这一层只描述"文件系统应该能做什么"，不涉及任何存储实现，
// 供 SFTP / SMB / WebDAV 等协议服务端复用：
//
//	SFTP   —— stat/lstat/fstat/setstat/opendir/readdir/mkdir/rmdir/remove/rename/
//	          symlink/readlink/open/read/write/close/fsync/statvfs
//	SMB2   —— CREATE/CLOSE/FLUSH/READ/WRITE/QUERY_INFO/SET_INFO/QUERY_DIRECTORY
//	          （外加可选的字节范围锁与目录变更通知）
//	WebDAV —— PROPFIND/PROPPATCH/MKCOL/GET/HEAD/PUT/DELETE/COPY/MOVE/LOCK/UNLOCK
//	          （外加可选的 dead properties）
//
// # 设计约定
//
//   - 路径是 '/' 分隔的绝对路径，'/' 表示挂载根。各协议的分隔符差异
//     （SMB 的 '\'、WebDAV 的 URL 转义）由协议层转换，VFS 一律按 POSIX 语义处理，
//     区分大小写，不做路径规范化之外的任何转换。
//   - 一个 FileSystem 实例绑定"一个已认证用户 + 一个挂载的 Share"，
//     所以方法不接收用户参数。ctx 只用于取消与超时。
//     多挂载点（一个用户能看到多个 Share）由上层聚合，不属于本接口。
//   - 错误是 POSIX 语义的哨兵值，调用方用 errors.Is 判定后映射成各自的错误码
//     （SFTP status code / HTTP 状态码 / SMB NTSTATUS），见文件末尾的错误表。
//   - 只读还是读写由 SharePermission 决定：只读挂载上的写操作返回 ErrPermission。
//   - 属性映射（ZenoFS 的落库约定）：Uid 取文件属主用户、Gid 取所属 Share 的 id、
//     Atime/Ctime 与 Mtime 一样取 inode 的 UpdatedAt；Mode 只区分"是否可执行"，
//     读写权限由 ShareUser.Permission 在挂载层控制，不体现在文件权限位上。
//   - 版本管理、软删除（回收站）、审计都不属于 VFS 契约，由实现层自行处理。
//
// # 可选能力
//
// 有些能力只有个别协议需要，不放进主接口，而是用独立的接口表达、
// 由协议层做类型断言（未实现时该协议降级或返回 ErrNotSupported）。
// 目前定义了 Locker（WebDAV LOCK / SMB 字节范围锁）。
// 若将来需要 SMB 的目录变更通知或 WebDAV 的 dead properties，
// 按同样方式各加一个小接口即可，不必改动主接口。
package vfs

import (
	"context"
	"errors"
	"io"
	"os"
	"time"
)

// ─────────────────────────────────────────────────────────────
// 主接口
// ─────────────────────────────────────────────────────────────

// FileSystem 是一个挂载点上的文件系统操作。
// 所有 path 参数都是该挂载点内的绝对路径。
type FileSystem interface {
	// ── 命名空间 ──

	// Stat 返回路径的元数据，跟随符号链接（等价 stat(2)）。
	Stat(ctx context.Context, path string) (FileInfo, error)
	// Lstat 与 Stat 相同，但不跟随符号链接（等价 lstat(2)）。
	// SFTP 的 LSTAT、WebDAV PROPFIND 的链接处理会用到。
	Lstat(ctx context.Context, path string) (FileInfo, error)

	// ReadDir 列出目录下的条目，不含 "." 与 ".."。
	// 路径不是目录时返回 ErrNotDir。
	ReadDir(ctx context.Context, path string) ([]FileInfo, error)

	// Mkdir 创建一个目录，父目录必须已存在（不做隐式递归创建）。
	// 目标已存在时返回 ErrExist。
	Mkdir(ctx context.Context, path string, mode FileMode) error

	// Remove 删除文件、符号链接或空目录。
	// 目录非空时返回 ErrNotEmpty；目录与文件的区分由实现按 inode 类型决定。
	Remove(ctx context.Context, path string) error

	// Rename 在同一挂载点内改名或移动。
	// 目标已存在时是否覆盖由实现按 POSIX 语义决定（文件覆盖、目录须为空）。
	Rename(ctx context.Context, oldPath, newPath string) error

	// Symlink 创建内容为 target 的符号链接（SFTP SYMLINK）。
	// 不支持链接的实现返回 ErrNotSupported。
	Symlink(ctx context.Context, target, linkPath string) error

	// Readlink 读取符号链接的目标；路径不是链接时返回 ErrInvalid。
	Readlink(ctx context.Context, path string) (string, error)

	// Copy 复制文件或目录树（WebDAV COPY）。
	// recursive 为 true 时递归复制目录，否则目录返回 ErrIsDir。
	Copy(ctx context.Context, srcPath, dstPath string, recursive bool) error

	// ── 属性 ──

	// SetAttr 修改元数据，等价 SFTP SETSTAT / SMB SET_INFO / WebDAV PROPPATCH(live)。
	// Attrs 中为 nil 的字段表示不改动。
	SetAttr(ctx context.Context, path string, attrs Attrs) error

	// ── 文件读写 ──

	// Open 打开文件并返回句柄，语义等价 open(2)。
	// flags 中的 Create 为 true 时按 mode 创建，Exclusive 时目标已存在返回 ErrExist。
	Open(ctx context.Context, path string, flags OpenFlags, mode FileMode) (File, error)

	// Create 是 Open 的便捷形式，等价于"写 + 创建 + 截断"（WebDAV PUT、SMB 建文件）。
	Create(ctx context.Context, path string, mode FileMode) (File, error)

	// ── 容量 ──

	// StatFS 返回挂载点的容量信息（SFTP STATVFS、WebDAV quota-*）。
	// ZenoFS 的容量上限来自所属 Share 的配额，见 FSInfo。
	StatFS(ctx context.Context, path string) (FSInfo, error)
}

// File 是一个已打开的文件句柄。
//
// 嵌入这些 io 接口是为了让协议层能把句柄直接接到自己的读写循环上：
// SFTP 的 READ/WRITE、SMB 的 READ/WRITE 请求都自带 offset，对应 ReaderAt/WriterAt；
// WebDAV 的 Range 读用 Seek + Read 即可。ReaderAt/WriterAt 不影响文件当前偏移，
// 因此并发处理多个请求时不需要额外的句柄级加锁。
type File interface {
	io.Reader   // Read：从当前偏移读取
	io.Writer   // Write：向当前偏移写入
	io.ReaderAt // ReadAt：从指定偏移读取，不改动当前偏移
	io.WriterAt // WriteAt：向指定偏移写入，不改动当前偏移
	io.Seeker   // Seek：移动当前偏移
	io.Closer   // Close：提交并释放句柄

	// Truncate 把文件截断或扩展到指定字节数。
	Truncate(size int64) error

	// Sync 把已写数据刷到稳定存储（SFTP FSYNC、SMB FLUSH）。
	Sync() error

	// Stat 返回该句柄对应文件的最新元数据（SFTP FSTAT、SMB QUERY_INFO on handle）。
	// 句柄打开期间其它会话的修改应能从这里看到。
	Stat() (FileInfo, error)

	// SetAttr 在该句柄上修改属性（SFTP FSETSTAT、SMB SET_INFO on handle）。
	SetAttr(attrs Attrs) error
}

// ─────────────────────────────────────────────────────────────
// 数据类型
// ─────────────────────────────────────────────────────────────

// Kind 描述目录项的类型。
type Kind int8

const (
	KindFile    Kind = iota // 普通文件
	KindDir                 // 目录
	KindSymlink             // 符号链接
)

// FileMode 是 POSIX 权限位与类型位，直接复用 os.FileMode 的语义，
// 这样 SFTP attrs.permissions 与 WebDAV 的权限映射都能直接对应。
type FileMode = os.FileMode

// FileInfo 是一个路径或句柄的元数据。
// 只覆盖三协议共同需要的属性；实现缺少的字段（如 Uid/Gid）可以按约定合成。
type FileInfo struct {
	Id   int64  // inode 编号：SMB 用作 FileId，WebDAV 可参与合成 ETag
	Name string // 路径最后一段；挂载根为 "/"
	Path string // 规范化后的绝对路径
	Kind Kind   // 文件 / 目录 / 符号链接

	Size int64    // 文件字节数；目录通常为 0
	Mode FileMode // 权限与类型位；ZenoFS 只区分"是否可执行"
	Uid  uint32   // 属主用户
	Gid  uint32   // 属组（ZenoFS 用所属 Share 的 id）

	Atime time.Time // 访问时间
	Mtime time.Time // 内容修改时间
	Ctime time.Time // 元数据修改时间

	// Target 仅在 Kind == KindSymlink 时有效，是链接指向的目标（Lstat 场景）。
	// 注意 WebDAV 的 getetag 建议由 Id + Mtime 合成，不必在此单独给出。
	Target string
}

// OpenFlags 描述打开文件的意图，字段语义与 open(2) 的 flags 一致。
// 用结构体而不是位掩码，是为了让协议层拼装与阅读都更直观。
type OpenFlags struct {
	Read      bool // 允许读
	Write     bool // 允许写
	Create    bool // 不存在时创建
	Exclusive bool // 配合 Create：已存在则返回 ErrExist（O_EXCL）
	Truncate  bool // 打开时清空（O_TRUNC）
	Append    bool // 每次写入前移到末尾（O_APPEND）
}

// Attrs 描述一次要修改的元数据。
// 指针为 nil 表示该字段不改动，非 nil 表示置为所指的值。
type Attrs struct {
	Mode  *FileMode  // chmod
	Uid   *uint32    // chown
	Gid   *uint32    // chgrp
	Atime *time.Time // utimes
	Mtime *time.Time // utimes
	Size  *int64     // truncate（也可改用 File.Truncate）
}

// FSInfo 描述挂载点的容量。
//
// ZenoFS 的容量上限来自所属 Share 的配额（shares.quota，单位 MB）：
// TotalBytes = quota，UsedBytes = 该 Share 下所有文件大小之和，FreeBytes 为两者之差。
// quota 为 0 表示不限制，此时容量以底层存储池为准。
type FSInfo struct {
	TotalBytes uint64 // 总容量
	FreeBytes  uint64 // 剩余可用
	UsedBytes  uint64 // 已使用
	TotalFiles uint64 // 总 inode 数（0 表示不限制）
	FreeFiles  uint64 // 可用 inode 数（0 表示不限制）
}

// ─────────────────────────────────────────────────────────────
// 可选能力
// ─────────────────────────────────────────────────────────────

// Locker 是可选能力：路径级的文件锁。
//
// WebDAV 的 LOCK/UNLOCK 用整文件锁（Offset/Length 为 0），
// SMB 用字节范围锁（Offset/Length 指定区间），两者共用同一组方法。
// 协议层应这样使用：
//
//	if lk, ok := fs.(vfs.Locker); ok { ... } else { 返回 ErrNotSupported }
type Locker interface {
	// Lock 加锁并返回令牌（WebDAV 把它作为 lock token 回给客户端）。
	// 锁冲突时返回 ErrBusy。
	Lock(ctx context.Context, path string, l Lock) (LockToken, error)
	// Unlock 释放锁；令牌无效或不属于该持有者时返回 ErrInvalid。
	Unlock(ctx context.Context, path string, token LockToken) error
	// Refresh 续期锁（WebDAV LOCK 的超时刷新）。
	Refresh(ctx context.Context, path string, token LockToken, ttl time.Duration) error
}

// Lock 描述一次加锁请求。
type Lock struct {
	Exclusive bool          // true 为排他锁，false 为共享锁
	Offset    int64         // 字节范围起点；0 配合 Length=0 表示整文件锁
	Length    int64         // 区间长度；0 表示到文件末尾
	Owner     string        // 持有者标识，由协议层传入（会话/客户端 id）
	TTL       time.Duration // 期望的锁超时；0 表示由实现决定默认值
}

// LockToken 是一次成功加锁返回的令牌。
type LockToken string

// ─────────────────────────────────────────────────────────────
// 错误
// ─────────────────────────────────────────────────────────────

// 所有错误都是 POSIX 语义的哨兵值，调用方用 errors.Is 判定后
// 映射成协议自己的错误码，例如：
//
//	ErrNotExist    → SSH_FX_NO_SUCH_FILE / 404 / STATUS_OBJECT_NAME_NOT_FOUND
//	ErrPermission  → SSH_FX_PERMISSION_DENIED / 403 / STATUS_ACCESS_DENIED
//	ErrExist       → SSH_FX_FILE_ALREADY_EXISTS / 409 / STATUS_OBJECT_NAME_COLLISION
//	ErrNotEmpty    → SSH_FX_DIR_NOT_EMPTY / 409 / STATUS_DIRECTORY_NOT_EMPTY
//	ErrIsDir       → SSH_FX_FILE_IS_A_DIRECTORY / 405 / STATUS_FILE_IS_A_DIRECTORY
//	ErrNoSpace     → SSH_FX_FAILURE / 507 / STATUS_DISK_FULL
//	ErrNotSupported → SSH_FX_OP_UNSUPPORTED / 501 / STATUS_NOT_SUPPORTED
var (
	ErrNotExist     = errors.New("vfs: no such file or directory")
	ErrExist        = errors.New("vfs: file exists")
	ErrPermission   = errors.New("vfs: permission denied")
	ErrNotEmpty     = errors.New("vfs: directory not empty")
	ErrIsDir        = errors.New("vfs: is a directory")
	ErrNotDir       = errors.New("vfs: not a directory")
	ErrReadOnly     = errors.New("vfs: read-only filesystem")
	ErrNoSpace      = errors.New("vfs: no space left on device")
	ErrInvalid      = errors.New("vfs: invalid argument")
	ErrNotSupported = errors.New("vfs: operation not supported")
	ErrBusy         = errors.New("vfs: resource busy")
	ErrNameTooLong  = errors.New("vfs: file name too long")
	ErrLoop         = errors.New("vfs: too many levels of symbolic links")
	// ErrCrossDevice 表示操作跨越了挂载点（跨 Share 的 Rename/Copy）。
	// 对应 POSIX 的 EXDEV、SFTP 的 SSH_FX_OP_UNSUPPORTED、SMB 的 STATUS_NOT_SAME_DEVICE。
	ErrCrossDevice = errors.New("vfs: cross-device link")
	// ErrLocked 表示 Share 启用了加密、但当前会话尚未用口令解锁。
	// 对应 SFTP 的 SSH_FX_PERMISSION_DENIED、HTTP 403、SMB 的 STATUS_ACCESS_DENIED。
	ErrLocked = errors.New("vfs: share is locked")
)
