package vfs

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
	"github.com/zzc53/zenofs/internal/pool"
	"gorm.io/gorm"
)

const (
	// defaultSliceSize 是 Share.SliceSize 缺省时的切片大小（4MB）。
	defaultSliceSize = 4 * 1024 * 1024
	// maxSymlinkDepth 是解析符号链接的最大跳数。
	maxSymlinkDepth = 16
	// maxPathDepth 是反推路径时向上遍历的最大层数，用于兜住异常数据形成的环。
	maxPathDepth = 256
	// maxNameLen 是单个文件名的最大字节数。
	maxNameLen = 255
)

// ShareFS 把一个 Share 实现成 FileSystem。
//
// 它绑定"一个用户 + 一个 Share"：
//   - 所有 inode 查询都限定在这个 Share 内，路径不会跨出挂载点；
//   - 读写权限取自该用户在 ShareUser 里的 Permission，所以方法不接收用户参数；
//   - 解密所需的密钥只在会话内存中（key），不落库。
type ShareFS struct {
	pm     *pool.PoolManager
	share  db.Share
	userID int64
	perm   db.SharePermission

	// key 是解密该 Share 的密钥，仅在启用加密且用户已解锁时非空。
	key []byte
}

var _ FileSystem = (*ShareFS)(nil)

// NewShareFS 创建 Share 文件系统。加密的 Share 需要再调 Unlock 才能读写文件内容。
func NewShareFS(pm *pool.PoolManager, share db.Share, userID int64, perm db.SharePermission) *ShareFS {
	return &ShareFS{pm: pm, share: share, userID: userID, perm: perm}
}

// Share 返回当前挂载的 Share 元数据。
func (fs *ShareFS) Share() db.Share { return fs.share }

// sliceSize 返回该 Share 的切片大小（字节），即 version chunk 的定长边界。
func (fs *ShareFS) sliceSize() int64 {
	if fs.share.SliceSize <= 0 {
		return defaultSliceSize
	}
	return fs.share.SliceSize * 1024
}

// up 判断当前挂载是否具备写权限。
func (fs *ShareFS) up() bool { return fs.perm >= db.ShareWrite }

// requireWrite 在只读挂载上拒绝写操作。
func (fs *ShareFS) requireWrite() error {
	if !fs.up() {
		return ErrPermission
	}
	return nil
}

// ---------------------------------------------------------------
// 内核查找
// ---------------------------------------------------------------

// rootInode 合成 Share 根目录的 inode。
// 模型里没有单独的根 inode 记录：ParentId 为 NULL 的条目就是根下的一级条目，
// 因此根的 Id 为 0，不会与真实 inode 冲突。
func (fs *ShareFS) rootInode() db.Inode {
	return db.Inode{Kind: db.InodeDir, ShareId: fs.share.Id, Name: "/"}
}

// lookup 把 Share 内的绝对路径解析成 inode；follow 为 true 时跟随末端的符号链接。
//
// 中间层级的符号链接不做展开（会被当成 ErrNotDir），这是刻意的简化。
func (fs *ShareFS) lookup(p string, follow bool) (db.Inode, error) {
	segs := pathSegments(p)
	if len(segs) == 0 {
		return fs.rootInode(), nil
	}

	parent := sql.NullInt64{} // 无效表示"根下"
	var cur db.Inode
	for i, seg := range segs {
		var in db.Inode
		q := fs.pm.DbManager.DB.
			Where("share_id = ? AND name = ? AND deleted = 0", fs.share.Id, seg)
		if parent.Valid {
			q = q.Where("parent_id = ?", parent.Int64)
		} else {
			q = q.Where("parent_id IS NULL")
		}
		if err := q.First(&in).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return db.Inode{}, ErrNotExist
			}
			return db.Inode{}, errs.DBQuery(err)
		}
		if i < len(segs)-1 && in.Kind != db.InodeDir {
			return db.Inode{}, ErrNotDir
		}
		cur = in
		parent = sql.NullInt64{Int64: in.Id, Valid: true}
	}

	// 末端是符号链接且需要跟随：LinkId 直接指向目标 inode，逐跳解析并限制深度
	if follow {
		for depth := 0; cur.Kind == db.InodeLink && cur.LinkId.Valid; depth++ {
			if depth >= maxSymlinkDepth {
				return db.Inode{}, ErrLoop
			}
			// 必须用新变量接收：若复用 cur，GORM 会把 cur 已有的主键也拼进 WHERE
			var target db.Inode
			if err := fs.pm.DbManager.DB.First(&target, cur.LinkId.Int64).Error; err != nil {
				return db.Inode{}, ErrNotExist
			}
			cur = target
		}
	}
	return cur, nil
}

