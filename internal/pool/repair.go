package pool

import (
	"log"
	"sync"

	"github.com/klauspost/reedsolomon"
	"github.com/zeebo/blake3"
	"github.com/zzc53/zenofs/internal/db"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ReconstructStripes 重建 pool 中所有含 ChunkError 的 stripe。
// 批量 DB 读 → 并行文件计算 → 批量 DB 写。
func (p *PoolManager) ReconstructStripes(poolId int64) {
	if err := p.acquireLock("reconstruct_stripes", "reconstruct stripes", poolId, 0); err != nil {
		log.Printf("reconstruct: %v", err)
		return
	}
	defer p.releaseLock("reconstruct_stripes")

	// 查出 pool 配置（确定 RS 参数）
	var pool db.Pool
	if err := p.DbManager.DB.Where("id = ?", poolId).First(&pool).Error; err != nil {
		log.Printf("reconstruct: pool %d not found: %v", poolId, err)
		return
	}
	ds, ps := int(pool.DataShards), int(pool.ParityShards)

	// ── 批量加载 ──
	var allChunks []db.Chunk
	if err := p.DbManager.DB.Joins("JOIN stripes ON chunks.stripe_id = stripes.id").
		Where("stripes.pool_id = ?", poolId).Find(&allChunks).Error; err != nil {
		log.Printf("reconstruct: query chunks failed: %v", err)
		return
	}

	// 按 stripe 组织
	type stripeInfo struct {
		chunks []db.Chunk
	}
	stripeMap := make(map[int64]*stripeInfo)
	for i := range allChunks {
		c := &allChunks[i]
		si, ok := stripeMap[c.StripeId]
		if !ok {
			si = &stripeInfo{}
			stripeMap[c.StripeId] = si
		}
		si.chunks = append(si.chunks, *c)
	}

	// 分离出需要重建的 stripe
	var stripes []*stripeInfo
	for _, si := range stripeMap {
		for _, c := range si.chunks {
			if c.Status == db.ChunkError {
				stripes = append(stripes, si)
				break
			}
		}
	}

	if len(stripes) == 0 {
		p.bringOnline(poolId)
		return
	}

	var disks []db.Disk
	if err := p.DbManager.DB.Where("pool_id = ?", poolId).Find(&disks).Error; err != nil {
		log.Printf("reconstruct: query disks failed: %v", err)
		return
	}
	diskById := make(map[int64]db.Disk, len(disks))
	for i := range disks {
		diskById[disks[i].Id] = disks[i]
	}

	// ── Phase 1: 锁 Error chunk 并标记 Pending ──
	var errorIds []int64
	for _, si := range stripes {
		for _, c := range si.chunks {
			if c.Status == db.ChunkError {
				errorIds = append(errorIds, c.Id)
			}
		}
	}
	if err := p.DbManager.DB.Transaction(func(tx *gorm.DB) error {
		var locked []db.Chunk
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id IN ?", errorIds).Find(&locked).Error; err != nil {
			return err
		}
		return tx.Model(&db.Chunk{}).
			Where("id IN ?", errorIds).Update("status", db.ChunkPending).Error
	}); err != nil {
		log.Printf("reconstruct: phase1 lock failed: %v", err)
		return
	}

	// ── Phase 2: 并行文件计算 ──
	type recoverResult struct {
		stripeIdx int
		ok        bool
		updates   []db.Chunk // chunks to update to Active
	}
	resultCh := make(chan recoverResult, len(stripes))
	var computeWg sync.WaitGroup
	sem := make(chan struct{}, 4)

	for idx, si := range stripes {
		computeWg.Add(1)
		go func(idx int, si *stripeInfo) {
			defer computeWg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			updates := p.recoverStripe(si.chunks, ds, ps, diskById)
			resultCh <- recoverResult{stripeIdx: idx, ok: updates != nil, updates: updates}
		}(idx, si)
	}
	computeWg.Wait()
	close(resultCh)

	// ── Phase 3: 批量 DB 写 ──
	var allUpdates []db.Chunk
	for r := range resultCh {
		if !r.ok {
			log.Printf("reconstruct: stripe %d failed, skipped", stripes[r.stripeIdx].chunks[0].StripeId)
			continue
		}
		allUpdates = append(allUpdates, r.updates...)
	}

	if len(allUpdates) > 0 {
		if err := p.DbManager.DB.Transaction(func(tx *gorm.DB) error {
			return tx.Save(&allUpdates).Error
		}); err != nil {
			log.Printf("reconstruct: batch save failed: %v", err)
			return
		}
	}

	p.bringOnline(poolId)
	log.Printf("reconstruct: pool %d done (%d stripes)", poolId, len(stripes))
}

