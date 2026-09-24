package pool

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	// cachePromoteThreshold 被读取的次数超过它之后，chunk 才真正落到缓存盘。
	cachePromoteThreshold = 5
	// cacheIdleTTL 缓存条目多久没被访问即视为冷却，由后台清理淘汰。
	cacheIdleTTL = 1 * time.Hour
	// cacheCleanInterval 后台缓存清理的轮询间隔。
	cacheCleanInterval = 60 * time.Second
)

// ---------------------------------------------------------------
// 缓存盘选择
// ---------------------------------------------------------------

// cacheDisks 返回 pool 中所有 Online 的 CacheDisk。
func (p *PoolManager) cacheDisks(poolId int64) []db.Disk {
	var disks []db.Disk
	// 空路径的缓存盘同样是脏数据：读缓存失败会回退源盘，写缓存则会写错地方
	p.DbManager.DB.Where("pool_id = ? AND type = ? AND status = ? AND path <> ''",
		poolId, db.CacheDisk, db.Online).Find(&disks)
	return disks
}

// pickCacheDisk 按 chunkId 平均选择一个缓存盘。
func pickCacheDisk(disks []db.Disk, chunkId int64) *db.Disk {
	if len(disks) == 0 {
		return nil
	}
	return &disks[chunkId%int64(len(disks))]
}

// ---------------------------------------------------------------
// 批量缓存维护
// ---------------------------------------------------------------

// loadCaches 一次查出这批 chunk 的缓存记录，按 chunkId 建索引。
// 查不到记录的 chunk 不会出现在返回值里，调用方据此判定"首次访问"。
func (p *PoolManager) loadCaches(chunkIds []int64) map[int64]db.ReadCache {
	caches := make(map[int64]db.ReadCache, len(chunkIds))
	if len(chunkIds) == 0 {
		return caches
	}
	var entries []db.ReadCache
	if err := p.DbManager.DB.Where("chunk_id IN ?", chunkIds).Find(&entries).Error; err != nil {
		log.Printf("cache: load entries failed: %v", err)
		return caches
	}
	for _, e := range entries {
		caches[e.ChunkId] = e
	}
	return caches
}

// readCacheFile 从缓存盘读取一个已落盘的缓存条目。
func (p *PoolManager) readCacheFile(entry db.ReadCache, diskById map[int64]db.Disk) ([]byte, error) {
	disk, ok := diskById[entry.DiskId]
	if !ok {
		return nil, fmt.Errorf("cache disk %d not found", entry.DiskId)
	}
	h := p.handlerFor(disk.Backend)
	if h == nil {
		return nil, fmt.Errorf("no handler for backend %d", disk.Backend)
	}
	return h.Read(disk, entry.Path)
}

// updateCacheAccess 读取完成后批量维护这批 chunk 的缓存状态：
//
//	首次访问            —— 建一条计数记录（AccessCount=1，尚未落盘）
//	计数已超阈值且未落盘 —— 写缓存文件（随机路径）并把状态置为 Cached
//	其余（含 Cached 命中）—— AccessCount 批量 +1
//
// 只按分支攒好待办再批量落库，DB 往返固定为几条。
func (p *PoolManager) updateCacheAccess(poolId int64, chunks []db.Chunk, data [][]byte, caches map[int64]db.ReadCache) {
	disks := p.cacheDisks(poolId)
	if len(disks) == 0 {
		return // 没有缓存盘，不维护缓存
	}

	now := time.Now().Unix()
	var (
		inserts   []db.ReadCache // 首次访问，只记计数
		promoteAt []int          // 计数已超阈值，需要落盘的 chunk 下标
		bumpIds   []int64        // AccessCount 批量 +1
	)
	for i, c := range chunks {
		entry, ok := caches[c.Id]
		switch {
		case !ok:
			inserts = append(inserts, db.ReadCache{
				ChunkId:     c.Id,
				DiskId:      pickCacheDisk(disks, c.Id).Id,
				AccessCount: 1,
				Status:      db.NotCached,
			})
		case entry.Status != db.Cached && entry.AccessCount > cachePromoteThreshold:
			promoteAt = append(promoteAt, i)
		default:
			bumpIds = append(bumpIds, c.Id)
		}
	}

	// 达到阈值的 chunk 落盘：一次批量生成路径，并发写文件，再批量写回元数据。
	if len(promoteAt) > 0 {
		if paths, err := generateChunkPaths(len(promoteAt)); err != nil {
			log.Printf("cache: generate paths failed: %v", err)
		} else {
			promoted := make([]db.ReadCache, len(promoteAt))
			writeErrs := parallelEach(len(promoteAt), 0, func(k int) error {
				i := promoteAt[k]
				entry := caches[chunks[i].Id]
				disk := pickCacheDisk(disks, chunks[i].Id)
				h := p.handlerFor(disk.Backend)
				if h == nil {
					return errs.New(errs.ECODE_FILE_WRITE, errs.ESTR_FILE_WRITE,
						"no handler for backend", fmt.Sprintf("%d", disk.Backend))
				}
				if err := h.Write(*disk, paths[k], data[i]); err != nil {
					return err
				}
				promoted[k] = db.ReadCache{
					Id:          entry.Id,
					ChunkId:     entry.ChunkId,
					Path:        paths[k],
					DiskId:      disk.Id,
					AccessCount: entry.AccessCount,
					Status:      db.Cached,
					UpdatedAt:   now,
				}
				return nil
			})

			// 只把真正写成功的条目落库（CreatedAt 不在更新列里，保持原值）
			ok := make([]db.ReadCache, 0, len(promoteAt))
			for k, err := range writeErrs {
				if err != nil {
					log.Printf("cache: promote chunk %d failed: %v", chunks[promoteAt[k]].Id, err)
					continue
				}
				ok = append(ok, promoted[k])
			}
			if len(ok) > 0 {
				if err := p.DbManager.DB.Clauses(clause.OnConflict{
					Columns:   []clause.Column{{Name: "id"}},
					DoUpdates: clause.AssignmentColumns([]string{"path", "disk_id", "access_count", "status", "updated_at"}),
				}).Create(&ok).Error; err != nil {
					log.Printf("cache: save promoted entries failed: %v", err)
				}
			}
		}
	}

	if len(inserts) > 0 {
		// 并发读取同一 chunk 时可能同时判定为"首次访问"，
		// 用 DO NOTHING 让插入幂等，避免撞上 chunk_id 唯一索引。
		if err := p.DbManager.DB.Clauses(clause.OnConflict{DoNothing: true}).
			Create(&inserts).Error; err != nil {
			log.Printf("cache: insert entries failed: %v", err)
		}
	}

	// 一条 SQL 累加整批计数（并刷新 updated_at，供后台淘汰判断闲置）
	if len(bumpIds) > 0 {
		if err := p.DbManager.DB.Model(&db.ReadCache{}).
			Where("chunk_id IN ?", bumpIds).
			Updates(map[string]any{
				"access_count": gorm.Expr("access_count + ?", 1),
				"updated_at":   now,
			}).Error; err != nil {
			log.Printf("cache: bump access count failed: %v", err)
		}
	}
}

