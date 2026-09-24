package vfs

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
	"gorm.io/gorm"
)

// wrapDB 把非 nil 的数据库错误包装成统一的 ZenoError。
func wrapDB(err error) error {
	if err == nil {
		return nil
	}
	return errs.DBQuery(err)
}

// parentID 把节点转换成 "parent_id" 列值；根目录（Id 0）对应 NULL。
// refOf 把一个 inode 包成 parent_id 列用的引用（0 表示"没有父"，即挂载根）。
//
// 注意它返回的是**这个 inode 自己的 id**，不是它的父目录——它以前叫 parentID，
// 正因如此被误当成"源文件的父目录"，导致改名总被记成移动。
func refOf(in db.Inode) sql.NullInt64 {
	if in.Id == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: in.Id, Valid: true}
}

// parentOf 返回 inode 所在目录的 id；父为空（挂在根下）返回 0。
func parentOf(in db.Inode) int64 {
	if in.ParentId.Valid {
		return in.ParentId.Int64
	}
	return 0
}

// checkName 校验单个文件名的合法性。
func checkName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return ErrInvalid
	}
	if len(name) > maxNameLen {
		return ErrNameTooLong
	}
	return nil
}

// isDescendant 判断 nodeID 是否位于 ancestorID 之下。
// 用于阻止"把目录移动进自己的子目录"这种会形成环的操作。
func (fs *ShareFS) isDescendant(ancestorID, nodeID int64) bool {
	id := nodeID
	for depth := 0; depth < maxPathDepth && id != 0; depth++ {
		var in db.Inode
		if err := fs.pm.DbManager.DB.Select("id", "parent_id").First(&in, id).Error; err != nil {
			return false
		}
		if !in.ParentId.Valid {
			return false
		}
		if in.ParentId.Int64 == ancestorID {
			return true
		}
		id = in.ParentId.Int64
	}
	return false
}

// markDeleted 软删除 inode，并写一条审计事件。
// 数据与历史版本都保留，后续的"已删除文件恢复/清空"功能依赖这一点。
func (fs *ShareFS) markDeleted(in db.Inode, ev db.InodeEventType) error {
	now := time.Now().Unix()
	return fs.pm.DbManager.Tx(func(tx *gorm.DB) error {
		if err := tx.Model(&db.Inode{}).Where("id = ?", in.Id).
			Updates(map[string]any{
				"deleted":    1,
				"updated_by": fs.userID,
				"updated_at": now,
			}).Error; err != nil {
			return errs.DBQuery(err)
		}
		return wrapDB(tx.Create(&db.InodeHistory{
			InodeId:   in.Id,
			EventType: ev,
			OldName:   sql.NullString{String: in.Name, Valid: true},
			CreatedAt: now,
		}).Error)
	})
}

// requireDir 解析路径并要求它是个目录，返回该目录 inode。
func (fs *ShareFS) requireDir(p string) (db.Inode, error) {
	in, err := fs.lookup(p, true)
	if err != nil {
		return db.Inode{}, err
	}
	if in.Kind != db.InodeDir {
		return db.Inode{}, ErrNotDir
	}
	return in, nil
}

// ---------------------------------------------------------------
// 写操作
// ---------------------------------------------------------------

// Mkdir 创建一个目录，父目录必须已存在。
func (fs *ShareFS) Mkdir(_ context.Context, p string, mode FileMode) error {
	if err := fs.requireWrite(); err != nil {
		return err
	}
	clean, err := cleanPath(p)
	if err != nil {
		return err
	}
	if clean == "/" {
		return ErrExist
	}
	parent, err := fs.requireDir(parentPath(clean))
	if err != nil {
		return err
	}
	name := baseName(clean)
	if err := checkName(name); err != nil {
		return err
	}
	if _, err := fs.lookup(clean, false); err == nil {
		return ErrExist
	} else if !errors.Is(err, ErrNotExist) {
		return err
	}

	in := db.Inode{
		ParentId:   refOf(parent),
		Name:       name,
		Kind:       db.InodeDir,
		ShareId:    fs.share.Id,
		Executable: boolToInt8(mode&0o111 != 0),
		CreatedBy:  fs.userID,
		UpdatedBy:  fs.userID,
	}
	return wrapDB(fs.pm.DbManager.DB.Create(&in).Error)
}

// Remove 删除文件、符号链接或空目录（软删除，数据保留）。
func (fs *ShareFS) Remove(_ context.Context, p string) error {
	if err := fs.requireWrite(); err != nil {
		return err
	}
	clean, err := cleanPath(p)
	if err != nil {
		return err
	}
	if clean == "/" {
		return ErrIsDir // 挂载根不能删
	}
	in, err := fs.lookup(clean, false)
	if err != nil {
		return err
	}
	if in.Kind == db.InodeDir {
		var n int64
		if err := fs.pm.DbManager.DB.Model(&db.Inode{}).
			Where("parent_id = ? AND deleted = 0", in.Id).Count(&n).Error; err != nil {
			return errs.DBQuery(err)
		}
		if n > 0 {
			return ErrNotEmpty
		}
	}
	return fs.markDeleted(in, db.InodeDeleted)
}

