package pool

import (
	"fmt"
	"path"
	"strconv"
	"sync"
	"time"

	"crypto/rand"

	mrand "math/rand"

	"github.com/zeebo/blake3"
	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func generateSecureRandomString(length int) (string, error) {
	const charset = "abcdefghijklmnopqrstuvwxyz234567"
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

// AddChunk 兼容包装：单 chunk 写入。
func (p *PoolManager) AddChunk(poolId int64, bytes []byte) (*db.Chunk, error) {
	chunks, err := p.AddChunks(poolId, [][]byte{bytes})
	if err != nil {
		return nil, err
	}
	return &chunks[0], nil
}

// AddChunks 批量写入 chunks，支持任意数量（1 ≤ N）。
//
// 分配策略：
//  1. Phase 1 — 消费预分配的 Reserved chunks（只锁 chunk 行，不锁 stripe）
//  2. Phase 2 — 不够则建新 stripe，data chunk 创建时直接写入数据（新行，无需锁）
//  3. Phase 3 — 激活 old Reserved chunks（补 size/checksum/path）
//
// 锁竞争：唯一可能碰撞的地方是 Phase 1 中多个事务抢同一条 Reserved chunk，
// SKIP LOCKED 让它们各自跳过已锁的行，天然并行。
func (p *PoolManager) AddChunks(poolId int64, dataList [][]byte) ([]db.Chunk, error) {
	if len(dataList) == 0 {
		return nil, errs.New(errs.ECODE_CHUNK_EMPTY, errs.ESTR_CHUNK_EMPTY, "empty data list", "")
	}
	for i, data := range dataList {
		if len(data) == 0 {
			return nil, errs.New(errs.ECODE_CHUNK_EMPTY, errs.ESTR_CHUNK_EMPTY, "empty data", strconv.Itoa(i))
		}
	}

	// 校验 pool
	pool, err := p.GetPool(poolId)
	if err != nil {
		return nil, err
	}
	if pool.Status != db.Online {
		return nil, errs.New(errs.ECODE_POOL_OFFLINE, errs.ESTR_POOL_OFFLINE, "pool is offline", strconv.FormatInt(poolId, 10))
	}

	N := len(dataList)

	// 预计算每个 chunk 的 hash 和 size
	type item struct {
		data []byte
		hash [32]byte
		size int
	}
	items := make([]item, N)
	for i, data := range dataList {
		items[i] = item{
			data: data,
			hash: blake3.Sum256(data),
			size: len(data),
		}
	}

	// 校验每个 chunk 大小不超过 pool 设定（ChunkSize 单位为 KB）
	maxSize := pool.ChunkSize * 1024
	for _, it := range items {
		if int64(it.size) > maxSize {
			return nil, errs.New(errs.ECODE_CHUNK_SIZE_EXCEED, errs.ESTR_CHUNK_SIZE_EXCEED,
				"chunk exceeds pool chunk size", strconv.FormatInt(maxSize, 10))
		}
	}

	// 预加载盘信息，事务外写文件时要用（通过 disk.Backend 匹配 Writer）
	var allDisks []db.Disk
	if err := p.DbManager.DB.Where("pool_id = ?", poolId).Find(&allDisks).Error; err != nil {
		return nil, errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
	}
	diskById := make(map[int64]db.Disk, len(allDisks))
	for i := range allDisks {
		diskById[allDisks[i].Id] = allDisks[i]
	}

	var chunks []db.Chunk

	err = p.DbManager.DB.Transaction(func(tx *gorm.DB) error {
		// Phase 1: 消费预分配的 Reserved chunks（JOIN 形式查 stripe）
		var reserved []db.Chunk
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Model(&db.Chunk{}).
			Select("chunks.*").
			Joins("JOIN stripes ON chunks.stripe_id = stripes.id").
			Where("chunks.status = ? AND stripes.pool_id = ?", db.ChunkReserved, poolId).
			Limit(N).
			Find(&reserved).Error; err != nil {
			return errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
		}

		need := N - len(reserved)

		// 查询 pool 所有 online data 盘（Phase 2 建 stripe 用）
		var disks []db.Disk
		if err := tx.Where("pool_id = ? AND status = ? AND type = ?",
			poolId, db.Online, db.DataDisk).Find(&disks).Error; err != nil {
			return errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
		}

		// Phase 2: 建新 stripe，data chunk 创建时直接写入数据（无需 Phase 3 二次 save）
		var newChunks []db.Chunk
		if need > 0 {
			dataShards := int(pool.DataShards)
			totalSlots := dataShards + int(pool.ParityShards)
			if totalSlots <= 0 {
				return errs.New(errs.ECODE_POOL_BAD, errs.ESTR_POOL_BAD, "invalid stripe capacity",
					fmt.Sprintf("data_shards=%d, parity_shards=%d", pool.DataShards, pool.ParityShards))
			}
			if len(disks) != totalSlots {
				return errs.New(errs.ECODE_DISK_OFFLINE, errs.ESTR_DISK_OFFLINE,
					"not all disks online for stripe allocation",
					fmt.Sprintf("online=%d, need=%d", len(disks), totalSlots))
			}

			remaining := need
			globalNewIdx := 0
			for remaining > 0 {
				batchSize := dataShards
				if remaining < dataShards {
					batchSize = remaining
				}

				stripe := db.Stripe{PoolId: poolId}
				if err := tx.Create(&stripe).Error; err != nil {
					return errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
				}

				shuffled := append([]db.Disk{}, disks...)
				mrand.New(mrand.NewSource(stripe.Id)).Shuffle(len(shuffled), func(i, j int) {
					shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
				})

				allChunks := make([]db.Chunk, totalSlots)
				for i := 0; i < totalSlots; i++ {
					typ := db.ParityChunk
					if i < dataShards {
						typ = db.DataChunk
					}
					status := db.ChunkReserved
					var size int64
					var checksum []byte
					if i < batchSize {
						status = db.ChunkPending
						itemIdx := len(reserved) + globalNewIdx + i
						size = int64(items[itemIdx].size)
						checksum = items[itemIdx].hash[:]
					}
					p, err := generateChunkPath()
					if err != nil {
						return errs.FromError(err, errs.ECODE_CRYPTO_ERROR, errs.ESTR_CRYPTO_ERROR)
					}
					allChunks[i] = db.Chunk{
						Status:   status,
						Path:     p,
						DiskId:   shuffled[i].Id,
						StripeId: stripe.Id,
						Size:     size,
						Checksum: checksum,
						Type:     typ,
						Index:    int64(i),
					}
				}
				if err := tx.Create(&allChunks).Error; err != nil {
					return errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
				}

				for i := 0; i < batchSize; i++ {
					newChunks = append(newChunks, allChunks[i])
				}

				remaining -= batchSize
				globalNewIdx += batchSize
			}
		}

		// Phase 3: 激活 old reserved chunks（new chunks 创建时已写完整）
		for i := range reserved {
			reserved[i].Status = db.ChunkPending
			reserved[i].Size = int64(items[i].size)
			reserved[i].Checksum = items[i].hash[:]
			if reserved[i].Path == "" {
				p, err := generateChunkPath()
				if err != nil {
					return errs.FromError(err, errs.ECODE_CRYPTO_ERROR, errs.ESTR_CRYPTO_ERROR)
				}
				reserved[i].Path = p
			}
			if err := tx.Save(&reserved[i]).Error; err != nil {
				return errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
			}
		}

		// 组装结果：reserved 在前，new 在后，保持 dataList 顺序
		chunks = make([]db.Chunk, 0, N)
		chunks = append(chunks, reserved...)
		chunks = append(chunks, newChunks...)
		return nil
	})
	if err != nil {
		return nil, err
	}

	// 事务外写文件（并行写入多盘），按 disk.Backend 匹配 Writer
	type writeResult struct {
		idx int
		err error
	}
	resultCh := make(chan writeResult, len(chunks))
	var wg sync.WaitGroup
	for i, c := range chunks {
		wg.Add(1)
		go func(idx int, disk db.Disk, relPath string, data []byte) {
			defer wg.Done()
			for _, w := range p.Writers {
				if w.WriterType() == disk.Backend {
					if err := w.Write(disk, relPath, data); err != nil {
						resultCh <- writeResult{idx, errs.FromError(err, errs.ECODE_FILE_WRITE, errs.ESTR_FILE_WRITE)}
					}
					return
				}
			}
			resultCh <- writeResult{idx, errs.New(errs.ECODE_FILE_WRITE, errs.ESTR_FILE_WRITE,
				"no writer found for backend", strconv.Itoa(int(disk.Backend)))}
		}(i, diskById[c.DiskId], c.Path, items[i].data)
	}
	wg.Wait()
	close(resultCh)

	var firstErr error
	var failedIdx []int
	for r := range resultCh {
		if r.err != nil {
			failedIdx = append(failedIdx, r.idx)
			if firstErr == nil {
				firstErr = r.err
			}
		}
	}
	if firstErr != nil {
		// 批量更新：写失败的标记 Error，写成功的标记 Updated
		var errorIds, successIds []int64
		failed := make(map[int]bool, len(failedIdx))
		for _, idx := range failedIdx {
			failed[idx] = true
		}
		for i, c := range chunks {
			if failed[i] {
				errorIds = append(errorIds, c.Id)
			} else {
				successIds = append(successIds, c.Id)
			}
		}
		if len(errorIds) > 0 {
			p.DbManager.DB.Model(&db.Chunk{}).Where("id IN ?", errorIds).Update("status", db.ChunkError)
		}
		if len(successIds) > 0 {
			p.DbManager.DB.Model(&db.Chunk{}).Where("id IN ?", successIds).Update("status", db.ChunkUpdated)
		}
		return nil, firstErr
	}

	// 批量写 WriteQueue（即 parity 队列）
	writeEntries := make([]db.WriteQueue, len(chunks))
	for i, c := range chunks {
		writeEntries[i] = db.WriteQueue{
			ChunkId:  c.Id,
			StripeId: c.StripeId,
		}
	}
	if err := p.DbManager.DB.Create(&writeEntries).Error; err != nil {
		return nil, errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
	}
	p.WriteQueue <- writeEntries

	return chunks, nil
}

func generateChunkPath() (string, error) {
	dateStr := time.Now().Format("2006/01/02/15/04")
	suffix, err := generateSecureRandomString(8)
	if err != nil {
		return "", err
	}
	return path.Join(dateStr, suffix), nil
}