// dropCaches 批量清除这批 chunk 的缓存。
//
// 先删记录——缓存是数据的副本，写新数据前必须保证读不再命中旧内容，失败向上报错；
// 再并发删除缓存文件——文件残留只是垃圾，失败只记日志。
func (p *PoolManager) dropCaches(chunkIds []int64) error {
	if len(chunkIds) == 0 {
		return nil
	}
	caches := p.loadCaches(chunkIds)
	if len(caches) == 0 {
		return nil
	}

	ids := make([]int64, 0, len(caches))
	var files []db.ReadCache
	for _, e := range caches {
		ids = append(ids, e.ChunkId)
		if e.Status == db.Cached && e.Path != "" {
			files = append(files, e)
		}
	}

	if err := p.DbManager.DB.Where("chunk_id IN ?", ids).Delete(&db.ReadCache{}).Error; err != nil {
		return errs.DBQuery(err)
	}
	p.deleteCacheFiles(files)
	return nil
}

// deleteCacheFiles 并发删除这些缓存条目对应的缓存文件。
// 缓存文件只是副本，删除失败只记日志。
func (p *PoolManager) deleteCacheFiles(entries []db.ReadCache) {
	if len(entries) == 0 {
		return
	}

	// 一次查出涉及的缓存盘
	diskIds := make([]int64, len(entries))
	for i, e := range entries {
		diskIds[i] = e.DiskId
	}
	diskById, err := loadDisksByIds(p.DbManager.DB, uniqueIds(diskIds))
	if err != nil {
		log.Printf("cache: query cache disks failed: %v", err)
		return
	}

	deleteErrs := parallelEach(len(entries), 0, func(k int) error {
		e := entries[k]
		disk, ok := diskById[e.DiskId]
		if !ok {
			return fmt.Errorf("cache disk %d not found", e.DiskId)
		}
		h := p.handlerFor(disk.Backend)
		if h == nil {
			return fmt.Errorf("no handler for backend %d", disk.Backend)
		}
		return h.Delete(disk, e.Path)
	})
	for k, err := range deleteErrs {
		if err != nil {
			log.Printf("cache: delete file for chunk %d failed: %v", entries[k].ChunkId, err)
		}
	}
}

// ---------------------------------------------------------------
// 后台清理
// ---------------------------------------------------------------

// StartCacheCleaner 启动后台 goroutine，周期性淘汰冷却的缓存条目。
func (p *PoolManager) StartCacheCleaner(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(cacheCleanInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.cleanupColdCache()
			}
		}
	}()
	log.Printf("cache cleaner started (interval=%s)", cacheCleanInterval)
}

// cleanupColdCache 淘汰冷却的缓存条目：
//
//   - 未落盘的记录：计数从未超过阈值，说明没热起来，闲置超过 cacheIdleTTL 就丢弃；
//   - 已落盘的条目：闲置超过 cacheIdleTTL 说明热点已冷，删记录并删除缓存文件。
//
// 命中缓存会刷新 updated_at（见 updateCacheAccess），因此持续被读的条目不会被淘汰。
func (p *PoolManager) cleanupColdCache() {
	cutoff := time.Now().Unix() - int64(cacheIdleTTL.Seconds())

	var cold []db.ReadCache
	if err := p.DbManager.DB.Where("updated_at < ?", cutoff).Find(&cold).Error; err != nil {
		log.Printf("cache: query cold entries failed: %v", err)
		return
	}
	if len(cold) == 0 {
		return
	}

	ids := make([]int64, 0, len(cold))
	var files []db.ReadCache
	for _, e := range cold {
		ids = append(ids, e.Id)
		if e.Status == db.Cached && e.Path != "" {
			files = append(files, e)
		}
	}

	if err := p.DbManager.DB.Where("id IN ?", ids).Delete(&db.ReadCache{}).Error; err != nil {
		log.Printf("cache: delete cold entries failed: %v", err)
		return
	}
	p.deleteCacheFiles(files)
	log.Printf("cache: cleaned %d cold entries (%d files)", len(cold), len(files))
}
