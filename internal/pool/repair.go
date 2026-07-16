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
//
// 整体流程：
//  1. 获取全局锁（防止和其他管理操作冲突）
//  2. 查出 pool 配置（RS 参数 DataShards / ParityShards）
//  3. 批量加载 pool 下所有的 chunk 元数据
//  4. 按 stripe 分组，筛选出含有 Error chunk 的 stripe
//  5. 加载 pool 下所有磁盘信息
//  6. Phase 1: 锁定 Error chunk 并标记为 Pending
//  7. Phase 2: 并发调用 recoverStripe 对每个损坏的 stripe 执行 RS 重建（最多 4 路并发）
//  8. Phase 3: 批量更新数据库，将已恢复的 chunk 标记为 Active
//  9. 将 pool 和 Repair 状态的磁盘恢复为 Online
func (p *PoolManager) ReconstructStripes(poolId int64) {
	// ---------------------------------------------------------------
	// Step 1: 获取全局锁，同一时间只能有一个重建任务运行。
	// ---------------------------------------------------------------
	if err := p.acquireLock("reconstruct_stripes", "reconstruct stripes", poolId, 0, ""); err != nil {
		log.Printf("reconstruct: %v", err)
		return
	}
	defer p.releaseLock("reconstruct_stripes", "")

	// ---------------------------------------------------------------
	// Step 2: 读取 pool 配置，获取 RS 参数（data shards / parity shards）。
	// ---------------------------------------------------------------
	var pool db.Pool
	if err := p.DbManager.DB.Where("id = ?", poolId).First(&pool).Error; err != nil {
		log.Printf("reconstruct: pool %d not found: %v", poolId, err)
		return
	}
	ds, ps := int(pool.DataShards), int(pool.ParityShards)

	// ---------------------------------------------------------------
	// Step 3: 批量加载 pool 下所有 chunk 数据。
	// 通过 JOIN stripes 表过滤出属于该 pool 的所有 chunk。
	// ---------------------------------------------------------------
	var allChunks []db.Chunk
	if err := p.DbManager.DB.Joins("JOIN stripes ON chunks.stripe_id = stripes.id").
		Where("stripes.pool_id = ?", poolId).Find(&allChunks).Error; err != nil {
		log.Printf("reconstruct: query chunks failed: %v", err)
		return
	}

	// ---------------------------------------------------------------
	// Step 4: 按 stripe ID 分组，构建 stripeMap。
	// 然后筛选出含有至少一个 Error chunk 的 stripe 进行重建。
	// ---------------------------------------------------------------
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

	// 遍历所有 stripe，挑出那些包含 Error chunk 的
	var stripes []*stripeInfo
	for _, si := range stripeMap {
		for _, c := range si.chunks {
			if c.Status == db.ChunkError {
				stripes = append(stripes, si)
				break
			}
		}
	}

	// 如果没有损坏的 stripe，直接恢复 online 状态并返回
	if len(stripes) == 0 {
		p.bringOnline(poolId)
		return
	}

	// ---------------------------------------------------------------
	// Step 5: 加载 pool 下所有磁盘信息，建立 diskId → Disk 映射。
	// ---------------------------------------------------------------
	var disks []db.Disk
	if err := p.DbManager.DB.Where("pool_id = ?", poolId).Find(&disks).Error; err != nil {
		log.Printf("reconstruct: query disks failed: %v", err)
		return
	}
	diskById := make(map[int64]db.Disk, len(disks))
	for i := range disks {
		diskById[disks[i].Id] = disks[i]
	}

	// ---------------------------------------------------------------
	// Phase 1: 锁住所有 Error chunk 并标记为 Pending。
	//
	// 使用行锁（Strength: "UPDATE"）锁定这些 chunk，防止在重建过程中
	// 被其他操作写入。锁定后统一更新为 Pending 状态。
	// ---------------------------------------------------------------
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

	// ---------------------------------------------------------------
	// Phase 2: 并发对每个损坏的 stripe 执行 RS 重建。
	// 每个 stripe 启动一个 goroutine，最多同时运行 4 个（信号量控制）。
	// recoverStripe 返回需要更新到 Active 的 chunk 列表。
	// ---------------------------------------------------------------
	type recoverResult struct {
		stripeIdx int
		ok        bool
		updates   []db.Chunk // 需要更新到 Active 的 chunk
	}
	resultCh := make(chan recoverResult, len(stripes))
	var computeWg sync.WaitGroup
	sem := make(chan struct{}, 4) // 重建并发上限 4

	for idx, si := range stripes {
		computeWg.Add(1)
		go func(idx int, si *stripeInfo) {
			defer computeWg.Done()
			sem <- struct{}{}       // 获取信号量
			defer func() { <-sem }() // 释放信号量
			updates := p.recoverStripe(si.chunks, ds, ps, diskById)
			resultCh <- recoverResult{stripeIdx: idx, ok: updates != nil, updates: updates}
		}(idx, si)
	}
	computeWg.Wait()
	close(resultCh)

	// ---------------------------------------------------------------
	// Phase 3: 批量收集所有重建成功的 chunk，一次性写入数据库。
	// 失败的 stripe 仅记录日志，不阻塞其他 stripe 的重建。
	// ---------------------------------------------------------------
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

	// ---------------------------------------------------------------
	// Step 9: 重建完毕，恢复 pool 和 Repair 状态磁盘为 Online。
	// ---------------------------------------------------------------
	p.bringOnline(poolId)
	log.Printf("reconstruct: pool %d done (%d stripes)", poolId, len(stripes))
}

