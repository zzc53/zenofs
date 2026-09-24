package vfs

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
	"github.com/zzc53/zenofs/internal/pool"
	"gorm.io/gorm"
)

// ---------------------------------------------------------------
// 根聚合
// ---------------------------------------------------------------
//
// ShareFS 一个实例只挂载"一个用户 + 一个 Share"。SFTP / WebDAV 这类协议
// 有统一的根目录：客户端连上来先看到"这个用户有哪些 Share"，
// 再把某个 Share 当一级目录进去。RootFS 就是这一层：
//
//	/            → 合成的根，只列出 Share 目录
//	/<share>     → 某个 Share 的根目录
//	/<share>/a/b → 该 Share 内的 /a/b
//
// 根与挂载点根都是**合成的只读视图**：Share 的增删改属于授权管理
// （shares / share_users 表），不通过文件系统接口做，因此这两层上的写操作
// 一律返回 ErrNotSupported；跨 Share 的 Rename/Copy 返回 ErrCrossDevice。
// 挂载点内部的读写与 ShareFS 完全一致，权限取自 share_users.permission。
//
// 解析路径时会确认授权仍然存在，所以会话期间回收授权立刻生效；
// 权限或影响编解码/配额的字段变了会重建挂载实例，而口令派生出的会话密钥
// 存在 RootFS 里，重建不会丢密钥。

// RootFS 把一个用户能看到的多个 Share 聚合成一个挂载点。
//
// 并发说明：文件/目录方法可以并发调用；UsePassword / ClearKey 会写会话密钥，
// 应当与会话的建立、结束同步（不要与同一 Share 的读写并发调用）。
type RootFS struct {
	pm     *pool.PoolManager
	userID int64

	mu     sync.Mutex
	mounts map[int64]*ShareFS // share id → 挂载实例（缓存以保留密钥）
	keys   map[int64][]byte   // share id → 已校验的会话密钥（重建实例时沿用）
}

var _ FileSystem = (*RootFS)(nil)

// NewRootFS 创建某个用户的聚合挂载点；userID 决定根目录下能看到哪些 Share。
func NewRootFS(pm *pool.PoolManager, userID int64) *RootFS {
	return &RootFS{
		pm:     pm,
		userID: userID,
		mounts: make(map[int64]*ShareFS),
		keys:   make(map[int64][]byte),
	}
}

// ---------------------------------------------------------------
// 挂载表与密钥
// ---------------------------------------------------------------

// shareMount 是一条"用户可见的 Share"：Share 记录 + 该用户的权限。
type shareMount struct {
	Share      db.Share
	Permission db.SharePermission
}

// Share 返回根下某个 Share 的记录与该用户的权限，供协议层取配额、
// 判断是否需要口令；名字不存在或该用户没有授权都返回 ErrNotExist。
func (r *RootFS) Share(name string) (db.Share, db.SharePermission, error) {
	return r.shareByName(name)
}

// Mount 返回根下某个 Share 的挂载点，便于协议层绕开路径解析直接操作单个 Share
// （返回的 *ShareFS 也满足 FileSystem 接口）。实例由 RootFS 缓存，状态一直有效；
// 要打开加密的 Share 请用 RootFS.UsePassword，密钥才会记在 RootFS 上、重建时不丢。
func (r *RootFS) Mount(name string) (*ShareFS, error) {
	return r.mount(name)
}

// UsePassword 用口令打开启用了加密的 Share，密钥留在 RootFS 会话内存里
// （挂载实例重建时沿用）；未启用加密的 Share 无需提供口令，直接返回 nil。
func (r *RootFS) UsePassword(name, password string) error {
	share, perm, err := r.shareByName(name)
	if err != nil {
		return err
	}
	fs := r.mountFor(share, perm)
	if err := fs.UsePassword(password); err != nil {
		return err
	}
	if len(fs.key) == 0 {
		return nil
	}
	r.mu.Lock()
	r.keys[share.Id] = append([]byte(nil), fs.key...)
	r.mu.Unlock()
	return nil
}