// recoverStripe 纯文件计算：读取存活 shard → RS 重建 → 写回文件。
// dataShards/parityShards 为 pool 固定值，未写入 slot 以全零填充。
func (p *PoolManager) recoverStripe(chunks []db.Chunk, dataShards, parityShards int,
	diskById map[int64]db.Disk) []db.Chunk {

	if dataShards == 0 {
		return nil
	}

	shards := make([][]byte, dataShards+parityShards)
	shardOk := make([]bool, dataShards+parityShards)

	// chunk.Index 决定 data 位置，parity 偏移 dataShards
	for _, c := range chunks {
		pos := int(c.Index)
		if c.Type == db.ParityChunk {
			pos += dataShards
		}

		disk, ok := diskById[c.DiskId]
		if !ok {
			continue
		}
		h := p.handlerFor(disk.Backend)
		if h == nil {
			continue
		}

		data, err := h.Read(disk, c.Path)
		if err != nil {
			continue
		}
		if int64(len(data)) != c.Size {
			continue
		}
		if len(c.Checksum) > 0 {
			hash := blake3.Sum256(data)
			if string(hash[:]) != string(c.Checksum) {
				continue
			}
		}
		shards[pos] = data
		shardOk[pos] = true
	}

	// 补齐所有 shard 到等长（parity chunk 已经是最长 data 的大小）
	var maxSize int
	for _, s := range shards {
		if len(s) > maxSize {
			maxSize = len(s)
		}
	}
	for i := range shards {
		if shards[i] == nil {
			shards[i] = make([]byte, maxSize)
		} else if len(shards[i]) < maxSize {
			padded := make([]byte, maxSize)
			copy(padded, shards[i])
			shards[i] = padded
		}
	}

	var validCount int
	for _, ok := range shardOk {
		if ok {
			validCount++
		}
	}
	if validCount < dataShards {
		log.Printf("reconstruct: stripe only %d/%d shards available", validCount, dataShards)
		return nil
	}

	enc, err := reedsolomon.New(dataShards, parityShards)
	if err != nil {
		log.Printf("reconstruct: new encoder: %v", err)
		return nil
	}
	if err := enc.Reconstruct(shards); err != nil {
		log.Printf("reconstruct: failed: %v", err)
		return nil
	}

	// 写回重建的文件，收集更新
	var updates []db.Chunk
	for _, c := range chunks {
		pos := int(c.Index)
		if c.Type == db.ParityChunk {
			pos += dataShards
		}

		if shardOk[pos] {
			c.Status = db.ChunkActive
			updates = append(updates, c)
		} else {
			recovered := shards[pos]
			if recovered == nil {
				continue
			}
			disk, ok := diskById[c.DiskId]
			if !ok {
				continue
			}
			h := p.handlerFor(disk.Backend)
			if h == nil {
				continue
			}
			if err := h.Write(disk, c.Path, recovered); err != nil {
				log.Printf("reconstruct: write chunk %d failed: %v", c.Id, err)
				continue
			}
			c.Status = db.ChunkActive
			c.Size = int64(len(recovered))
			hash := blake3.Sum256(recovered)
			c.Checksum = hash[:]
			updates = append(updates, c)
		}
	}
	return updates
}

func (p *PoolManager) bringOnline(poolId int64) {
	p.DbManager.DB.Model(&db.Disk{}).
		Where("pool_id = ? AND status = ?", poolId, db.Repair).
		Update("status", db.Online)
	p.DbManager.DB.Model(&db.Pool{}).
		Where("id = ?", poolId).Update("status", db.Online)
}