// recoverStripe 对一个 stripe 执行 RS 重建。
//
// 流程：
//  1. 读取所有存活 shard（data + parity），通过 checksum 验证数据完整性
//  2. 缺失或校验失败的 shard 留为空（nil）
//  3. 将所有 shard padding 到等长
//  4. 如果存活 shard 数量 >= dataShards，调用 RS Reconstruct 重建
//  5. 对重建成功的 chunk：写回磁盘，计算并更新 checksum
//  6. 对存活的 chunk：直接标记 Active
//
// 返回需要更新到 Active 的 chunk 列表（包含存活的 + 重建后的）。
func (p *PoolManager) recoverStripe(chunks []db.Chunk, dataShards, parityShards int,
	diskById map[int64]db.Disk) []db.Chunk {

	if dataShards == 0 {
		return nil
	}

	// ---------------------------------------------------------------
	// 第一步：初始化 shard 数组。
	// shards[pos] 存放 shard 数据，shardOk[pos] 标记该 shard 是否正常。
	// pos = chunk.Index (data) 或 dataShards + chunk.Index (parity)。
	// ---------------------------------------------------------------
	shards := make([][]byte, dataShards+parityShards)
	shardOk := make([]bool, dataShards+parityShards)

	// 遍历该 stripe 的所有 chunk，逐个尝试读取
	for _, c := range chunks {
		// 计算 shard 在数组中的位置：data 按 Index，parity 偏移 dataShards
		pos := int(c.Index)
		if c.Type == db.ParityChunk {
			pos += dataShards
		}

		// 找到对应的磁盘和 handler
		disk, ok := diskById[c.DiskId]
		if !ok {
			continue
		}
		h := p.handlerFor(disk.Backend)
		if h == nil {
			continue
		}

		// 从磁盘读取完整数据
		data, err := h.Read(disk, c.Path)
		if err != nil {
			continue
		}

		// 校验数据大小是否匹配元数据
		if int64(len(data)) != c.Size {
			continue
		}

		// 如果 chunk 有 checksum，验证数据完整性
		if len(c.Checksum) > 0 {
			hash := blake3.Sum256(data)
			if string(hash[:]) != string(c.Checksum) {
				// checksum 不匹配，说明数据已损坏，跳过
				continue
			}
		}

		// 数据校验通过，填入 shard 数组
		shards[pos] = data
		shardOk[pos] = true
	}

	// ---------------------------------------------------------------
	// 第二步：将所有 shard（包括缺失的）padding 到等长。
	// 缺失的 shard 用全零填充（RS Reconstruct 会修复它们）。
	// ---------------------------------------------------------------
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

	// ---------------------------------------------------------------
	// 第三步：检查存活 shard 数量是否足够重建。
	// RS 要求可用 shard 数 >= dataShards 才能恢复所有数据。
	// ---------------------------------------------------------------
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

	// ---------------------------------------------------------------
	// 第四步：执行 RS Reconstruct。
	// 该方法使用存活 shard 中的数据/parity 推算缺失 shard 的原始数据。
	// ---------------------------------------------------------------
	enc, err := reedsolomon.New(dataShards, parityShards)
	if err != nil {
		log.Printf("reconstruct: new encoder: %v", err)
		return nil
	}
	if err := enc.Reconstruct(shards); err != nil {
		log.Printf("reconstruct: failed: %v", err)
		return nil
	}

	// ---------------------------------------------------------------
	// 第五步：写回重建的数据，收集需要更新的 chunk。
	//
	// 对存活 shard：直接标记 Active（磁盘文件完好）
	// 对重建 shard：写回磁盘并更新 size 和 checksum
	// ---------------------------------------------------------------
	var updates []db.Chunk
	for _, c := range chunks {
		pos := int(c.Index)
		if c.Type == db.ParityChunk {
			pos += dataShards
		}

		if shardOk[pos] {
			// 存活 shard：仅标记状态为 Active
			c.Status = db.ChunkActive
			updates = append(updates, c)
		} else {
			// 重建 shard：写回磁盘
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
			// 更新元数据
			c.Status = db.ChunkActive
			c.Size = int64(len(recovered))
			hash := blake3.Sum256(recovered)
			c.Checksum = hash[:]
			updates = append(updates, c)
		}
	}
	return updates
}

// bringOnline 将 pool 及其下所有 Repair 状态的磁盘恢复为 Online。
// 通常在重建完成后调用，恢复系统的正常读写能力。
func (p *PoolManager) bringOnline(poolId int64) {
	// 将 pool 下所有 Repair 状态的磁盘恢复为 Online
	p.DbManager.DB.Model(&db.Disk{}).
		Where("pool_id = ? AND status = ?", poolId, db.Repair).
		Update("status", db.Online)
	// 将 pool 本身恢复为 Online
	p.DbManager.DB.Model(&db.Pool{}).
		Where("id = ?", poolId).Update("status", db.Online)
}
