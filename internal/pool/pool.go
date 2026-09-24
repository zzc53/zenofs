package pool

import (
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
}

// New 创建 PoolManager。
// handlers 是按 Backend 类型提供的 ChunkHandler 列表，用于读写数据。
func New(dbManager *db.DbManager, handlers []ChunkHandler) *PoolManager {
	return &PoolManager{
		DbManager: dbManager,
		Handlers:  handlers,
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
		return nil, errs.FromError(result.Error, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
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

	// Cache 盘不需要 stripe 预分配，直接创建并返回
	if diskType == int8(db.CacheDisk) {
		disk = db.Disk{
			PoolId:  poolId,
			Path:    path,
			Backend: db.DiskBackend(diskBackend),
			Type:    db.DiskType(diskType),
			Status:  db.DiskPoolStatus(db.Online),
		}
		if err := p.DbManager.DB.Create(&disk).Error; err != nil {
			return nil, errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
		}
		return &disk, nil
	}

	var chunkType db.ChunkType
	var idx int64 // new chunk's index (= old shard count)

	err := p.DbManager.DB.Transaction(func(tx *gorm.DB) error {
		var existingPool db.Pool
		if err1 := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("id = ?", poolId).First(&existingPool).Error; err1 != nil {
			return errs.FromError(err1, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)

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
			return errs.FromError(err1, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
		}
		if addParity {
			existingPool.ParityShards += 1
			chunkType = db.ParityChunk
			idx = existingPool.ParityShards - 1
		} else {
			existingPool.DataShards += 1
			chunkType = db.DataChunk
			idx = existingPool.DataShards - 1
		}
		if err1 := tx.Save(&existingPool).Error; err1 != nil {
			return errs.FromError(err1, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// 释放 pool 锁后，为已有 stripe 预分配 chunk slot（新盘对应位置）
	var stripes []db.Stripe
	if err := p.DbManager.DB.Where("pool_id = ?", poolId).Find(&stripes).Error; err != nil {
		return nil, errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
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
			return nil, errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
		}
	}
	return &disk, nil
}

// OfflinePool 标记 pool 为 Offline，暂停所有读写操作。
func (p *PoolManager) OfflinePool(poolId int64) error {
	return p.DbManager.DB.Model(&db.Pool{}).
		Where("id = ?", poolId).Update("status", db.Offline).Error
}

// SwapDisk 替换硬盘路径：标记 disk 为 Repair，对应 chunk 标记 Error，更新路径。
func (p *PoolManager) SwapDisk(diskId int64, newPath string) error {
	return p.DbManager.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&db.Disk{}).Where("id = ?", diskId).
			Updates(map[string]interface{}{
				"status": db.Repair,
				"path":   newPath,
			}).Error; err != nil {
			return err
		}
		return nil
	})
}

func (p *PoolManager) getDiskMapByPoolId(poolId int64) (map[int64]db.Disk, error) {
	// 预加载盘信息（写文件用）
	var allDisks []db.Disk
	if err := p.DbManager.DB.Where("pool_id = ?", poolId).Find(&allDisks).Error; err != nil {
		return nil, errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
	}
	diskById := make(map[int64]db.Disk, len(allDisks))
	for i := range allDisks {
		diskById[allDisks[i].Id] = allDisks[i]
	}
	return diskById, nil
}