// ClearKey 清除某个 Share 的会话密钥，其它 Share 不受影响。
func (r *RootFS) ClearKey(name string) error {
	share, _, err := r.shareByName(name)
	if err != nil {
		return err
	}
	r.mu.Lock()
	delete(r.keys, share.Id)
	cur := r.mounts[share.Id]
	r.mu.Unlock()
	if cur != nil {
		cur.ClearKey()
	}
	return nil
}

// shareByName 解析根下的一级目录名：返回 Share 记录与该用户的权限。
// Share 不存在、名字不能作为目录名、或该用户没有 share_users 记录时统一返回
// ErrNotExist，不把"存在但无权访问"的 Share 名字暴露给调用方。
func (r *RootFS) shareByName(name string) (db.Share, db.SharePermission, error) {
	if checkName(name) != nil {
		return db.Share{}, 0, ErrNotExist
	}
	var share db.Share
	err := r.pm.DbManager.DB.Where("name = ?", name).First(&share).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return db.Share{}, 0, ErrNotExist
		}
		return db.Share{}, 0, errs.DBQuery(err)
	}
	var grant db.ShareUser
	err = r.pm.DbManager.DB.
		Where("share_id = ? AND user_id = ?", share.Id, r.userID).First(&grant).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return db.Share{}, 0, ErrNotExist
		}
		return db.Share{}, 0, errs.DBQuery(err)
	}
	return share, grant.Permission, nil
}

// visibleShares 列出该用户可见的 Share（share_users 里有记录的），按名字排序。
//
// 名字不能作为目录名的 Share（含 '/'、NUL 或过长）会被隐藏并记一条日志：
// 这种 Share 在根目录里没有可寻址的路径。
func (r *RootFS) visibleShares() ([]shareMount, error) {
	var grants []db.ShareUser
	if err := r.pm.DbManager.DB.Where("user_id = ?", r.userID).Find(&grants).Error; err != nil {
		return nil, errs.DBQuery(err)
	}
	if len(grants) == 0 {
		return nil, nil
	}
	permOf := make(map[int64]db.SharePermission, len(grants))
	ids := make([]int64, 0, len(grants))
	for _, g := range grants {
		permOf[g.ShareId] = g.Permission
		ids = append(ids, g.ShareId)
	}

	var shares []db.Share
	if err := r.pm.DbManager.DB.Where("id IN ?", ids).Order("name").Find(&shares).Error; err != nil {
		return nil, errs.DBQuery(err)
	}
	out := make([]shareMount, 0, len(shares))
	for _, s := range shares {
		if err := checkName(s.Name); err != nil {
			log.Printf("vfs: share %d 名字 %q 不能作为目录名，已从根目录视图隐藏", s.Id, s.Name)
			continue
		}
		out = append(out, shareMount{Share: s, Permission: permOf[s.Id]})
	}
	return out, nil
}

// mount 按目录名解析出一个 Share 的挂载实例（每次都会重新确认授权）。
func (r *RootFS) mount(name string) (*ShareFS, error) {
	share, perm, err := r.shareByName(name)
	if err != nil {
		return nil, err
	}
	return r.mountFor(share, perm), nil
}

// mountFor 返回某个 Share 的挂载实例，首次访问或元数据已变时重建。
func (r *RootFS) mountFor(share db.Share, perm db.SharePermission) *ShareFS {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.mounts[share.Id]; ok && sameMount(cur, share, perm) {
		return cur
	}
	fs := NewShareFS(r.pm, share, r.userID, perm)
	// 沿用之前派生出的密钥；这里要拷贝一份，ShareFS.ClearKey 会原地清零自己的 key。
	if key, ok := r.keys[share.Id]; ok {
		fs.key = append([]byte(nil), key...)
	}
	r.mounts[share.Id] = fs
	return fs
}

// sameMount 判断缓存的实例是否仍能代表库里的这条 Share：
// 权限或任何影响编解码/配额的字段变了就重建实例（会话密钥在 RootFS.keys 里，不丢）。
func sameMount(fs *ShareFS, share db.Share, perm db.SharePermission) bool {
	cur := fs.share
	return fs.perm == perm &&
		cur.Id == share.Id &&
		cur.Name == share.Name &&
		cur.PoolId == share.PoolId &&
		cur.Quota == share.Quota &&
		cur.Compression == share.Compression &&
		cur.Encryption == share.Encryption &&
		bytes.Equal(cur.EncryptionKeyHash, share.EncryptionKeyHash)
}

