package vfs

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
	"github.com/zzc53/zenofs/internal/pool"
	"gorm.io/gorm"
)

// 保留策略（回收站 TTL / 版本上限）都是**每个 Share 独立配置**的，所以这里
// 一律从 shares 表取各自的阈值，而不是用一个全局常量。
const (
	// retentionInterval 是后台保留策略的轮询间隔。
	retentionInterval = 30 * time.Minute
	// retentionFirstRunDelay 是首轮之前等的时间（不影响正确性，只影响多久见效）。
	retentionFirstRunDelay = 1 * time.Minute
	// retentionBatchLimit 是单轮最多处理的条目数，避免一轮扫描长时间占住数据库。
	retentionBatchLimit = 2000
)

// SweepRecycle 按每个 Share 的回收站 TTL 彻底删除超期条目，返回清掉的条数。
//
// TTL 为 0 的 Share 直接跳过（不自动清除）。判定用 inode.updated_at——软删除
// 写的就是它，而 Restore 会刷新它，所以"恢复了再删"重新计时。
//
// 清理动作复用 purgeInode：元数据删干净的同时，没人再引用的分片也会真的从盘上
// 回收（和手动"彻底删除"同一条路径）。
func SweepRecycle(pm *pool.PoolManager, limit int) (int, error) {
	if limit <= 0 || limit > retentionBatchLimit {
		limit = retentionBatchLimit
	}
	now := time.Now().Unix()

	var ids []int64
	if err := pm.DbManager.DB.Table("inodes AS i").
		Joins("JOIN shares s ON s.id = i.share_id").
		Where("i.deleted = 1 AND s.recycle_ttl_hours > 0").
		Where("i.updated_at < ? - s.recycle_ttl_hours * 3600", now).
		Order("i.updated_at").
		Limit(limit).
		Pluck("i.id", &ids).Error; err != nil {
		return 0, errs.DBQuery(err)
	}

	cleared := 0
	for _, id := range ids {
		// 父项先被清掉时子项已经不存在了，跳过而不是报错（与 PurgeAll 一致）
		if err := purgeInode(pm, id); err != nil {
			if errors.Is(err, ErrNotExist) {
				continue
			}
			return cleared, err
		}
		cleared++
	}
	if cleared > 0 {
		log.Printf("retention: cleared %d expired trash entr(ies)", cleared)
	}
	return cleared, nil
}

// SweepVersions 按每个 Share 的版本上限裁剪历史版本，返回裁掉的版本数。
//
// 先聚合出"版本数已经超过上限"的文件，再逐个处理——比逐文件检查便宜得多，
// 而绝大多数文件根本不需要动。
func SweepVersions(pm *pool.PoolManager, limit int) (int, error) {
	if limit <= 0 || limit > retentionBatchLimit {
		limit = retentionBatchLimit
	}

	var rows []struct {
		InodeId int64
		Keep    int64
	}
	if err := pm.DbManager.DB.Table("versions AS v").
		Select("v.inode_id AS inode_id, s.version_keep AS keep").
		Joins("JOIN inodes i ON i.id = v.inode_id").
		Joins("JOIN shares s ON s.id = i.share_id").
		Where("s.version_keep > 0").
		Group("v.inode_id, s.version_keep").
		Having("COUNT(*) > s.version_keep").
		Limit(limit).
		Scan(&rows).Error; err != nil {
		return 0, errs.DBQuery(err)
	}

	trimmed := 0
	for _, row := range rows {
		n, err := trimVersions(pm, row.InodeId, row.Keep)
		if err != nil {
			// 文件可能在聚合查询之后刚被彻底删除，跳过
			if errors.Is(err, ErrNotExist) {
				continue
			}
			return trimmed, err
		}
		trimmed += n
	}
	if trimmed > 0 {
		log.Printf("retention: trimmed %d old version(s)", trimmed)
	}
	return trimmed, nil
}

// trimVersions 把一个文件的版本裁到 keep 个（含当前版本），返回裁掉的版本数。
//
// 保留规则：**当前版本（inode.version_id）一定保留**。它不一定是 id 最大的那个
// ——RestoreVersion 会把旧版本指回去，按"留最新的 N 个"一刀切会删掉活数据。
// 其余名额从新到旧补满，剩下的版本连同 version_chunk 记录一起删掉，再回收因此
// 变成无引用的分片（ReleaseChunks）。
//
// 代价说明：被裁掉的版本无法再回滚，而且此刻正打开着旧版本的句柄会读失败。
// 所以这是"按 Share 显式开启"的功能（version_keep 默认为 0，即不裁）。
func trimVersions(pm *pool.PoolManager, inodeId int64, keep int64) (int, error) {
	if keep <= 0 {
		return 0, nil
	}

	var (
		trimmed int
		stale   []pool.StaleFile
	)
	err := pm.DbManager.Tx(func(tx *gorm.DB) error {
		var in db.Inode
		if err := tx.Select("id", "version_id").First(&in, inodeId).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotExist
			}
			return errs.DBQuery(err)
		}

		var ids []int64
		if err := tx.Model(&db.Version{}).Where("inode_id = ?", inodeId).
			Order("id DESC").Pluck("id", &ids).Error; err != nil {
			return errs.DBQuery(err)
		}

		keepSet := make(map[int64]struct{}, keep)
		if in.VersionId.Valid {
			keepSet[in.VersionId.Int64] = struct{}{}
		}
		for _, id := range ids {
			if int64(len(keepSet)) >= keep {
				break
			}
			keepSet[id] = struct{}{}
		}

		var drop []int64
		for _, id := range ids {
			if _, ok := keepSet[id]; !ok {
				drop = append(drop, id)
			}
		}
		if len(drop) == 0 {
			return nil
		}

		chunkIds, err := pluckChunkIds(tx, drop)
		if err != nil {
			return err
		}
		if err := forEachBatch(drop, func(batch []int64) error {
			return tx.Where("version_id IN ?", batch).Delete(&db.VersionChunk{}).Error
		}); err != nil {
			return errs.DBQuery(err)
		}
		if err := forEachBatch(drop, func(batch []int64) error {
			return tx.Where("id IN ?", batch).Delete(&db.Version{}).Error
		}); err != nil {
			return errs.DBQuery(err)
		}

		res, err := pm.ReleaseChunks(tx, chunkIds)
		if err != nil {
			return err
		}
		stale = res.Stale
		trimmed = len(drop)
		return nil
	})
	if err != nil {
		return 0, err
	}
	pm.DeleteStaleFiles(stale)
	return trimmed, nil
}

// StartRetention 启动后台保留策略 worker：按 Share 配置清回收站、裁历史版本。
//
// 与 chunk 的孤儿回收（pool.StartOrphanGC）是两件事：那边管"没人引用的分片"，
// 这边管"还有人引用、但按策略不该留的数据"。两者叠加起来，磁盘占用才有上限。
func StartRetention(pm *pool.PoolManager, ctx context.Context) {
	go func() {
		wait := retentionFirstRunDelay
		for {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			if _, err := SweepRecycle(pm, retentionBatchLimit); err != nil {
				log.Printf("retention: recycle sweep failed: %v", err)
			}
			if _, err := SweepVersions(pm, retentionBatchLimit); err != nil {
				log.Printf("retention: version sweep failed: %v", err)
			}
			wait = retentionInterval
		}
	}()
	log.Printf("retention worker started (interval=%s)", retentionInterval)
}