// Rename 在同一 Share 内改名或移动，目标已存在时按 POSIX 语义覆盖。
func (fs *ShareFS) Rename(_ context.Context, oldPath, newPath string) error {
	if err := fs.requireWrite(); err != nil {
		return err
	}
	oldClean, err := cleanPath(oldPath)
	if err != nil {
		return err
	}
	newClean, err := cleanPath(newPath)
	if err != nil {
		return err
	}
	if oldClean == "/" || newClean == "/" {
		return ErrInvalid
	}
	if oldClean == newClean {
		return nil
	}

	src, err := fs.lookup(oldClean, false)
	if err != nil {
		return err
	}
	dstDir, err := fs.requireDir(parentPath(newClean))
	if err != nil {
		return err
	}
	name := baseName(newClean)
	if err := checkName(name); err != nil {
		return err
	}
	// 目录不能移动到自己或自己的子目录下
	if src.Kind == db.InodeDir && dstDir.Id != 0 &&
		(dstDir.Id == src.Id || fs.isDescendant(src.Id, dstDir.Id)) {
		return ErrInvalid
	}

	// 目标已存在时的 POSIX 处理：目录必须为空，文件直接覆盖
	var overwrite *db.Inode
	switch dst, err := fs.lookup(newClean, false); {
	case err == nil:
		if dst.Id == src.Id {
			return nil
		}
		if src.Kind == db.InodeDir && dst.Kind != db.InodeDir {
			return ErrNotDir
		}
		if src.Kind != db.InodeDir && dst.Kind == db.InodeDir {
			return ErrIsDir
		}
		if dst.Kind == db.InodeDir {
			var n int64
			if err := fs.pm.DbManager.DB.Model(&db.Inode{}).
				Where("parent_id = ? AND deleted = 0", dst.Id).Count(&n).Error; err != nil {
				return errs.DBQuery(err)
			}
			if n > 0 {
				return ErrNotEmpty
			}
		}
		overwrite = &dst
	case !errors.Is(err, ErrNotExist):
		return err
	}

	now := time.Now().Unix()
	return fs.pm.DbManager.Tx(func(tx *gorm.DB) error {
		if overwrite != nil {
			if err := tx.Model(&db.Inode{}).Where("id = ?", overwrite.Id).
				Updates(map[string]any{"deleted": 1, "updated_by": fs.userID, "updated_at": now}).
				Error; err != nil {
				return errs.DBQuery(err)
			}
		}
		if err := tx.Model(&db.Inode{}).Where("id = ?", src.Id).
			Updates(map[string]any{
				"parent_id":  refOf(dstDir),
				"name":       name,
				"updated_by": fs.userID,
				"updated_at": now,
			}).Error; err != nil {
			return errs.DBQuery(err)
		}
		// 改名还是移动，看新旧父目录变没变（dstDir 就是新父目录）
		ev := db.InodeRenamed
		if parentOf(src) != dstDir.Id {
			ev = db.InodeMoved
		}
		return wrapDB(tx.Create(&db.InodeHistory{
			InodeId:     src.Id,
			EventType:   ev,
			OldName:     sql.NullString{String: src.Name, Valid: true},
			NewName:     sql.NullString{String: name, Valid: true},
			OldParentId: src.ParentId,
			NewParentId: refOf(dstDir),
			CreatedAt:   now,
		}).Error)
	})
}

// Symlink 创建指向 target 的符号链接。
//
// 注意：模型的 LinkId 存的是"目标 inode 引用"而不是路径字符串，
// 所以 target 必须是本 Share 内已经存在的路径。
func (fs *ShareFS) Symlink(_ context.Context, target, linkPath string) error {
	if err := fs.requireWrite(); err != nil {
		return err
	}
	targetClean, err := cleanPath(target)
	if err != nil {
		return err
	}
	tgt, err := fs.lookup(targetClean, true)
	if err != nil {
		return err
	}
	if tgt.Id == 0 {
		return ErrInvalid // 不能链接到挂载根
	}
	linkClean, err := cleanPath(linkPath)
	if err != nil {
		return err
	}
	if linkClean == "/" {
		return ErrExist
	}
	parent, err := fs.requireDir(parentPath(linkClean))
	if err != nil {
		return err
	}
	name := baseName(linkClean)
	if err := checkName(name); err != nil {
		return err
	}
	if _, err := fs.lookup(linkClean, false); err == nil {
		return ErrExist
	} else if !errors.Is(err, ErrNotExist) {
		return err
	}

	in := db.Inode{
		ParentId:  refOf(parent),
		Name:      name,
		Kind:      db.InodeLink,
		ShareId:   fs.share.Id,
		LinkId:    sql.NullInt64{Int64: tgt.Id, Valid: true},
		CreatedBy: fs.userID,
		UpdatedBy: fs.userID,
	}
	return wrapDB(fs.pm.DbManager.DB.Create(&in).Error)
}