// ---------------------------------------------------------------
// 合成条目
// ---------------------------------------------------------------

// splitMount 把挂载点内的绝对路径拆成「第一段（Share 名）」与「余下路径」；
// 根目录返回 ("", "/")。
func splitMount(clean string) (string, string) {
	segs := pathSegments(clean)
	switch len(segs) {
	case 0:
		return "", "/"
	case 1:
		return segs[0], "/"
	default:
		return segs[0], "/" + strings.Join(segs[1:], "/")
	}
}

// rootInfo 合成根目录的条目。根是只读视图，Id 用 0（与 ShareFS 的合成根一致）。
func (r *RootFS) rootInfo() FileInfo {
	return FileInfo{
		Id:   0,
		Name: "/",
		Path: "/",
		Kind: KindDir,
		Mode: os.ModeDir | 0o555,
	}
}

// shareInfo 合成根下某个 Share 目录的条目。
//
// Id 取 -shares.id：根层条目的 Id 来自 shares 表，Share 内条目的 Id 来自 inodes 表，
// 两张表各自自增，取负数才能保证同一个挂载点内 Id 唯一（协议层可拿它当 FileId）。
// Uid/Gid 沿用 ShareFS 的约定：属主是 Share 的创建者，属组是 Share id。
func shareInfo(share db.Share, perm db.SharePermission) FileInfo {
	bits := FileMode(0o444)
	if perm >= db.ShareWrite {
		bits |= 0o222
	}
	ts := time.Unix(share.CreatedAt, 0)
	return FileInfo{
		Id:    -share.Id,
		Name:  share.Name,
		Path:  "/" + share.Name,
		Kind:  KindDir,
		Mode:  os.ModeDir | bits | 0o111, // 目录必须带 x 位，否则客户端进不去
		Uid:   uint32(share.CreatedBy),
		Gid:   uint32(share.Id),
		Atime: ts,
		Mtime: ts,
		Ctime: ts, // 三者同源：都取 Share 的 CreatedAt
	}
}

// prefixTarget 给转发回来的符号链接目标补上挂载点前缀：
// ShareFS 的 Target 是 Share 内的绝对路径（如 /a/b），
// RootFS 里要还原成 "/<share>/a/b" 才与 Readlink 的返回值一致。
func prefixTarget(name string, fi *FileInfo) {
	if fi.Target == "" {
		return
	}
	fi.Target = "/" + name + "/" + strings.TrimPrefix(fi.Target, "/")
}

// ---------------------------------------------------------------
// FileSystem 实现
// ---------------------------------------------------------------

// Stat 返回路径的元数据，跟随末端符号链接。
func (r *RootFS) Stat(ctx context.Context, p string) (FileInfo, error) {
	return r.stat(ctx, p, true)
}

// Lstat 与 Stat 相同，但不跟随末端符号链接。
func (r *RootFS) Lstat(ctx context.Context, p string) (FileInfo, error) {
	return r.stat(ctx, p, false)
}

func (r *RootFS) stat(ctx context.Context, p string, follow bool) (FileInfo, error) {
	clean, err := cleanPath(p)
	if err != nil {
		return FileInfo{}, err
	}
	name, rest := splitMount(clean)
	if name == "" {
		return r.rootInfo(), nil
	}
	share, perm, err := r.shareByName(name)
	if err != nil {
		return FileInfo{}, err
	}
	if rest == "/" {
		return shareInfo(share, perm), nil
	}
	fs := r.mountFor(share, perm)
	var fi FileInfo
	if follow {
		fi, err = fs.Stat(ctx, rest)
	} else {
		fi, err = fs.Lstat(ctx, rest)
	}
	if err != nil {
		return FileInfo{}, err
	}
	prefixTarget(name, &fi)
	return fi, nil
}

