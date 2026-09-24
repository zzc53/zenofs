package pool

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// PoolManager 管理存储池、磁盘、chunk 的元数据和操作。
// 包含 DB 连接和一组 ChunkHandler（按 disk.Backend 匹配使用）。
type PoolManager struct {
	DbManager *db.DbManager
	Handlers  []ChunkHandler

	// Keys 是进程级的 Share 解锁密钥表（只在内存里，见 keyring.go）。
	// 加密 Share 要先用口令解锁，之后所有协议都能通过它拿到密钥。
	Keys *Keyring
}

// New 创建 PoolManager。
// handlers 是按 Backend 类型提供的 ChunkHandler 列表，用于读写数据。
func New(dbManager *db.DbManager, handlers []ChunkHandler) *PoolManager {
	return &PoolManager{
		DbManager: dbManager,
		Handlers:  handlers,
		Keys:      NewKeyring(),
	}
}

// checkPoolName 检查 pool 名称是否已存在。
func (p *PoolManager) checkPoolName(name string) error {
	var existingPool db.Pool
	if p.DbManager.DB.Where("name = ?", name).First(&existingPool).Error == nil && existingPool.Id > 0 {
		return errs.New(errs.ECODE_POOL_BAD_NAME, errs.ESTR_POOL_BAD_NAME, "duplicate pool name", name)
	}
	return nil
}

// GetPool 按 ID 查询存储池。
func (p *PoolManager) GetPool(id int64) (*db.Pool, error) {
	var existingPool db.Pool
	if p.DbManager.DB.Where("id = ?", id).First(&existingPool).Error != nil {
		return nil, errs.New(errs.ECODE_POOL_BAD, errs.ESTR_POOL_BAD, "invalid pool id", strconv.FormatInt(id, 10))
	}
	return &existingPool, nil
}

// PoolIdOfChunk 由 chunk 所属的 stripe 推导它所在的 pool。
func (p *PoolManager) PoolIdOfChunk(chunkId int64) (int64, error) {
	var chunk db.Chunk
	if err := p.DbManager.DB.First(&chunk, chunkId).Error; err != nil {
		return 0, errs.FromError(err, errs.ECODE_CHUNK_NOT_FOUND, errs.ESTR_CHUNK_NOT_FOUND)
	}
	return p.poolIdByStripeId(chunk.StripeId)
}

// poolIdByStripeId 由 stripe ID 推导所属 pool。
func (p *PoolManager) poolIdByStripeId(stripeId int64) (int64, error) {
	var stripe db.Stripe
	if err := p.DbManager.DB.First(&stripe, stripeId).Error; err != nil {
		return 0, errs.DBQuery(err)
	}
	return stripe.PoolId, nil
}

// AddPool 创建一个新的存储池。
// chunkSizeKb 范围 1~65536 KB（最大 64MB）。
func (p *PoolManager) AddPool(name string, chunkSizeKb int64) (*db.Pool, error) {
	if chunkSizeKb <= 0 || chunkSizeKb > 64*1024 {
		return nil, errs.New(errs.ECODE_POOL_BAD, errs.ESTR_POOL_BAD, "chunk size must be 1-65536 KB (max 64MB)", strconv.FormatInt(chunkSizeKb, 10))
	}

	if err := p.checkPoolName(name); err != nil {
		return nil, err
	}
	pool := db.Pool{
		Name:      name,
		ChunkSize: chunkSizeKb,
		Status:    db.DiskPoolStatus(db.Online),
	}

	result := p.DbManager.DB.Create(&pool)
	if result.Error != nil {
		return nil, errs.DBQuery(result.Error)
	}
	return &pool, nil
}