// Copy 复制文件或目录树；目标已存在时返回 ErrExist。
func (fs *ShareFS) Copy(ctx context.Context, srcPath, dstPath string, recursive bool) error {
	if err := fs.requireWrite(); err != nil {
		return err
	}
	srcClean, err := cleanPath(srcPath)
	if err != nil {
		return err
	}
	dstClean, err := cleanPath(dstPath)
	if err != nil {
		return err
	}
	src, err := fs.lookup(srcClean, true)
	if err != nil {
		return err
	}
	if src.Id == 0 || src.Id == 0 && dstClean == "/" {
		return ErrInvalid
	}
	if _, err := fs.lookup(dstClean, false); err == nil {
		return ErrExist
	}
	return fs.copyNode(ctx, srcClean, dstClean, recursive)
}

// copyNode 按类型分发复制：目录递归、文件走读写句柄。
func (fs *ShareFS) copyNode(ctx context.Context, srcPath, dstPath string, recursive bool) error {
	src, err := fs.lookup(srcPath, false)
	if err != nil {
		return err
	}
	switch src.Kind {
	case db.InodeDir:
		if !recursive {
			return ErrIsDir
		}
		if err := fs.Mkdir(ctx, dstPath, inodeMode(src)); err != nil {
			return err
		}
		children, err := fs.ReadDir(ctx, srcPath)
		if err != nil {
			return err
		}
		for _, c := range children {
			if err := fs.copyNode(ctx, joinPath(srcPath, c.Name), joinPath(dstPath, c.Name), recursive); err != nil {
				return err
			}
		}
		return nil

	case db.InodeLink:
		if !src.LinkId.Valid {
			return ErrInvalid
		}
		target, err := fs.pathOf(src.LinkId.Int64)
		if err != nil {
			return err
		}
		return fs.Symlink(ctx, target, dstPath)

	default:
		return fs.copyFileContent(ctx, srcPath, dstPath)
	}
}

// copyFileContent 把一个文件的内容完整复制到新文件。
func (fs *ShareFS) copyFileContent(ctx context.Context, srcPath, dstPath string) error {
	src, err := fs.Open(ctx, srcPath, OpenFlags{Read: true}, 0)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := fs.Create(ctx, dstPath, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}

// inodeMode 把 inode 的执行位转成 FileMode（复制目录时沿用）。
func inodeMode(in db.Inode) FileMode {
	if in.Executable != 0 {
		return 0o755
	}
	return 0o644
}

// SetAttr 修改元数据。
//
// 映射关系（见 db.Inode 的注释）：
//   - Mode → 只影响 Executable（即可执行位）；
//   - Mtime / Atime → 都写 inode 的 UpdatedAt（三者同源）；
//   - Size → 走 Truncate；
//   - Uid / Gid → 忽略：Gid 就是 ShareId（改了等于换 Share），Uid 只在创建时确定。
//     客户端（尤其 SMB）经常带上这两个字段，直接忽略比报错更兼容。
func (fs *ShareFS) SetAttr(ctx context.Context, p string, attrs Attrs) error {
	if err := fs.requireWrite(); err != nil {
		return err
	}
	clean, err := cleanPath(p)
	if err != nil {
		return err
	}
	in, err := fs.lookup(clean, false)
	if err != nil {
		return err
	}
	if attrs.Size != nil {
		return fs.truncatePath(ctx, clean, *attrs.Size)
	}

	now := time.Now().Unix()
	updates := map[string]any{"updated_by": fs.userID, "updated_at": now}
	changed := false
	if attrs.Mode != nil {
		updates["executable"] = boolToInt8(*attrs.Mode&0o111 != 0)
		changed = true
	}
	if attrs.Mtime != nil {
		updates["updated_at"] = attrs.Mtime.Unix()
		changed = true
	}
	if attrs.Atime != nil && attrs.Mtime == nil {
		updates["updated_at"] = attrs.Atime.Unix()
		changed = true
	}
	if !changed {
		return nil // 只有 Uid/Gid 之类的不可改字段
	}
	return wrapDB(fs.pm.DbManager.DB.Model(&db.Inode{}).Where("id = ?", in.Id).
		Updates(updates).Error)
}

// boolToInt8 把布尔转成模型里用的 0/1。
func boolToInt8(b bool) int8 {
	if b {
		return 1
	}
	return 0
}