// pathOf 由 inode 反推出它在 Share 内的绝对路径（符号链接的 Readlink 需要）。
func (fs *ShareFS) pathOf(inodeID int64) (string, error) {
	names := make([]string, 0, 8)
	id := inodeID
	for depth := 0; depth < maxPathDepth; depth++ {
		var in db.Inode
		if err := fs.pm.DbManager.DB.First(&in, id).Error; err != nil {
			return "", ErrNotExist
		}
		names = append(names, in.Name)
		if !in.ParentId.Valid {
			// 找到根下的一级条目，停止向上
			for i, j := 0, len(names)-1; i < j; i, j = i+1, j-1 {
				names[i], names[j] = names[j], names[i]
			}
			return "/" + strings.Join(names, "/"), nil
		}
		id = in.ParentId.Int64
	}
	return "", ErrLoop
}

// ---------------------------------------------------------------
// 元数据合成
// ---------------------------------------------------------------

// kindOf 把模型的 inode 类型映射成 VFS 类型。
func kindOf(k db.InodeKind) Kind {
	switch k {
	case db.InodeDir:
		return KindDir
	case db.InodeLink:
		return KindSymlink
	default:
		return KindFile
	}
}

// modeOf 合成权限位：读写位来自挂载权限，执行位来自 inode 的 Executable。
// 目录固定带 x 位，否则客户端无法进入。
func (fs *ShareFS) modeOf(in db.Inode) FileMode {
	perm := FileMode(0o444) // 能看到这个 Share 就至少有读权限
	if fs.up() {
		perm |= 0o222
	}
	switch in.Kind {
	case db.InodeDir:
		return os.ModeDir | perm | 0o111
	case db.InodeLink:
		return os.ModeSymlink | perm | 0o111
	default:
		if in.Executable != 0 {
			perm |= 0o111
		}
		return perm
	}
}

// buildInfo 用给定的文件大小组装 FileInfo。
func (fs *ShareFS) buildInfo(in db.Inode, p string, size int64) FileInfo {
	ts := time.Unix(in.UpdatedAt, 0)
	return FileInfo{
		Id:    in.Id,
		Name:  baseName(p),
		Path:  p,
		Kind:  kindOf(in.Kind),
		Size:  size,
		Mode:  fs.modeOf(in),
		Uid:   uint32(in.CreatedBy),
		Gid:   uint32(in.ShareId),
		Atime: ts,
		Mtime: ts,
		Ctime: ts, // 三者同源：都取 inode 的 UpdatedAt
	}
}

// info 组装单个 inode 的 FileInfo（会查一次当前版本的大小）。
func (fs *ShareFS) info(in db.Inode, p string) (FileInfo, error) {
	fi := fs.buildInfo(in, p, fs.versionSize(in))
	if in.Kind == db.InodeLink && in.LinkId.Valid {
		if target, err := fs.pathOf(in.LinkId.Int64); err == nil {
			fi.Target = target
		}
	}
	return fi, nil
}

// versionSize 返回 inode 当前版本的文件大小；目录与链接为 0。
func (fs *ShareFS) versionSize(in db.Inode) int64 {
	if in.Kind != db.InodeFile || !in.VersionId.Valid {
		return 0
	}
	var v db.Version
	if err := fs.pm.DbManager.DB.First(&v, in.VersionId.Int64).Error; err != nil {
		return 0
	}
	return v.Size
}

// ---------------------------------------------------------------
// 只读操作
// ---------------------------------------------------------------

// Stat 返回路径的元数据，跟随末端符号链接。
func (fs *ShareFS) Stat(_ context.Context, p string) (FileInfo, error) {
	return fs.stat(p, true)
}

// Lstat 与 Stat 相同，但不跟随末端符号链接。
func (fs *ShareFS) Lstat(_ context.Context, p string) (FileInfo, error) {
	return fs.stat(p, false)
}

func (fs *ShareFS) stat(p string, follow bool) (FileInfo, error) {
	clean, err := cleanPath(p)
	if err != nil {
		return FileInfo{}, err
	}
	in, err := fs.lookup(clean, follow)
	if err != nil {
		return FileInfo{}, err
	}
	return fs.info(in, clean)
}

