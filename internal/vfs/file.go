package vfs

import (
	"context"
	"errors"
	"io"
	"log"
	"time"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
	"gorm.io/gorm"
)

// fileHandle 实现 vfs.File。
//
// # 读
//
// 以"打开时的那个版本"为准（快照语义）：切片按 Idx 懒加载，缺某个 Idx 就是
// 稀疏空洞，读作零。别人在本次句柄存活期间提交的新版本看不到。
//
// # 写
//
// 切片级写时复制：写哪个 Idx 就重建哪个切片，没触及的 Idx 沿用旧版本的 chunk。
// 新切片一旦生成就立刻写进存储池并同步写 VersionChunk 记录；句柄里只保留
// "哪些 Idx 已被重建"的映射，以及当前正在写的那一个切片的明文缓冲
// （同一 Idx 被反复写时不必反复读回存储层）。
// Version 本身（大小、哈希）和 Inode.VersionId 的切换在 Close 时一次性完成。
type fileHandle struct {
	fs    *ShareFS
	inode db.Inode

	readable bool
	writable bool
	append   bool
	offset   int64
	closed   bool

	// 打开时的版本快照，读操作都基于它
	oldVer    db.Version
	oldChunks map[int64]db.VersionChunk

	// 本次写入
	newVer    db.Version
	newChunks map[int64]db.VersionChunk
	dirty     bool

	// 当前正在写的切片：避免同一 Idx 反复读回存储层
	hasActive bool
	activeIdx int64
	activeBuf []byte
}

var _ File = (*fileHandle)(nil)

// ---------------------------------------------------------------
// 打开与创建
// ---------------------------------------------------------------

// Open 打开文件；flags 的语义与 open(2) 一致。
func (fs *ShareFS) Open(ctx context.Context, p string, flags OpenFlags, mode FileMode) (File, error) {
	clean, err := cleanPath(p)
	if err != nil {
		return nil, err
	}
	if flags.Write || flags.Truncate {
		if err := fs.requireWrite(); err != nil {
			return nil, err
		}
	}
	// 读写文件内容都需要编解码能力：算法要认识，启用加密时必须已提供口令。
	// 元数据操作（Stat/ReadDir 等）不需要密钥，不受这里影响。
	if err := fs.requireCodec(); err != nil {
		return nil, err
	}

	in, err := fs.lookup(clean, true)
	switch {
	case err == nil:
		if in.Kind == db.InodeDir {
			return nil, ErrIsDir
		}
		if flags.Create && flags.Exclusive {
			return nil, ErrExist
		}
	case errors.Is(err, ErrNotExist):
		if !flags.Create {
			return nil, err
		}
		if in, err = fs.createInode(parentPath(clean), baseName(clean), db.InodeFile, mode); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}

	f := &fileHandle{
		fs:       fs,
		inode:    in,
		readable: flags.Read || !flags.Write,
		writable: flags.Write,
		append:   flags.Append,
	}
	if err := f.loadSnapshot(); err != nil {
		return nil, err
	}
	if f.writable {
		if err := f.beginWrite(ctx); err != nil {
			return nil, err
		}
		if flags.Truncate {
			if err := f.truncateTo(0); err != nil {
				f.Close()
				return nil, err
			}
		}
	}
	return f, nil
}

// Create 等价于"写 + 创建 + 截断"。
func (fs *ShareFS) Create(ctx context.Context, p string, mode FileMode) (File, error) {
	return fs.Open(ctx, p, OpenFlags{Read: true, Write: true, Create: true, Truncate: true}, mode)
}

// createInode 在 parent 下创建一个指定类型的 inode。
func (fs *ShareFS) createInode(parentPath, name string, kind db.InodeKind, mode FileMode) (db.Inode, error) {
	if err := checkName(name); err != nil {
		return db.Inode{}, err
	}
	dir, err := fs.requireDir(parentPath)
	if err != nil {
		return db.Inode{}, err
	}
	in := db.Inode{
		ParentId:   refOf(dir),
		Name:       name,
		Kind:       kind,
		ShareId:    fs.share.Id,
		Executable: boolToInt8(mode&0o111 != 0),
		CreatedBy:  fs.userID,
		UpdatedBy:  fs.userID,
	}
	if err := fs.pm.DbManager.DB.Create(&in).Error; err != nil {
		return db.Inode{}, errs.DBQuery(err)
	}
	return in, nil
}

// loadSnapshot 载入"打开时"的版本及其切片映射。
func (f *fileHandle) loadSnapshot() error {
	f.oldChunks = map[int64]db.VersionChunk{}
	if !f.inode.VersionId.Valid {
		return nil // 新建的空文件还没有版本
	}
	gdb := f.fs.pm.DbManager.DB
	if err := gdb.First(&f.oldVer, f.inode.VersionId.Int64).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return errs.DBQuery(err)
	}
	var vcs []db.VersionChunk
	if err := gdb.Where("version_id = ?", f.oldVer.Id).Find(&vcs).Error; err != nil {
		return errs.DBQuery(err)
	}
	for _, vc := range vcs {
		f.oldChunks[vc.Idx] = vc
	}
	return nil
}

// ---------------------------------------------------------------
// 读
// ---------------------------------------------------------------

// Read 从当前偏移读取并把偏移前移。
func (f *fileHandle) Read(p []byte) (int, error) {
	n, err := f.ReadAt(p, f.offset)
	f.offset += int64(n)
	return n, err
}

