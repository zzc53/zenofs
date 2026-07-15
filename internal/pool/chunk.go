package pool

import (
	"fmt"
	"os"
	"path"
	"strconv"
	"time"

	"crypto/rand"

	"github.com/zeebo/blake3"
	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func generateSecureRandomString(length int) (string, error) {
	const charset = "abcdefghijklmnopqrstuvwxyz23456789"
	b := make([]byte, length)

	_, err := rand.Read(b)
	if err != nil {
		return "", err
	}

	for i := range b {
		b[i] = charset[int(b[i])%len(charset)]
	}
	return string(b), nil
}
func (p *PoolManager) AllocateChunk(stripeId int64, disks []db.Disk, chunks []db.Chunk, size int, hash []byte) (*db.Chunk, error) {
	var maxDataIndex int64 = -1
	var selectedDisk *db.Disk
	for _, disk := range disks {
		chunkFoundInDisk := false
		for _, chunk := range chunks {
			if disk.Id == chunk.DiskId {
				chunkFoundInDisk = true
				break
			}
		}
		if chunkFoundInDisk {
			continue
		}
		selectedDisk = &disk
		break
	}

	if selectedDisk == nil {
		return nil, errs.New(errs.ECODE_STRIPE_FULL, errs.ESTR_STRIPE_FULL, "stripe full", strconv.FormatInt(stripeId, 10))
	}

	for _, chunk := range chunks {
		if chunk.Type == db.DataChunk && chunk.Index > maxDataIndex {
			maxDataIndex = chunk.Index
		}
	}

	dateStr := fmt.Sprintf(time.Now().Format("2006/01/02/15/04"))
	fileSuffix, err := generateSecureRandomString(8)
	if err != nil {
		return nil, errs.FromError(err, errs.ECODE_CRYPTO_ERROR, errs.ESTR_CRYPTO_ERROR)
	}
	fileName := path.Join(selectedDisk.Path, dateStr, fileSuffix)
	return &db.Chunk{
		Path:     fileName,
		DiskId:   selectedDisk.Id,
		StripeId: stripeId,
		Size:     int64(size),
		Checksum: hash,
		Type:     db.DataChunk,
		Index:    maxDataIndex + 1,
	}, nil

}

func (p *PoolManager) AddChunk(poolId int64, bytes []byte) (*db.Chunk, error) {
	pool, err := p.checkPoolId(poolId)
	if err != nil {
		return nil, err
	}
	if pool.Status != db.Online {
		return nil, errs.New(errs.ECODE_POOL_OFFLINE, errs.ESTR_POOL_OFFLINE, "pool is offline", strconv.FormatInt(poolId, 10))
	}
	disks, err := p.getPoolOnlineDataDisks(p.DbManager.DB, poolId)
	if err != nil {
		return nil, err
	}
	if len(disks) == 0 {
		return nil, errs.New(errs.ECODE_DISK_OFFLINE, errs.ESTR_DISK_OFFLINE, "pool does not have online disk", strconv.FormatInt(poolId, 10))
	}
	size := len(bytes)
	hash := blake3.Sum256(bytes)
	var chunk db.Chunk
	if err := p.DbManager.DB.Where("size = ? AND checksum = ?", size, hash).First(&chunk).Error; err == nil {
		return &chunk, nil
	}
	err = p.DbManager.DB.Transaction(func(tx *gorm.DB) error {
		var stripe db.Stripe
		if err1 := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("pool_id = ? AND data_shards < ?", poolId, pool.DataShards).Order("data_shards ASC").First(&stripe).Error; err1 != nil {
			stripe = db.Stripe{
				PoolId:       pool.Id,
				DataShards:   1,
				ParityShards: 0,
			}
			if err1 := tx.Create(&stripe).Error; err1 != nil {
				return errs.FromError(err1, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
			}
			newChunk, err1 := p.AllocateChunk(stripe.Id, disks, []db.Chunk{}, size, hash[:])
			if err1 != nil {
				return err1
			}
			if newChunk == nil {
				return errs.New(errs.ECODE_CHUNK_ALLOC, errs.ESTR_CHUNK_ALLOC, "chunk not allocated", strconv.FormatInt(stripe.Id, 0))
			}
			chunk = *newChunk
			if err1 := tx.Create(&chunk).Error; err1 != nil {
				return errs.FromError(err1, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
			}
		} else {
			var stripeChunks []db.Chunk
			if err1 := tx.Where("stripe_id = ?", stripe.Id).Find(&stripeChunks).Error; err1 != nil {
				return errs.FromError(err1, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
			}
			newChunk, err1 := p.AllocateChunk(stripe.Id, disks, stripeChunks, size, hash[:])
			if err1 != nil {
				return err1
			}
			if newChunk == nil {
				return errs.New(errs.ECODE_CHUNK_ALLOC, errs.ESTR_CHUNK_ALLOC, "chunk not allocated", strconv.FormatInt(stripe.Id, 0))
			}
			chunk = *newChunk
			if err1 := tx.Create(&chunk).Error; err1 != nil {
				return errs.FromError(err1, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
			}
			stripe.DataShards += 1
			if err1 := tx.Save(&stripe).Error; err1 != nil {
				return errs.FromError(err1, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(chunk.Path, bytes, 0644); err != nil {
		return nil, errs.FromError(err, errs.ECODE_FILE_WRITE, errs.ESTR_FILE_WRITE)
	}
	writeQueue := db.WriteQueue{
		ChunkId:  chunk.Id,
		StripeId: chunk.StripeId,
	}
	if err1 := p.DbManager.DB.Create(&writeQueue).Error; err1 != nil {
		return nil, errs.FromError(err1, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
	}
	return &chunk, err
}