// ReadDir 列出目录下的条目。
func (fs *ShareFS) ReadDir(_ context.Context, p string) ([]FileInfo, error) {
	clean, err := cleanPath(p)
	if err != nil {
		return nil, err
	}
	dir, err := fs.lookup(clean, true)
	if err != nil {
		return nil, err
	}
	if dir.Kind != db.InodeDir {
		return nil, ErrNotDir
	}

	q := fs.pm.DbManager.DB.Where("share_id = ? AND deleted = 0", fs.share.Id)
	if dir.Id == 0 {
		q = q.Where("parent_id IS NULL")
	} else {
		q = q.Where("parent_id = ?", dir.Id)
	}
	var children []db.Inode
	if err := q.Find(&children).Error; err != nil {
		return nil, errs.DBQuery(err)
	}

	// 批量取各文件当前版本的大小，避免逐条查询
	sizeOf := fs.sizesOf(children)

	out := make([]FileInfo, 0, len(children))
	for _, c := range children {
		fi := fs.buildInfo(c, joinPath(clean, c.Name), sizeOf[c.Id])
		if c.Kind == db.InodeLink && c.LinkId.Valid {
			if target, err := fs.pathOf(c.LinkId.Int64); err == nil {
				fi.Target = target
			}
		}
		out = append(out, fi)
	}
	return out, nil
}

// sizesOf 一次查出这批 inode 当前版本的文件大小，按 inode id 建索引。
func (fs *ShareFS) sizesOf(children []db.Inode) map[int64]int64 {
	sizes := make(map[int64]int64, len(children))
	verIDs := make([]int64, 0, len(children))
	for _, c := range children {
		if c.Kind == db.InodeFile && c.VersionId.Valid {
			verIDs = append(verIDs, c.VersionId.Int64)
		}
	}
	if len(verIDs) == 0 {
		return sizes
	}
	var vs []db.Version
	if err := fs.pm.DbManager.DB.Where("id IN ?", verIDs).Find(&vs).Error; err != nil {
		return sizes
	}
	sizeByVer := make(map[int64]int64, len(vs))
	for _, v := range vs {
		sizeByVer[v.Id] = v.Size
	}
	for _, c := range children {
		if c.Kind == db.InodeFile && c.VersionId.Valid {
			sizes[c.Id] = sizeByVer[c.VersionId.Int64]
		}
	}
	return sizes
}

// Readlink 读取符号链接的目标路径。
func (fs *ShareFS) Readlink(_ context.Context, p string) (string, error) {
	clean, err := cleanPath(p)
	if err != nil {
		return "", err
	}
	in, err := fs.lookup(clean, false)
	if err != nil {
		return "", err
	}
	if in.Kind != db.InodeLink || !in.LinkId.Valid {
		return "", ErrInvalid
	}
	target, err := fs.pathOf(in.LinkId.Int64)
	if err != nil {
		return "", ErrNotExist
	}
	return target, nil
}

// StatFS 返回该 Share 的容量信息（上限来自 Share.Quota）。
func (fs *ShareFS) StatFS(_ context.Context, _ string) (FSInfo, error) {
	used, err := fs.usedBytes()
	if err != nil {
		return FSInfo{}, err
	}
	if fs.share.Quota <= 0 {
		// 未设配额：只报已用量，总量交给底层存储池
		return FSInfo{UsedBytes: uint64(used)}, nil
	}
	total := uint64(fs.share.Quota) << 20 // MB → 字节
	var free uint64
	if uint64(used) < total {
		free = total - uint64(used)
	}
	return FSInfo{TotalBytes: total, UsedBytes: uint64(used), FreeBytes: free}, nil
}

// usedBytes 统计该 Share 已占用的字节数——所有 inode 当前版本大小之和。
// 软删除的文件仍然计入（数据还在，后续要支持恢复与清空）。
func (fs *ShareFS) usedBytes() (int64, error) {
	var total int64
	err := fs.pm.DbManager.DB.Table("inodes").
		Joins("JOIN versions ON versions.id = inodes.version_id").
		Where("inodes.share_id = ?", fs.share.Id).
		Select("COALESCE(SUM(versions.size), 0)").
		Scan(&total).Error
	if err != nil {
		return 0, errs.DBQuery(err)
	}
	return total, nil
}
