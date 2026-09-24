package vfs

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
)

// 回收站是在软删除（fs_write.go 的 markDeleted）之上的一层视图：
// 删除只把 inode 标成 deleted=1 并留审计事件，恢复就是把标记抹掉，
// 彻底删除才真正移除 inode/version/version_chunk 记录。

// DeletedEntry 是回收站里的一个条目。
type DeletedEntry struct {
	FileInfo        // 元数据（Id/Name/Path/Kind/Size/Mtime…按删除前的位置重建）
	DeletedAt int64 // 删除时间（Unix 秒）
	DeletedBy int64 // 删除者用户 id
}

// ListDeleted 列出这个 Share 里被软删除的条目，最近删除的在前。
func (fs *ShareFS) ListDeleted(_ context.Context) ([]DeletedEntry, error) {
	var inodes []db.Inode
	if err := fs.pm.DbManager.DB.
		Where("share_id = ? AND deleted = 1", fs.share.Id).
		Order("updated_at DESC, id DESC").Find(&inodes).Error; err != nil {
		return nil, errs.DBQuery(err)
	}
	out := make([]DeletedEntry, 0, len(inodes))
	for _, in := range inodes {
		// 按删除前的位置重建路径；父目录也已被删时同样能重建（只是路径暂时不可访问）
		p, err := fs.pathOf(in.Id)
		if err != nil {
			p = "/" + in.Name
		}
		out = append(out, DeletedEntry{
			FileInfo:  fs.buildInfo(in, p, fs.versionSize(in)),
			DeletedAt: in.UpdatedAt,
			DeletedBy: in.UpdatedBy,
		})
	}
	return out, nil
}

// Restore 恢复回收站里的一个条目，返回恢复后的元数据。
//
// 原父目录还在（且没被删）就放回原位；父目录已经不在了就恢复到 Share 根目录——
// "先删文件、再删空目录"是很常见的顺序，这样两步都能恢复回来。
// 目标位置已有同名条目时返回 ErrExist（调用方可以让用户改名后再试）。
func (fs *ShareFS) Restore(_ context.Context, inodeId int64) (FileInfo, error) {
	if err := fs.requireWrite(); err != nil {
		return FileInfo{}, err
	}
	in, err := fs.deletedInode(inodeId)
	if err != nil {
		return FileInfo{}, err
	}

	// 目标父目录：原父目录可用就用它，否则回落到 Share 根
	parent := sql.NullInt64{}
	if in.ParentId.Valid {
		var p db.Inode
		if err := fs.pm.DbManager.DB.First(&p, in.ParentId.Int64).Error; err == nil &&
			p.Deleted == 0 && p.Kind == db.InodeDir {
			parent = in.ParentId
		}
	}
	if err := fs.ensureNoConflict(parent, in.Name); err != nil {
		return FileInfo{}, err
	}

	now := time.Now().Unix()
	if err := fs.pm.DbManager.Tx(func(tx *gorm.DB) error {
		if err := tx.Model(&db.Inode{}).Where("id = ?", in.Id).
			Updates(map[string]any{
				"deleted":    0,
				"parent_id":  parent,
				"updated_by": fs.userID,
				"updated_at": now,
			}).Error; err != nil {
			return wrapDB(err)
		}
		return wrapDB(tx.Create(&db.InodeHistory{
			InodeId:     in.Id,
			EventType:   db.InodeRestored,
			NewParentId: parent,
			CreatedAt:   now,
		}).Error)
	}); err != nil {
		return FileInfo{}, err
	}

	var fresh db.Inode
	if err := fs.pm.DbManager.DB.First(&fresh, in.Id).Error; err != nil {
		return FileInfo{}, errs.DBQuery(err)
	}
	p, err := fs.pathOf(fresh.Id)
	if err != nil {
		p = "/" + fresh.Name
	}
	return fs.buildInfo(fresh, p, fs.versionSize(fresh)), nil
}