// AddDisk 向存储池添加一块磁盘。
// diskType 支持 DataDisk（参与条带化）和 CacheDisk（仅做读缓存）。
// addParity=true 时将新盘标记为 parity 盘并递增 ParityShards。
// CacheDisk 不参与条带化，直接创建并返回。
func (p *PoolManager) AddDisk(poolId int64, path string, diskBackend int8, diskType int8, addParity bool) (*db.Disk, error) {
	if diskBackend != int8(db.LocalBackend) {
		return nil, errs.New(errs.ECODE_DISK_BAD_BACKEND, errs.ESTR_DISK_BAD_BACKEND, "invalid disk backend", fmt.Sprintf("%d", diskBackend))
	}

	if diskType != int8(db.DataDisk) && diskType != int8(db.CacheDisk) {
		return nil, errs.New(errs.ECODE_DISK_BAD_TYPE, errs.ESTR_DISK_BAD_TYPE, "invalid disk type", fmt.Sprintf("%d", diskType))
	}

	var disk db.Disk

	// Cache 盘只需要一条记录，不参与条带化。
	// 但它要求池里**已经有数据盘**：缓存存的只是读副本，没有数据盘就没有东西可缓存，
	// 先加缓存盘只会得到一个永远用不上的盘。
	if diskType == int8(db.CacheDisk) {
		if _, err := p.GetPool(poolId); err != nil {
			return nil, err
		}
		var striped int64
		if err := p.DbManager.DB.Model(&db.Disk{}).
			Where("pool_id = ? AND type = ?", poolId, db.DataDisk).Count(&striped).Error; err != nil {
			return nil, errs.DBQuery(err)
		}
		if striped == 0 {
			return nil, errs.New(errs.ECODE_POOL_BAD, errs.ESTR_POOL_BAD,
				"add a data disk before adding a cache disk", fmt.Sprintf("%d", poolId))
		}
		disk = db.Disk{
			PoolId:  poolId,
			Path:    path,
			Backend: db.DiskBackend(diskBackend),
			Type:    db.DiskType(diskType),
			Status:  db.DiskPoolStatus(db.Online),
		}
		if err := p.DbManager.DB.Create(&disk).Error; err != nil {
			return nil, errs.DBQuery(err)
		}
		return &disk, nil
	}

	var chunkType db.ChunkType
	var idx int64 // new chunk's index (= old shard count)

	err := p.DbManager.DB.Transaction(func(tx *gorm.DB) error {
		var existingPool db.Pool
		if err1 := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("id = ?", poolId).First(&existingPool).Error; err1 != nil {
			return errs.DBQuery(err1)

		}
		if existingPool.Id == 0 {
			return errs.New(errs.ECODE_POOL_BAD_NAME, errs.ESTR_POOL_BAD_NAME, "invalid pool id", fmt.Sprintf("%d", poolId))
		}
		disk = db.Disk{
			PoolId:  poolId,
			Path:    path,
			Backend: db.DiskBackend(diskBackend),
			Type:    db.DiskType(diskType),
			Status:  db.DiskPoolStatus(db.Online),
		}
		if err1 := tx.Create(&disk).Error; err1 != nil {
			return errs.DBQuery(err1)
		}
		if addParity {
			// 池里必须先有数据分片：只加 parity 的话没有东西可保护（RS 的 data 分片数为 0）。
			if existingPool.DataShards == 0 {
				return errs.New(errs.ECODE_POOL_BAD, errs.ESTR_POOL_BAD,
					"add a data disk before adding parity", fmt.Sprintf("%d", poolId))
			}
			existingPool.ParityShards += 1
			chunkType = db.ParityChunk
			idx = existingPool.ParityShards - 1
		} else {
			existingPool.DataShards += 1
			chunkType = db.DataChunk
			idx = existingPool.DataShards - 1
		}
		if err1 := tx.Save(&existingPool).Error; err1 != nil {
			return errs.DBQuery(err1)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// 释放 pool 锁后，为已有 stripe 预分配 chunk slot（新盘对应位置）
	var stripes []db.Stripe
	if err := p.DbManager.DB.Where("pool_id = ?", poolId).Find(&stripes).Error; err != nil {
		return nil, errs.DBQuery(err)
	}
	if len(stripes) > 0 {
		allocs := make([]db.Chunk, len(stripes))
		paths, err2 := generateChunkPaths(len(stripes))
		if err2 != nil {
			return nil, errs.FromError(err2, errs.ECODE_CRYPTO_ERROR, errs.ESTR_CRYPTO_ERROR)
		}
		for i, s := range stripes {
			allocs[i] = db.Chunk{
				Status:   db.ChunkReserved,
				Path:     paths[i],
				DiskId:   disk.Id,
				StripeId: s.Id,
				PoolId:   poolId,
				Type:     chunkType,
				Index:    idx,
			}
		}
		if err := p.DbManager.DB.Create(&allocs).Error; err != nil {
			return nil, errs.DBQuery(err)
		}
	}
	return &disk, nil
}

// DeleteCacheDisk 删除一块缓存盘：连同盘上的缓存文件与 read_caches 记录一起清掉。
//
// 只允许删**缓存盘**：数据盘（含 parity 位）是唯一数据副本，删掉会丢数据，
// 那种情况应该走"下线 / 换盘 + 重建"的流程。
//
// 删除是安全的：读路径在缓存文件读不到时会自动回退到源盘（见 ReadChunks），
// 所以即使此刻正好有请求命中这块盘的缓存，也只是多一次回退。
// 返回从库里移除的缓存记录数。
func (p *PoolManager) DeleteCacheDisk(diskId int64) (int, error) {
	var disk db.Disk
	err := p.DbManager.DB.First(&disk, diskId).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, errs.New(errs.ECODE_DISK_NOT_FOUND, errs.ESTR_DISK_NOT_FOUND,
			"disk not found", strconv.FormatInt(diskId, 10))
	}
	if err != nil {
		return 0, errs.DBQuery(err)
	}
	if disk.Type != db.CacheDisk {
		return 0, errs.New(errs.ECODE_DISK_BAD_TYPE, errs.ESTR_DISK_BAD_TYPE,
			"only cache disks can be deleted", strconv.FormatInt(diskId, 10))
	}

	var entries []db.ReadCache
	if err := p.DbManager.DB.Where("disk_id = ?", diskId).Find(&entries).Error; err != nil {
		return 0, errs.DBQuery(err)
	}

	// 先删文件、再删记录：反过来就找不到文件了。删文件是尽力而为的（失败只记日志），
	// 残留文件不影响正确性——盘记录没了就不会再被引用。
	p.deleteCacheFiles(entries)

	if err := p.DbManager.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("disk_id = ?", diskId).Delete(&db.ReadCache{}).Error; err != nil {
			return errs.DBQuery(err)
		}
		if err := tx.Delete(&db.Disk{}, diskId).Error; err != nil {
			return errs.DBQuery(err)
		}
		return nil
	}); err != nil {
		return 0, err
	}
	return len(entries), nil
}

