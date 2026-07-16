package pool

import (
	"encoding/binary"
	"fmt"
	"path"
	"strconv"
	"sync"
	"time"

	cryptorand "crypto/rand"

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

	_, err := cryptorand.Read(b)
	if err != nil {
		return "", err
	}

	for i := range b {
		b[i] = charset[int(b[i])%len(charset)]
	}
	return string(b), nil
}

// cryptoRandSeed 用 crypto/rand 生成一个 int64 随机种子。
func cryptoRandSeed() int64 {
	var buf [8]byte
	cryptorand.Read(buf[:])
	return int64(binary.LittleEndian.Uint64(buf[:]))
}

// AddChunk 兼容包装：单 chunk 写入。
func (p *PoolManager) AddChunk(poolId int64, bytes []byte) (*db.Chunk, error) {
	chunks, err := p.AddChunks(poolId, [][]byte{bytes})
	if err != nil {
		return nil, err
	}
	return &chunks[0], nil
}

const maxChunkBytes = 64 * 1024 * 1024 // 64MB safety guard

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

	// 校验每个 chunk 大小不超过 pool 的 chunk size
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
			Where("chunks.status = ? AND chunks.type = ? AND stripes.pool_id = ?", db.ChunkReserved, db.DataChunk, poolId).
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
				mrand.New(mrand.NewSource(cryptoRandSeed())).Shuffle(len(shuffled), func(i, j int) {
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
					idx := int64(i)
					if i >= dataShards {
						idx = int64(i - dataShards) // parity 独立编号
					}
					allChunks[i] = db.Chunk{
						Status:   status,
						Path:     p,
						DiskId:   shuffled[i].Id,
						StripeId: stripe.Id,
						Size:     size,
						Checksum: checksum,
						Type:     typ,
						Index:    idx,
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
			h := p.handlerFor(disk.Backend)
			if h == nil {
				resultCh <- writeResult{idx, errs.New(errs.ECODE_FILE_WRITE, errs.ESTR_FILE_WRITE,
					"no handler for backend", strconv.Itoa(int(disk.Backend)))}
				return
			}
			if err := h.Write(disk, relPath, data); err != nil {
				resultCh <- writeResult{idx, errs.FromError(err, errs.ECODE_FILE_WRITE, errs.ESTR_FILE_WRITE)}
			}
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
		failed := make(map[int]bool, len(failedIdx))
		for _, idx := range failedIdx {
			failed[idx] = true
		}
		var errorIds, successIds []int64
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
			p.DbManager.DB.Model(&db.Chunk{}).Where("id IN ?", successIds).Update("status", db.ChunkDirty)
			var wq []db.WriteQueue
			for i, c := range chunks {
				if !failed[i] {
					wq = append(wq, db.WriteQueue{ChunkId: c.Id, StripeId: c.StripeId})
				}
			}
			p.DbManager.DB.Create(&wq)
		}
		return nil, firstErr
	}

	// 全部写入成功
	ids := make([]int64, len(chunks))
	for i, c := range chunks {
		ids[i] = c.Id
	}
	p.DbManager.DB.Model(&db.Chunk{}).Where("id IN ?", ids).Update("status", db.ChunkDirty)

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

// WriteChunkItem 指定待写入的 chunk ID 和对应的数据。
type WriteChunkItem struct {
	ChunkId int64
	Data    []byte
}

// WriteChunks 向已分配的 chunk（通过 ChunkId）写入数据。
// poolId 从 chunk 所属 stripe 自动推导，不跨 pool。
func (p *PoolManager) WriteChunks(items []WriteChunkItem) ([]db.Chunk, error) {
	if len(items) == 0 {
		return nil, errs.New(errs.ECODE_CHUNK_EMPTY, errs.ESTR_CHUNK_EMPTY, "empty items", "")
	}

	N := len(items)

	// 预计算 hash 和 size
	type prepared struct {
		chunkId int64
		data    []byte
		hash    [32]byte
		size    int
	}
	prep := make([]prepared, N)
	for i, it := range items {
		if len(it.Data) == 0 {
			return nil, errs.New(errs.ECODE_CHUNK_EMPTY, errs.ESTR_CHUNK_EMPTY, "empty data", strconv.Itoa(i))
		}
		prep[i] = prepared{
			chunkId: it.ChunkId,
			data:    it.Data,
			hash:    blake3.Sum256(it.Data),
			size:    len(it.Data),
		}
	}

	// 一次查出所有 chunk 及所属 pool
	chunkIds := make([]int64, N)
	for i, it := range prep {
		chunkIds[i] = it.chunkId
	}
	var chunks []db.Chunk
	if err := p.DbManager.DB.Joins("JOIN stripes ON chunks.stripe_id = stripes.id").
		Where("chunks.id IN ?", chunkIds).Find(&chunks).Error; err != nil {
		return nil, errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
	}
	if len(chunks) != N {
		return nil, errs.New(errs.ECODE_CHUNK_NOT_FOUND, errs.ESTR_CHUNK_NOT_FOUND,
			"some chunks not found", "")
	}

	// 推导 poolId，校验 pool 在线
	poolId := int64(0)
	{
		var stripe db.Stripe
		if err := p.DbManager.DB.First(&stripe, chunks[0].StripeId).Error; err != nil {
			return nil, errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
		}
		poolId = stripe.PoolId
	}
	pool, err := p.GetPool(poolId)
	if err != nil {
		return nil, err
	}
	if pool.Status != db.Online {
		return nil, errs.New(errs.ECODE_POOL_OFFLINE, errs.ESTR_POOL_OFFLINE, "pool is offline", strconv.FormatInt(poolId, 10))
	}

	maxSize := pool.ChunkSize * 1024
	for _, it := range prep {
		if int64(it.size) > maxSize {
			return nil, errs.New(errs.ECODE_CHUNK_SIZE_EXCEED, errs.ESTR_CHUNK_SIZE_EXCEED,
				"data exceeds pool chunk size", strconv.FormatInt(maxSize, 10))
		}
	}

	// 按 chunk ID 建索引保持顺序
	chunkById := make(map[int64]*db.Chunk, N)
	for i := range chunks {
		chunkById[chunks[i].Id] = &chunks[i]
	}
	ordered := make([]db.Chunk, N)
	for i, it := range prep {
		ordered[i] = *chunkById[it.chunkId]
	}

	// 预加载盘信息（写文件用）
	var allDisks []db.Disk
	if err := p.DbManager.DB.Where("pool_id = ?", poolId).Find(&allDisks).Error; err != nil {
		return nil, errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
	}
	diskById := make(map[int64]db.Disk, len(allDisks))
	for i := range allDisks {
		diskById[allDisks[i].Id] = allDisks[i]
	}

	// 事务内更新 chunk 元数据
	err = p.DbManager.DB.Transaction(func(tx *gorm.DB) error {
		for i := range ordered {
			ordered[i].Status = db.ChunkPending
			ordered[i].Size = int64(prep[i].size)
			ordered[i].Checksum = prep[i].hash[:]
			if ordered[i].Path == "" {
				p, err := generateChunkPath()
				if err != nil {
					return errs.FromError(err, errs.ECODE_CRYPTO_ERROR, errs.ESTR_CRYPTO_ERROR)
				}
				ordered[i].Path = p
			}
		}
		return tx.Save(&ordered).Error
	})
	if err != nil {
		return nil, err
	}

	// 事务外并行写文件
	type writeResult struct {
		idx int
		err error
	}
	resultCh := make(chan writeResult, N)
	var wg sync.WaitGroup
	for i := range ordered {
		wg.Add(1)
		go func(idx int, disk db.Disk, relPath string, data []byte) {
			defer wg.Done()
			h := p.handlerFor(disk.Backend)
			if h == nil {
				resultCh <- writeResult{idx, errs.New(errs.ECODE_FILE_WRITE, errs.ESTR_FILE_WRITE,
					"no handler for backend", strconv.Itoa(int(disk.Backend)))}
				return
			}
			if err := h.Write(disk, relPath, data); err != nil {
				resultCh <- writeResult{idx, errs.FromError(err, errs.ECODE_FILE_WRITE, errs.ESTR_FILE_WRITE)}
			}
		}(i, diskById[ordered[i].DiskId], ordered[i].Path, prep[i].data)
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
		var errorIds, successIds []int64
		failed := make(map[int]bool, len(failedIdx))
		for _, idx := range failedIdx {
			failed[idx] = true
		}
		for i := range ordered {
			if failed[i] {
				errorIds = append(errorIds, ordered[i].Id)
			} else {
				successIds = append(successIds, ordered[i].Id)
			}
		}
		if len(errorIds) > 0 {
			p.DbManager.DB.Model(&db.Chunk{}).Where("id IN ?", errorIds).Update("status", db.ChunkError)
		}
		if len(successIds) > 0 {
			p.DbManager.DB.Model(&db.Chunk{}).Where("id IN ?", successIds).Update("status", db.ChunkDirty)
			var wq []db.WriteQueue
			for i, c := range ordered {
				if !failed[i] {
					wq = append(wq, db.WriteQueue{ChunkId: c.Id, StripeId: c.StripeId})
				}
			}
			p.DbManager.DB.Create(&wq)
		}
		return nil, firstErr
	}

	// 全部写入成功，标记 Updated
	allIds := make([]int64, N)
	for i := range ordered {
		allIds[i] = ordered[i].Id
	}
	p.DbManager.DB.Model(&db.Chunk{}).Where("id IN ?", allIds).Update("status", db.ChunkDirty)

	writeEntries := make([]db.WriteQueue, N)
	for i := range ordered {
		writeEntries[i] = db.WriteQueue{
			ChunkId:  ordered[i].Id,
			StripeId: ordered[i].StripeId,
		}
	}
	if err := p.DbManager.DB.Create(&writeEntries).Error; err != nil {
		return nil, errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
	}

	return ordered, nil
}

// ReadChunks 批量读取 chunk 数据，返回顺序与 chunkIds 一致。
func (p *PoolManager) ReadChunks(poolId int64, chunkIds []int64) ([][]byte, error) {
	if len(chunkIds) == 0 {
		return nil, errs.New(errs.ECODE_CHUNK_EMPTY, errs.ESTR_CHUNK_EMPTY, "empty chunk ids", "")
	}

	// 校验 pool
	pool, err := p.GetPool(poolId)
	if err != nil {
		return nil, err
	}
	if pool.Status != db.Online {
		return nil, errs.New(errs.ECODE_POOL_OFFLINE, errs.ESTR_POOL_OFFLINE, "pool is offline", strconv.FormatInt(poolId, 10))
	}

	N := len(chunkIds)

	// 一次查出所有 chunk，验证属于该 pool
	var chunks []db.Chunk
	if err := p.DbManager.DB.Joins("JOIN stripes ON chunks.stripe_id = stripes.id").
		Where("chunks.id IN ? AND stripes.pool_id = ?", chunkIds, poolId).
		Find(&chunks).Error; err != nil {
		return nil, errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
	}
	if len(chunks) != N {
		return nil, errs.New(errs.ECODE_CHUNK_NOT_FOUND, errs.ESTR_CHUNK_NOT_FOUND,
			"some chunks not found or not in pool", strconv.FormatInt(poolId, 10))
	}

	// 按 chunk ID 建索引保持顺序
	chunkById := make(map[int64]db.Chunk, N)
	for _, c := range chunks {
		chunkById[c.Id] = c
	}
	ordered := make([]db.Chunk, N)
	for i, id := range chunkIds {
		ordered[i] = chunkById[id]
	}

	// 预加载盘信息
	var allDisks []db.Disk
	if err := p.DbManager.DB.Where("pool_id = ?", poolId).Find(&allDisks).Error; err != nil {
		return nil, errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
	}
	diskById := make(map[int64]db.Disk, len(allDisks))
	for i := range allDisks {
		diskById[allDisks[i].Id] = allDisks[i]
	}

	// 并行读取（优先走缓存）
	type readResult struct {
		idx      int
		data     []byte
		err      error
		fromCache bool
	}
	resultCh := make(chan readResult, N)
	var wg sync.WaitGroup
	for i, c := range ordered {
		wg.Add(1)
		go func(idx int, chk db.Chunk) {
			defer wg.Done()
			// 尝试从缓存读取
			if data, _ := p.tryReadCache(chk.Id); data != nil {
				resultCh <- readResult{idx: idx, data: data, err: nil, fromCache: true}
				return
			}
			disk := diskById[chk.DiskId]
			h := p.handlerFor(disk.Backend)
			if h == nil {
				resultCh <- readResult{idx, nil, errs.New(errs.ECODE_FILE_WRITE, errs.ESTR_FILE_WRITE,
					"no handler for backend", strconv.Itoa(int(disk.Backend))), false}
				return
			}
			data, err := h.Read(disk, chk.Path)
			if err != nil {
				resultCh <- readResult{idx, nil, errs.FromError(err, errs.ECODE_FILE_WRITE, errs.ESTR_FILE_WRITE), false}
			} else {
				// 异步缓存
				go p.writeCache(poolId, chk.Id, data)
				resultCh <- readResult{idx, data, nil, false}
			}
		}(i, c)
	}
	wg.Wait()
	close(resultCh)

	results := make([][]byte, N)
	for r := range resultCh {
		if r.err != nil {
			return nil, r.err
		}
		results[r.idx] = r.data
	}
	return results, nil
}

// ReadChunkPartial 从 chunk 的指定偏移读取指定长度的数据。
func (p *PoolManager) ReadChunkPartial(chunkId int64, offset int64, length int64) ([]byte, error) {
	// 尝试从全量缓存读取
	if cached, _ := p.tryReadCache(chunkId); cached != nil {
		end := offset + length
		if end > int64(len(cached)) {
			end = int64(len(cached))
		}
		if offset >= end {
			return nil, errs.New(errs.ECODE_CHUNK_EMPTY, errs.ESTR_CHUNK_EMPTY, "offset beyond data", strconv.FormatInt(offset, 10))
		}
		return cached[offset:end], nil
	}

	var chunk db.Chunk
	if err := p.DbManager.DB.First(&chunk, chunkId).Error; err != nil {
		return nil, errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
	}

	// 推导 poolId 并校验 pool 在线
	var stripe db.Stripe
	if err := p.DbManager.DB.First(&stripe, chunk.StripeId).Error; err != nil {
		return nil, errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
	}
	poolObj, err := p.GetPool(stripe.PoolId)
	if err != nil {
		return nil, err
	}
	if poolObj.Status != db.Online {
		return nil, errs.New(errs.ECODE_POOL_OFFLINE, errs.ESTR_POOL_OFFLINE, "pool is offline", strconv.FormatInt(stripe.PoolId, 10))
	}

	var disk db.Disk
	if err := p.DbManager.DB.First(&disk, chunk.DiskId).Error; err != nil {
		return nil, errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
	}

	h := p.handlerFor(disk.Backend)
	if h == nil {
		return nil, errs.New(errs.ECODE_FILE_WRITE, errs.ESTR_FILE_WRITE, "no handler for backend", strconv.Itoa(int(disk.Backend)))
	}

	data, err := h.ReadAt(disk, chunk.Path, offset, length)
	if err != nil {
		return nil, errs.FromError(err, errs.ECODE_FILE_WRITE, errs.ESTR_FILE_WRITE)
	}

	// 异步缓存全量数据
	go func() {
		full, err := h.Read(disk, chunk.Path)
		if err == nil {
			p.writeCache(stripe.PoolId, chunkId, full)
		}
	}()

	return data, nil
}

// WriteChunkPartial 从指定偏移写入数据到 chunk。
// 写入后标记 chunk 为 Dirty，由 parity worker 异步更新 parity。
func (p *PoolManager) WriteChunkPartial(chunkId int64, offset int64, data []byte) error {
	if len(data) == 0 {
		return errs.New(errs.ECODE_CHUNK_EMPTY, errs.ESTR_CHUNK_EMPTY, "empty data", "")
	}
	if int64(len(data)) > maxChunkBytes {
		return errs.New(errs.ECODE_CHUNK_SIZE_EXCEED, errs.ESTR_CHUNK_SIZE_EXCEED,
			"write exceeds max chunk size", strconv.FormatInt(maxChunkBytes, 10))
	}

	var chunk db.Chunk
	if err := p.DbManager.DB.First(&chunk, chunkId).Error; err != nil {
		return errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
	}

	// 推导 poolId 并校验 pool 在线
	var stripe db.Stripe
	if err := p.DbManager.DB.First(&stripe, chunk.StripeId).Error; err != nil {
		return errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
	}
	pool, err := p.GetPool(stripe.PoolId)
	if err != nil {
		return err
	}
	if pool.Status != db.Online {
		return errs.New(errs.ECODE_POOL_OFFLINE, errs.ESTR_POOL_OFFLINE, "pool is offline", strconv.FormatInt(stripe.PoolId, 10))
	}

	var disk db.Disk
	if err := p.DbManager.DB.First(&disk, chunk.DiskId).Error; err != nil {
		return errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
	}

	h := p.handlerFor(disk.Backend)
	if h == nil {
		return errs.New(errs.ECODE_FILE_WRITE, errs.ESTR_FILE_WRITE, "no handler for backend", strconv.Itoa(int(disk.Backend)))
	}

	if err := h.WriteAt(disk, chunk.Path, offset, data); err != nil {
		return errs.FromError(err, errs.ECODE_FILE_WRITE, errs.ESTR_FILE_WRITE)
	}

	// 重新读取完整数据计算 checksum
	fullData, err := h.Read(disk, chunk.Path)
	if err != nil {
		return errs.FromError(err, errs.ECODE_FILE_WRITE, errs.ESTR_FILE_WRITE)
	}
	newSize := int64(len(fullData))
	hash := blake3.Sum256(fullData)

	// 更新元数据：size、checksum、状态、WriteQueue
	if err := p.DbManager.DB.Model(&db.Chunk{}).Where("id = ?", chunkId).Updates(map[string]interface{}{
		"status":   db.ChunkDirty,
		"size":     newSize,
		"checksum": hash[:],
	}).Error; err != nil {
		return errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
	}

	// 入队 WriteQueue
	wq := db.WriteQueue{
		ChunkId:  chunkId,
		StripeId: chunk.StripeId,
	}
	if err := p.DbManager.DB.Create(&wq).Error; err != nil {
		return errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
	}

	return nil
}
