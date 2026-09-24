package vfs

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
	"github.com/zzc53/zenofs/internal/pool"
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
// 元数据删干净之后，还会把"不再被任何版本引用"的存储层分片回滚成空槽：
// 数据文件从盘上删掉、槽位交还给存储池复用（见 purgeInode 与 pool.ReleaseChunks）。
func (fs *ShareFS) Purge(_ context.Context, inodeId int64) error {
	if err := fs.requireWrite(); err != nil {
		return err
	}
	in, err := fs.deletedInode(inodeId)
	if err != nil {
		return err
	}
	return purgeInode(fs.pm, in.Id)
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
		if err := purgeInode(fs.pm, id); err != nil {
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

// PurgeShare 删掉某个 Share 的全部文件元数据（inode / version / version_chunk /
// 审计历史），并把不再被任何版本引用的存储层分片回收成空槽。
//
// 这是"删除 Share"的数据部分：ShareUser / Share 行属于权限与配置，由调用方删。
// 这里不检查 Share 里还有没有活文件——那是调用方（API 的 force 开关）的职责。
//
// 与回收站的 Purge 走的是同一条释放路径（pool.ReleaseChunks），所以删除 Share
// 不再会像以前那样只删元数据、把整个 Share 的分片永久留在盘上。
func PurgeShare(pm *pool.PoolManager, shareId int64) (pool.ReleaseResult, error) {
	var released pool.ReleaseResult
	err := pm.DbManager.Tx(func(tx *gorm.DB) error {
		var inodeIds []int64
		if err := tx.Model(&db.Inode{}).Where("share_id = ?", shareId).
			Pluck("id", &inodeIds).Error; err != nil {
			return errs.DBQuery(err)
		}
		versionIds, err := pluckVersionIds(tx, inodeIds)
		if err != nil {
			return err
		}
		chunkIds, err := pluckChunkIds(tx, versionIds)
		if err != nil {
			return err
		}

		if err := forEachBatch(versionIds, func(batch []int64) error {
			return tx.Where("version_id IN ?", batch).Delete(&db.VersionChunk{}).Error
		}); err != nil {
			return errs.DBQuery(err)
		}
		if err := forEachBatch(inodeIds, func(batch []int64) error {
			return tx.Where("inode_id IN ?", batch).Delete(&db.Version{}).Error
		}); err != nil {
			return errs.DBQuery(err)
		}
		if err := forEachBatch(inodeIds, func(batch []int64) error {
			return tx.Where("inode_id IN ?", batch).Delete(&db.InodeHistory{}).Error
		}); err != nil {
			return errs.DBQuery(err)
		}
		if err := forEachBatch(inodeIds, func(batch []int64) error {
			return tx.Where("id IN ?", batch).Delete(&db.Inode{}).Error
		}); err != nil {
			return errs.DBQuery(err)
		}

		released, err = pm.ReleaseChunks(tx, chunkIds)
		return err
	})
	if err != nil {
		return pool.ReleaseResult{}, err
	}
	pm.DeleteStaleFiles(released.Stale)
	return released, nil
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

// purgeInode 删掉一个 inode 及其全部子项、版本与版本切片映射，并把不再被任何
// 版本引用的存储层分片回滚成空槽（见 pool.ReleaseChunks）。
//
// 与 ShareFS 无关（不校验挂载权限、也不查 share_id）：调用方必须先确认这个 inode
// 该删、且它属于哪个 Share 已经确定。这样回收站清理、TTL 过期、删除 Share 三条
// 路径能共用同一段逻辑。
//
// 三步顺序不能变：
//  1. 先收集候选 chunk id（来自本子树全部版本的切片映射）；
//  2. 删掉本子树自己的 version_chunks；
//  3. 再查"剩余引用"——剩下的只可能来自别的版本，有引用的一个都不能动。
//
// 2 与 3 必须在同一个事务里：fileHandle.Close 会把旧版本未触及的切片继承成新
// 版本的 version_chunks（切片级写时复制），分成两个事务就会漏掉这个新引用，
// 把还在被使用的 chunk 回滚掉。
//
// 磁盘文件的删除放在事务提交之后：那时旧路径已经不被任何元数据引用，
// 删除动作不可能和并发写入抢同一个文件。
func purgeInode(pm *pool.PoolManager, id int64) error {
	var stale []pool.StaleFile
	err := pm.DbManager.Tx(func(tx *gorm.DB) error {
		ids, err := collectSubtree(tx, id)
		if err != nil {
			return err
		}
		versionIds, err := pluckVersionIds(tx, ids)
		if err != nil {
			return err
		}
		chunkIds, err := pluckChunkIds(tx, versionIds)
		if err != nil {
			return err
		}

		if err := forEachBatch(versionIds, func(batch []int64) error {
			return tx.Where("version_id IN ?", batch).Delete(&db.VersionChunk{}).Error
		}); err != nil {
			return errs.DBQuery(err)
		}
		if err := forEachBatch(ids, func(batch []int64) error {
			return tx.Where("inode_id IN ?", batch).Delete(&db.Version{}).Error
		}); err != nil {
			return errs.DBQuery(err)
		}

		var deleted int64
		if err := forEachBatch(ids, func(batch []int64) error {
			res := tx.Where("id IN ?", batch).Delete(&db.Inode{})
			if res.Error != nil {
				return errs.DBQuery(res.Error)
			}
			deleted += res.RowsAffected
			return nil
		}); err != nil {
			return err
		}
		if deleted == 0 {
			return ErrNotExist
		}

		// 引用检查（"还有没有别的版本引用它"）由池层在同一个事务里做，
		// 这里只负责把候选交出去——见 pool.ReleaseChunks 的第 0 步。
		res, err := pm.ReleaseChunks(tx, chunkIds)
		if err != nil {
			return err
		}
		stale = res.Stale
		return nil
	})
	if err != nil {
		return err
	}
	pm.DeleteStaleFiles(stale)
	return nil
}

// purgeBatchSize 是单条 SQL 一次处理的 id 数上限。
// SQL 的绑定变量数量有限（SQLite 尤其保守），而一棵子树的 inode 数可能上万。
const purgeBatchSize = 500

// forEachBatch 按 purgeBatchSize 切分 ids，依次调用 fn。
func forEachBatch(ids []int64, fn func(batch []int64) error) error {
	for start := 0; start < len(ids); start += purgeBatchSize {
		end := start + purgeBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		if err := fn(ids[start:end]); err != nil {
			return err
		}
	}
	return nil
}

// collectSubtree 收集 id 及其全部子孙 inode（含已软删的），逐层批量查询。
func collectSubtree(tx *gorm.DB, id int64) ([]int64, error) {
	ids := []int64{id}
	for frontier := []int64{id}; len(frontier) > 0; {
		var children []int64
		if err := forEachBatch(frontier, func(batch []int64) error {
			var got []int64
			if err := tx.Model(&db.Inode{}).Where("parent_id IN ?", batch).Pluck("id", &got).Error; err != nil {
				return errs.DBQuery(err)
			}
			children = append(children, got...)
			return nil
		}); err != nil {
			return nil, err
		}
		if len(ids)+len(children) > maxPurgeNodes {
			return nil, ErrLoop
		}
		ids = append(ids, children...)
		frontier = children
	}
	return ids, nil
}

// pluckVersionIds 取这批 inode 的全部版本 id（含历史版本）。
func pluckVersionIds(tx *gorm.DB, inodeIds []int64) ([]int64, error) {
	var out []int64
	if err := forEachBatch(inodeIds, func(batch []int64) error {
		var got []int64
		if err := tx.Model(&db.Version{}).Where("inode_id IN ?", batch).Pluck("id", &got).Error; err != nil {
			return errs.DBQuery(err)
		}
		out = append(out, got...)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// pluckChunkIds 取这批版本引用到的 chunk id（去重）。同一分片被同一文件的多个
// 版本共享是常态（写时复制），去重能省掉大量重复处理。
func pluckChunkIds(tx *gorm.DB, versionIds []int64) ([]int64, error) {
	seen := make(map[int64]struct{})
	var out []int64
	if err := forEachBatch(versionIds, func(batch []int64) error {
		var got []int64
		if err := tx.Model(&db.VersionChunk{}).Where("version_id IN ?", batch).
			Pluck("chunk_id", &got).Error; err != nil {
			return errs.DBQuery(err)
		}
		for _, id := range got {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// maxPurgeNodes 是一次彻底删除允许触及的最大 inode 数，兜住异常数据。
const maxPurgeNodes = 100000
