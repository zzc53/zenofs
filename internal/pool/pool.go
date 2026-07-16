package pool

import (
	"fmt"
	"strconv"

	"github.com/zzc53/zenofs/internal/errs"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/zzc53/zenofs/internal/db"
)

type PoolManager struct {
	DbManager *db.DbManager
	Handlers  []ChunkHandler
}

func New(dbManager *db.DbManager, handlers []ChunkHandler) *PoolManager {
	return &PoolManager{
		DbManager: dbManager,
		Handlers:  handlers,
	}
}

func (p *PoolManager) checkPoolName(name string) error {
	var existingPool db.Pool
	if p.DbManager.DB.Where("name = ?", name).First(&existingPool).Error == nil && existingPool.Id > 0 {
		return errs.New(errs.ECODE_POOL_BAD_NAME, errs.ESTR_POOL_BAD_NAME, "duplicate pool name", name)
	}
	return nil
}

func (p *PoolManager) GetPool(id int64) (*db.Pool, error) {
	var existingPool db.Pool
	if p.DbManager.DB.Where("id = ?", id).First(&existingPool).Error != nil {
		return nil, errs.New(errs.ECODE_POOL_BAD, errs.ESTR_POOL_BAD, "invalid pool id", strconv.FormatInt(id, 10))
	}
	return &existingPool, nil
}

func (p *PoolManager) AddPool(name string, chunkSizeKb int64) (*db.Pool, error) {
	if err := p.acquireLock("add_pool", "add pool", 0, 0); err != nil {
		return nil, err
	}
	defer p.releaseLock("add_pool")

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

func (p *PoolManager) AddDisk(poolId int64, path string, diskBackend int8, diskType int8, addParity bool) (*db.Disk, error) {
	if err := p.acquireLock("add_disk", "add disk", poolId, 0); err != nil {
		return nil, err
	}
	defer p.releaseLock("add_disk")

	if diskBackend != int8(db.LocalBackend) {
		return nil, errs.New(errs.ECODE_DISK_BAD_BACKEND, errs.ESTR_DISK_BAD_BACKEND, "invalid disk backend", fmt.Sprintf("%d", diskBackend))
	}

	if diskType != int8(db.DataDisk) {
		return nil, errs.New(errs.ECODE_DISK_BAD_TYPE, errs.ESTR_DISK_BAD_TYPE, "invalid disk type", fmt.Sprintf("%d", diskType))
	}

	var disk db.Disk
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
			existingPool.Status = db.Offline
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
		for i, s := range stripes {
			p, err2 := generateChunkPath()
			if err2 != nil {
				return nil, errs.FromError(err2, errs.ECODE_CRYPTO_ERROR, errs.ESTR_CRYPTO_ERROR)
			}
			allocs[i] = db.Chunk{
				Status:   db.ChunkReserved,
				Path:     p,
				DiskId:   disk.Id,
				StripeId: s.Id,
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

// acquireLock 创建新任务记录，全局只有一个任务能处于 Pending/Running。
func (p *PoolManager) acquireLock(name, desc string, poolId, diskId int64) error {
	return p.DbManager.DB.Transaction(func(tx *gorm.DB) error {
		// 锁住 ACTION_LOCK 行，跨数据库通用（行锁 / 表锁）
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("name = ?", "ACTION_LOCK").First(&db.Setting{}).Error; err != nil {
			return err
		}

		var count int64
		tx.Model(&db.Task{}).Where("status IN ?",
			[]db.TaskStatus{db.TaskPending, db.TaskRunning}).Count(&count)
		if count > 0 {
			var t db.Task
			tx.Where("status IN ?",
				[]db.TaskStatus{db.TaskPending, db.TaskRunning}).First(&t)
			msg := t.Message
			if msg == "" {
				msg = "unknown"
			}
			return errs.New(errs.ECODE_POOL_BAD, errs.ESTR_POOL_BAD, "another operation is running: "+msg, "")
		}
		return tx.Create(&db.Task{Name: name, Status: db.TaskRunning, Message: desc, PoolId: poolId, DiskId: diskId}).Error
	})
}

// releaseLock 将当前 Running 任务标记为 Finished。
func (p *PoolManager) releaseLock(name string) {
	p.DbManager.DB.Model(&db.Task{}).
		Where("name = ? AND status = ?", name, db.TaskRunning).
		Update("status", db.TaskFinished)
}

// OfflinePool 标记 pool 为 Offline，暂停所有读写操作。
func (p *PoolManager) OfflinePool(poolId int64) error {
	if err := p.acquireLock("offline_pool", "offline pool", poolId, 0); err != nil {
		return err
	}
	defer p.releaseLock("offline_pool")

	return p.DbManager.DB.Model(&db.Pool{}).
		Where("id = ?", poolId).Update("status", db.Offline).Error
}

// SwapDisk 替换硬盘路径：标记 disk 为 Repair，对应 chunk 标记 Error，更新路径。
func (p *PoolManager) SwapDisk(diskId int64, newPath string) error {
	if err := p.acquireLock("swap_disk", "swap disk", 0, diskId); err != nil {
		return err
	}
	defer p.releaseLock("swap_disk")

	return p.DbManager.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&db.Disk{}).Where("id = ?", diskId).
			Updates(map[string]interface{}{
				"status": db.Repair,
				"path":   newPath,
			}).Error; err != nil {
			return err
		}
		return tx.Model(&db.Chunk{}).Where("disk_id = ?", diskId).
			Update("status", db.ChunkError).Error
	})
}
