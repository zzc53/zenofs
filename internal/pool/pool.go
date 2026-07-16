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
	DbManager  *db.DbManager
	WriteQueue chan []db.WriteQueue
	Writers    []ChunkWriter
}

func New(dbManager *db.DbManager, writeQueue chan []db.WriteQueue, writers []ChunkWriter) *PoolManager {
	return &PoolManager{
		DbManager:  dbManager,
		WriteQueue: writeQueue,
		Writers:    writers,
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
	if diskBackend != int8(db.LocalBackend) {
		return nil, errs.New(errs.ECODE_DISK_BAD_BACKEND, errs.ESTR_DISK_BAD_BACKEND, "invalid disk backend", fmt.Sprintf("%d", diskBackend))
	}

	if diskType != int8(db.DataDisk) {
		return nil, errs.New(errs.ECODE_DISK_BAD_TYPE, errs.ESTR_DISK_BAD_TYPE, "invalid disk type", fmt.Sprintf("%d", diskType))
	}

	var disk db.Disk
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
		} else {
			existingPool.DataShards += 1
		}
		if err1 := tx.Save(&existingPool).Error; err1 != nil {
			return errs.FromError(err1, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
		}
		return nil
	})
	return &disk, err
}