// OfflinePool 标记 pool 为 Offline，暂停所有读写操作。
func (p *PoolManager) OfflinePool(poolId int64) error {
	return p.DbManager.DB.Model(&db.Pool{}).
		Where("id = ?", poolId).Update("status", db.Offline).Error
}

// SwapDisk 替换硬盘路径：标记 disk 为 Repair、更新路径，
// 并把所属 pool 置为 Offline——换盘后条带数据不完整，池在重建完成前不应对外服务。
// 重建完成后由 recoverAfterRebuild 自动恢复 Online。
func (p *PoolManager) SwapDisk(diskId int64, newPath string) error {
	return p.DbManager.DB.Transaction(func(tx *gorm.DB) error {
		var disk db.Disk
		if err := tx.First(&disk, diskId).Error; err != nil {
			return errs.DBQuery(err)
		}
		if err := tx.Model(&db.Disk{}).Where("id = ?", diskId).
			Updates(map[string]interface{}{
				"status": db.Repair,
				"path":   newPath,
			}).Error; err != nil {
			return errs.DBQuery(err)
		}
		return tx.Model(&db.Pool{}).Where("id = ?", disk.PoolId).
			Update("status", db.Offline).Error
	})
}

// getDiskMapByPoolId 查出该池的全部磁盘，按磁盘 id 建索引（写文件时按 chunk 找盘用）。
func (p *PoolManager) getDiskMapByPoolId(poolId int64) (map[int64]db.Disk, error) {
	// 预加载盘信息（写文件用）
	var allDisks []db.Disk
	if err := p.DbManager.DB.Where("pool_id = ?", poolId).Find(&allDisks).Error; err != nil {
		return nil, errs.DBQuery(err)
	}
	diskById := make(map[int64]db.Disk, len(allDisks))
	for i := range allDisks {
		diskById[allDisks[i].Id] = allDisks[i]
	}
	return diskById, nil
}

// loadDisksByIds 用给定的连接/事务一次查出这批磁盘，按磁盘 id 建索引。
func loadDisksByIds(gdb *gorm.DB, ids []int64) (map[int64]db.Disk, error) {
	var disks []db.Disk
	if err := gdb.Where("id IN ?", ids).Find(&disks).Error; err != nil {
		return nil, errs.DBQuery(err)
	}
	diskById := make(map[int64]db.Disk, len(disks))
	for i := range disks {
		diskById[disks[i].Id] = disks[i]
	}
	return diskById, nil
}

// uniqueIds 去掉重复的 id，保持首次出现的顺序。
func uniqueIds(ids []int64) []int64 {
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