// ReadAt 从指定偏移读取，不改动当前偏移。
// 稀疏空洞（没有对应切片的 Idx）直接返回零。
func (f *fileHandle) ReadAt(p []byte, off int64) (int, error) {
	if !f.readable {
		return 0, ErrPermission
	}
	if off < 0 {
		return 0, ErrInvalid
	}
	if len(p) == 0 {
		return 0, nil
	}
	size := f.size()
	if off >= size {
		return 0, io.EOF
	}
	if max := size - off; int64(len(p)) > max {
		p = p[:max] // 读到文件末尾就截断，不补零
	}

	slice := f.fs.sliceSize()
	got := 0
	for got < len(p) {
		idx := (off + int64(got)) / slice
		intra := (off + int64(got)) % slice
		n := int64(len(p) - got)
		if remain := slice - intra; n > remain {
			n = remain
		}

		buf, err := f.slicePlain(idx)
		if err != nil {
			return got, err
		}
		// 空洞或切片比请求区间短：剩下的当成零
		for i := int64(0); i < n; i++ {
			if pos := intra + i; pos < int64(len(buf)) {
				p[got+int(i)] = buf[pos]
			} else {
				p[got+int(i)] = 0
			}
		}
		got += int(n)
	}

	if got < len(p) {
		return got, io.EOF
	}
	return got, nil
}

// Seek 移动当前偏移。
func (f *fileHandle) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = f.offset + offset
	case io.SeekEnd:
		abs = f.size() + offset
	default:
		return 0, ErrInvalid
	}
	if abs < 0 {
		return 0, ErrInvalid
	}
	f.offset = abs
	return abs, nil
}

// slicePlain 取某个 Idx 的明文内容。
// 优先级：本次写入的缓冲 → 本次写入已落盘的切片 → 打开时的旧切片 → 空洞（nil）。
func (f *fileHandle) slicePlain(idx int64) ([]byte, error) {
	if f.hasActive && f.activeIdx == idx {
		return f.activeBuf, nil
	}
	if vc, ok := f.newChunks[idx]; ok {
		return f.fs.readSlice(vc, f.newVer.Compression, f.newVer.Encryption)
	}
	if vc, ok := f.oldChunks[idx]; ok {
		return f.fs.readSlice(vc, f.oldVer.Compression, f.oldVer.Encryption)
	}
	return nil, nil
}

// size 返回该句柄当前看到的文件大小。
// 写入中的句柄返回"尚未提交的新大小"，因此写完立刻能 Stat 到。
func (f *fileHandle) size() int64 {
	if f.writable {
		return f.newVer.Size
	}
	return f.oldVer.Size
}

// readSlice 从存储层读出一个切片，按该切片所属版本的算法还原成明文。
// 读失败时顺手为该条带投递重建任务（尽力而为，不影响本次错误返回）。
func (fs *ShareFS) readSlice(vc db.VersionChunk, comp, enc int8) ([]byte, error) {
	data, err := fs.pm.ReadChunks(fs.share.PoolId, []int64{vc.ChunkId})
	if err != nil {
		fs.triggerRebuild(vc.ChunkId)
		return nil, err
	}
	plain, err := fs.decodeSlice(data[0], comp, enc)
	if err != nil {
		// 解密/解压失败通常意味着分片内容坏了，同样触发重建
		fs.triggerRebuild(vc.ChunkId)
		return nil, err
	}
	return plain, nil
}

// triggerRebuild 为坏 chunk 所属的条带投递重建任务。失败只记日志。
func (fs *ShareFS) triggerRebuild(chunkId int64) {
	if _, err := fs.pm.RebuildByChunks([]int64{chunkId}); err != nil {
		log.Printf("vfs: trigger rebuild for chunk %d failed: %v", chunkId, err)
	}
}

// Stat 返回句柄对应文件的最新元数据。
func (f *fileHandle) Stat() (FileInfo, error) {
	var in db.Inode
	if err := f.fs.pm.DbManager.DB.First(&in, f.inode.Id).Error; err != nil {
		return FileInfo{}, ErrNotExist
	}
	fi := f.fs.buildInfo(in, "", f.size())
	fi.Name = in.Name
	return fi, nil
}

// SetAttr 在句柄上修改属性。
func (f *fileHandle) SetAttr(attrs Attrs) error {
	if !f.writable {
		return ErrPermission
	}
	if attrs.Size != nil {
		return f.truncateTo(*attrs.Size)
	}
	if attrs.Mode == nil && attrs.Mtime == nil && attrs.Atime == nil {
		return nil
	}
	updates := map[string]any{"updated_by": f.fs.userID, "updated_at": time.Now().Unix()}
	if attrs.Mode != nil {
		updates["executable"] = boolToInt8(*attrs.Mode&0o111 != 0)
	}
	if attrs.Mtime != nil {
		updates["updated_at"] = attrs.Mtime.Unix()
	}
	return wrapDB(f.fs.pm.DbManager.DB.Model(&db.Inode{}).Where("id = ?", f.inode.Id).
		Updates(updates).Error)
}

// truncatePath 打开文件做一次截断后关闭（SetAttr 的 Size 走这条路）。
func (fs *ShareFS) truncatePath(ctx context.Context, p string, size int64) error {
	f, err := fs.Open(ctx, p, OpenFlags{Read: true, Write: true}, 0)
	if err != nil {
		return err
	}
	h := f.(*fileHandle)
	if err := h.truncateTo(size); err != nil {
		h.Close()
		return err
	}
	return h.Close()
}