// ReadDir 列出目录下的条目；根目录下列出的是该用户可见的 Share。
func (r *RootFS) ReadDir(ctx context.Context, p string) ([]FileInfo, error) {
	clean, err := cleanPath(p)
	if err != nil {
		return nil, err
	}
	name, rest := splitMount(clean)
	if name == "" {
		shares, err := r.visibleShares()
		if err != nil {
			return nil, err
		}
		out := make([]FileInfo, 0, len(shares))
		for _, s := range shares {
			out = append(out, shareInfo(s.Share, s.Permission))
		}
		return out, nil
	}
	fs, err := r.mount(name)
	if err != nil {
		return nil, err
	}
	out, err := fs.ReadDir(ctx, rest)
	if err != nil {
		return nil, err
	}
	for i := range out {
		prefixTarget(name, &out[i])
	}
	return out, nil
}

// Mkdir 在某个 Share 内创建目录；根与挂载点根不接受写操作。
func (r *RootFS) Mkdir(ctx context.Context, p string, mode FileMode) error {
	clean, err := cleanPath(p)
	if err != nil {
		return err
	}
	name, rest := splitMount(clean)
	if name == "" || rest == "/" {
		return ErrNotSupported // 根是只读视图：Share 的增删属于授权管理
	}
	fs, err := r.mount(name)
	if err != nil {
		return err
	}
	return fs.Mkdir(ctx, rest, mode)
}

// Remove 删除某个 Share 内的条目；挂载点本身不能被文件系统接口删除。
func (r *RootFS) Remove(ctx context.Context, p string) error {
	clean, err := cleanPath(p)
	if err != nil {
		return err
	}
	name, rest := splitMount(clean)
	if name == "" || rest == "/" {
		return ErrNotSupported
	}
	fs, err := r.mount(name)
	if err != nil {
		return err
	}
	return fs.Remove(ctx, rest)
}

// Rename 在同一个 Share 内改名或移动；跨 Share 返回 ErrCrossDevice。
func (r *RootFS) Rename(ctx context.Context, oldPath, newPath string) error {
	oldClean, err := cleanPath(oldPath)
	if err != nil {
		return err
	}
	newClean, err := cleanPath(newPath)
	if err != nil {
		return err
	}
	oldName, oldRest := splitMount(oldClean)
	newName, newRest := splitMount(newClean)
	if oldName == "" || newName == "" || oldRest == "/" || newRest == "/" {
		return ErrNotSupported // 挂载点本身不能改名或移动
	}
	if oldName != newName {
		return ErrCrossDevice
	}
	fs, err := r.mount(oldName)
	if err != nil {
		return err
	}
	return fs.Rename(ctx, oldRest, newRest)
}

// Symlink 创建符号链接。target 与 linkPath 都按挂载点内的路径解释，
// 链接只能指向同一个 Share 内已存在的路径（ShareFS 用 inode 引用表达链接，
// 跨 Share 或指向根都表示不了）。
func (r *RootFS) Symlink(ctx context.Context, target, linkPath string) error {
	linkClean, err := cleanPath(linkPath)
	if err != nil {
		return err
	}
	linkName, linkRest := splitMount(linkClean)
	if linkName == "" || linkRest == "/" {
		return ErrNotSupported
	}
	targetClean, err := cleanPath(target)
	if err != nil {
		return err
	}
	targetName, targetRest := splitMount(targetClean)
	if targetName == "" || targetRest == "/" {
		return ErrNotSupported // 链接到根或挂载点根没有意义
	}
	if targetName != linkName {
		return ErrCrossDevice
	}
	fs, err := r.mount(linkName)
	if err != nil {
		return err
	}
	return fs.Symlink(ctx, targetRest, linkRest)
}

// Readlink 读取符号链接目标，返回值带挂载点前缀，
// 即 RootFS 命名空间里的绝对路径（如 /<share>/a/b）。
func (r *RootFS) Readlink(ctx context.Context, p string) (string, error) {
	clean, err := cleanPath(p)
	if err != nil {
		return "", err
	}
	name, rest := splitMount(clean)
	if name == "" {
		return "", ErrInvalid // 根不是符号链接
	}
	fs, err := r.mount(name)
	if err != nil {
		return "", err
	}
	target, err := fs.Readlink(ctx, rest)
	if err != nil {
		return "", err
	}
	return "/" + name + "/" + strings.TrimPrefix(target, "/"), nil
}