// Purge 彻底删除回收站里的一个条目（连同它的子项）。
//
// 存储层的 chunk 不在这里物理删除：pool 没有单块删除的接口，删掉会破坏条带校验的
// 一致性，这些孤儿 chunk 留给后续的 GC。
func (fs *ShareFS) Purge(_ context.Context, inodeId int64) error {
	if err := fs.requireWrite(); err != nil {
		return err
	}
	in, err := fs.deletedInode(inodeId)
	if err != nil {
		return err
	}
	return fs.purgeInode(in.Id)
}

// PurgeAll 清空这个 Share 的回收站，返回清掉的顶层条目数。
func (fs *ShareFS) PurgeAll(_ context.Context) (int, error) {
	if err := fs.requireWrite(); err != nil {
		return 0, err
	}
	var ids []int64
	if err := fs.pm.DbManager.DB.Model(&db.Inode{}).
		Where("share_id = ? AND deleted = 1", fs.share.Id).Pluck("id", &ids).Error; err != nil {
		return 0, errs.DBQuery(err)
	}
	n := 0
	for _, id := range ids {
		if err := fs.purgeInode(id); err != nil {
			// 父项被清掉时子项已经不存在了，跳过而不是报错
			if errors.Is(err, ErrNotExist) {
				continue
			}
			return n, err
		}
		n++
	}
	return n, nil
}

// ─────────────────────────────────────────────────────────────
// 内部
// ─────────────────────────────────────────────────────────────

// deletedInode 取出本 Share 里一个处于"已删除"状态的 inode。
func (fs *ShareFS) deletedInode(inodeId int64) (db.Inode, error) {
	var in db.Inode
	err := fs.pm.DbManager.DB.Where("id = ? AND share_id = ?", inodeId, fs.share.Id).First(&in).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return db.Inode{}, ErrNotExist
	}
	if err != nil {
		return db.Inode{}, errs.DBQuery(err)
	}
	if in.Deleted == 0 {
		return db.Inode{}, ErrInvalid // 没被删过，谈不上恢复或清除
	}
	return in, nil
}

// ensureNoConflict 检查目标目录下是否已有同名存活条目。
func (fs *ShareFS) ensureNoConflict(parent sql.NullInt64, name string) error {
	q := fs.pm.DbManager.DB.Model(&db.Inode{}).
		Where("share_id = ? AND name = ? AND deleted = 0", fs.share.Id, name)
	if parent.Valid {
		q = q.Where("parent_id = ?", parent.Int64)
	} else {
		q = q.Where("parent_id IS NULL")
	}
	var n int64
	if err := q.Count(&n).Error; err != nil {
		return errs.DBQuery(err)
	}
	if n > 0 {
		return ErrExist
	}
	return nil
}

// purgeInode 删掉一个 inode 及其全部子项、版本与版本切片映射。
func (fs *ShareFS) purgeInode(id int64) error {
	return fs.pm.DbManager.Tx(func(tx *gorm.DB) error {
		// 先收集整棵子树（inode 层级很浅，宽度优先足够）
		ids := []int64{id}
		for i := 0; i < len(ids); i++ {
			var children []int64
			if err := tx.Model(&db.Inode{}).Where("parent_id = ?", ids[i]).Pluck("id", &children).Error; err != nil {
				return errs.DBQuery(err)
			}
			ids = append(ids, children...)
			if len(ids) > maxPurgeNodes {
				return ErrLoop
			}
		}

		var versionIds []int64
		if err := tx.Model(&db.Version{}).Where("inode_id IN ?", ids).Pluck("id", &versionIds).Error; err != nil {
			return errs.DBQuery(err)
		}
		if len(versionIds) > 0 {
			if err := tx.Where("version_id IN ?", versionIds).Delete(&db.VersionChunk{}).Error; err != nil {
				return errs.DBQuery(err)
			}
		}
		if err := tx.Where("inode_id IN ?", ids).Delete(&db.Version{}).Error; err != nil {
			return errs.DBQuery(err)
		}
		res := tx.Where("id IN ?", ids).Delete(&db.Inode{})
		if res.Error != nil {
			return errs.DBQuery(res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotExist
		}
		return nil
	})
}

// maxPurgeNodes 是一次彻底删除允许触及的最大 inode 数，兜住异常数据。
const maxPurgeNodes = 100000
