package pool

import (
	"context"
	"log"
	"path"
	"strconv"
	"time"

	"github.com/zzc53/zenofs/internal/db"
)

const (
	cacheTTL          = 1 * time.Hour
	cacheCleanInterval = 60 * time.Second
)

// cacheDisks 返回 pool 中所有 Online 的 CacheDisk。
func (p *PoolManager) cacheDisks(poolId int64) []db.Disk {
	var disks []db.Disk
	p.DbManager.DB.Where("pool_id = ? AND type = ? AND status = ?",
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

// cachePath 返回缓存文件的相对路径。
func cachePath(chunkId int64) string {
	dir := strconv.FormatInt(chunkId%256, 10)
	return path.Join("cache", dir, strconv.FormatInt(chunkId, 10))
}

// tryReadCache 尝试从缓存读取 chunk 数据。
// 返回 nil, nil 表示缓存未命中。
func (p *PoolManager) tryReadCache(chunkId int64) ([]byte, error) {
	var entry db.ReadCache
	if err := p.DbManager.DB.Where("chunk_id = ? AND expired_at > ?", chunkId, time.Now().Unix()).First(&entry).Error; err != nil {
		return nil, nil // 缓存未命中
	}

	var disk db.Disk
	if err := p.DbManager.DB.First(&disk, entry.DiskId).Error; err != nil {
		return nil, nil
	}

	h := p.handlerFor(disk.Backend)
	if h == nil {
		return nil, nil
	}

	data, err := h.Read(disk, entry.Path)
	if err != nil {
		return nil, nil
	}

	// 命中后延长 TTL
	p.DbManager.DB.Model(&entry).Update("expired_at", time.Now().Unix()+int64(cacheTTL.Seconds()))
	return data, nil
}

// writeCache 将 chunk 数据写入缓存盘。
func (p *PoolManager) writeCache(poolId int64, chunkId int64, data []byte) {
	disks := p.cacheDisks(poolId)
	if len(disks) == 0 {
		return
	}
	cacheDisk := pickCacheDisk(disks, chunkId)
	if cacheDisk == nil {
		return
	}

	h := p.handlerFor(cacheDisk.Backend)
	if h == nil {
		return
	}

	relPath := cachePath(chunkId)
	if err := h.Write(*cacheDisk, relPath, data); err != nil {
		log.Printf("cache: write chunk %d failed: %v", chunkId, err)
		return
	}

	// 写入或更新缓存记录
	now := time.Now().Unix()
	p.DbManager.DB.Where("chunk_id = ?", chunkId).Delete(&db.ReadCache{})
	if err := p.DbManager.DB.Create(&db.ReadCache{
		ChunkId:   chunkId,
		Path:      relPath,
		DiskId:    cacheDisk.Id,
		ExpiredAt: now + int64(cacheTTL.Seconds()),
	}).Error; err != nil {
		log.Printf("cache: record chunk %d failed: %v", chunkId, err)
	}
}

// StartCacheCleaner 启动后台 goroutine 清理过期缓存。
func (p *PoolManager) StartCacheCleaner(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(cacheCleanInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.cleanupExpiredCache()
			}
		}
	}()
	log.Printf("cache cleaner started (interval=%s)", cacheCleanInterval)
}

// cleanupExpiredCache 删除所有过期的缓存条目。
// 磁盘文件在被重新缓存时自然覆盖，不单独删除。
func (p *PoolManager) cleanupExpiredCache() {
	var entries []db.ReadCache
	if err := p.DbManager.DB.Where("expired_at < ?", time.Now().Unix()).Find(&entries).Error; err != nil {
		log.Printf("cache: query expired entries failed: %v", err)
		return
	}
	if len(entries) == 0 {
		return
	}

	ids := make([]int64, len(entries))
	for i, e := range entries {
		ids[i] = e.Id
		_ = e
	}

	if err := p.DbManager.DB.Where("id IN ?", ids).Delete(&db.ReadCache{}).Error; err != nil {
		log.Printf("cache: delete expired entries failed: %v", err)
		return
	}
	log.Printf("cache: cleaned %d expired entries", len(entries))
}