// Copy 在同一个 Share 内复制；跨 Share 返回 ErrCrossDevice。
func (r *RootFS) Copy(ctx context.Context, srcPath, dstPath string, recursive bool) error {
	srcClean, err := cleanPath(srcPath)
	if err != nil {
		return err
	}
	dstClean, err := cleanPath(dstPath)
	if err != nil {
		return err
	}
	srcName, srcRest := splitMount(srcClean)
	dstName, dstRest := splitMount(dstClean)
	if srcName == "" || dstName == "" || srcRest == "/" || dstRest == "/" {
		return ErrNotSupported // 挂载点本身不能作为复制源或目标
	}
	if srcName != dstName {
		return ErrCrossDevice
	}
	fs, err := r.mount(srcName)
	if err != nil {
		return err
	}
	return fs.Copy(ctx, srcRest, dstRest, recursive)
}

// SetAttr 修改某个 Share 内条目的元数据；根与挂载点的属性由授权管理决定。
func (r *RootFS) SetAttr(ctx context.Context, p string, attrs Attrs) error {
	clean, err := cleanPath(p)
	if err != nil {
		return err
	}
	name, rest := splitMount(clean)
	if name == "" || rest == "/" {
		return ErrNotSupported
	}
	fs, err := r.mount(name)
	if err != nil {
		return err
	}
	return fs.SetAttr(ctx, rest, attrs)
}

// Open 打开文件；根与挂载点根是目录，只读打开返回 ErrIsDir，
// 带写意图（含 Create）的打开属于结构性修改，返回 ErrNotSupported。
func (r *RootFS) Open(ctx context.Context, p string, flags OpenFlags, mode FileMode) (File, error) {
	clean, err := cleanPath(p)
	if err != nil {
		return nil, err
	}
	name, rest := splitMount(clean)
	if name == "" || rest == "/" {
		if flags.Write || flags.Create || flags.Truncate || flags.Append {
			return nil, ErrNotSupported
		}
		return nil, ErrIsDir
	}
	fs, err := r.mount(name)
	if err != nil {
		return nil, err
	}
	return fs.Open(ctx, rest, flags, mode)
}

// Create 等价于"在某个 Share 内写 + 创建 + 截断"；根层创建 Share 不受支持。
func (r *RootFS) Create(ctx context.Context, p string, mode FileMode) (File, error) {
	return r.Open(ctx, p, OpenFlags{Read: true, Write: true, Create: true, Truncate: true}, mode)
}

// StatFS 返回容量信息：Share 内的路径转发给 ShareFS（上限来自该 Share 的配额）；
// 根目录没有统一配额，只汇总该用户所有可见 Share 的已用量。
func (r *RootFS) StatFS(ctx context.Context, p string) (FSInfo, error) {
	clean, err := cleanPath(p)
	if err != nil {
		return FSInfo{}, err
	}
	name, rest := splitMount(clean)
	if name == "" {
		used, err := r.visibleUsedBytes()
		if err != nil {
			return FSInfo{}, err
		}
		return FSInfo{UsedBytes: uint64(used)}, nil
	}
	fs, err := r.mount(name)
	if err != nil {
		return FSInfo{}, err
	}
	return fs.StatFS(ctx, rest)
}

// visibleUsedBytes 统计该用户所有可见 Share 的已用量之和
// （口径与 ShareFS.usedBytes 一致：inode 当前版本大小之和，含软删除的文件）。
func (r *RootFS) visibleUsedBytes() (int64, error) {
	var total int64
	err := r.pm.DbManager.DB.Table("inodes").
		Joins("JOIN versions ON versions.id = inodes.version_id").
		Joins("JOIN share_users ON share_users.share_id = inodes.share_id").
		Where("share_users.user_id = ?", r.userID).
		Select("COALESCE(SUM(versions.size), 0)").
		Scan(&total).Error
	if err != nil {
		return 0, errs.DBQuery(err)
	}
	return total, nil
}
