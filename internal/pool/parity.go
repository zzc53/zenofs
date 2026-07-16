package pool

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/klauspost/reedsolomon"
	"github.com/zeebo/blake3"
	"github.com/zzc53/zenofs/internal/db"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// handlerFor 遍历已注册的 ChunkHandler 列表，返回匹配 disk.Backend 的第一个 handler。
func (p *PoolManager) handlerFor(backend db.DiskBackend) ChunkHandler {
	for _, h := range p.Handlers {
		if h.Type() == backend {
			return h
		}
	}
	return nil
}

// stripeResult 保存一次 RS 编码后单个 parity shard 的计算结果。
type stripeResult struct {
	parityIdx  int64    // parity shard 的序号
	parityData []byte   // parity shard 的原始数据
	parityHash [32]byte // parity shard 的 BLAKE3 哈希
}

// computeStripe 对一个 stripe 执行完整的 RS 编码流程。
//
// 流程：
//  1. 并发读取该 stripe 的所有 data chunk（从磁盘）
//  2. 将所有 data shard padding 到等长（RS 编码要求）
//  3. 调用 Reed-Solomon 编码生成 parity shard
//  4. 并发将 parity shard 写入磁盘
//  5. 计算每个 parity shard 的 BLAKE3 哈希返回
//
// 返回 ([]stripeResult, true) 表示成功，否则返回 (nil, false)。
func (p *PoolManager) computeStripe(stripeId int64, dataShards, parityShards int,
	dataChunks []db.Chunk, parityByIndex map[int64]db.Chunk, diskById map[int64]db.Disk) ([]stripeResult, bool) {

	// ---------------------------------------------------------------
	// 第一步：分配 shard 数组。shards[0..dataShards-1] 放 data，
	// shards[dataShards..] 放 parity。
	// ---------------------------------------------------------------
	shards := make([][]byte, dataShards+parityShards)

	// ---------------------------------------------------------------
	// 第二步：并发读取所有 data chunk 的数据。
	// 每个 goroutine 读取一个 chunk，通过 channel 收集结果。
	// ---------------------------------------------------------------
	type readRes struct {
		idx  int64
		data []byte
		err  error
	}
	readCh := make(chan readRes, len(dataChunks))
	var readWg sync.WaitGroup
	for _, c := range dataChunks {
		readWg.Add(1)
		go func(c db.Chunk) {
			defer readWg.Done()
			// 根据 chunk.DiskId 找到对应的物理磁盘
			disk, ok := diskById[c.DiskId]
			if !ok {
				readCh <- readRes{c.Index, nil, fmt.Errorf("disk %d not found", c.DiskId)}
				return
			}
			// 根据磁盘后端类型找到对应的读写 handler
			h := p.handlerFor(disk.Backend)
			if h == nil {
				readCh <- readRes{c.Index, nil, fmt.Errorf("no handler for backend %d", disk.Backend)}
				return
			}
			// 从磁盘完整读取 chunk 数据
			data, err := h.Read(disk, c.Path)
			readCh <- readRes{c.Index, data, err}
		}(c)
	}
	// 等待所有并发读取完成，关闭 channel
	readWg.Wait()
	close(readCh)

	// 收集读取结果，按 index 填入 shards 数组
	for r := range readCh {
		if r.err != nil {
			log.Printf("parity: stripe %d read data index %d failed: %v", stripeId, r.idx, r.err)
			return nil, false
		}
		shards[r.idx] = r.data
	}

	// ---------------------------------------------------------------
	// 第三步：将所有 data shard padding 到等长。
	// RS 编码要求输入的所有 shard 长度一致。找出最长的一个，
	// 将其余不足的 shard 用 0 padding 补足。
	// ---------------------------------------------------------------
	var maxSize int
	for _, s := range shards[:dataShards] {
		if len(s) > maxSize {
			maxSize = len(s)
		}
	}
	// 不足等长的 shard 拷贝到新的 padded 数组
	for i := 0; i < dataShards; i++ {
		if len(shards[i]) < maxSize {
			padded := make([]byte, maxSize)
			copy(padded, shards[i])
			shards[i] = padded
		}
	}
	// stripe 中未写入的 slot（比如 batch 不足 dataShards 个）用全零填充
	for i := 0; i < dataShards; i++ {
		if shards[i] == nil {
			shards[i] = make([]byte, maxSize)
		}
	}
	// 预分配 parity shard 的空间
	for i := dataShards; i < dataShards+parityShards; i++ {
		shards[i] = make([]byte, maxSize)
	}

	// ---------------------------------------------------------------
	// 第四步：初始化 Reed-Solomon 编码器，执行编码。
	// 编码完成后 shards[dataShards..] 中存放的是计算出的 parity 数据。
	// ---------------------------------------------------------------
	enc, err := reedsolomon.New(dataShards, parityShards)
	if err != nil {
		log.Printf("parity: new encoder failed: %v", err)
		return nil, false
	}
	if err := enc.Encode(shards); err != nil {
		log.Printf("parity: encode stripe %d failed: %v", stripeId, err)
		return nil, false
	}

	// ---------------------------------------------------------------
	// 第五步：并发将 parity shard 写入磁盘。
	// 每个 goroutine 写入一个 parity shard，通过 channel 收集结果。
	// ---------------------------------------------------------------
	type writeRes struct {
		idx int64
		err error
	}
	writeCh := make(chan writeRes, parityShards)
	var writeWg sync.WaitGroup
	for i := 0; i < parityShards; i++ {
		writeWg.Add(1)
		go func(idx int) {
			defer writeWg.Done()
			parityData := shards[dataShards+idx] // 取第 idx 个 parity shard 的数据
			// 根据 index 查找预分配的 parity chunk 元数据
			c, ok := parityByIndex[int64(idx)]
			if !ok {
				writeCh <- writeRes{int64(idx), fmt.Errorf("parity %d not found", idx)}
				return
			}
			// 找到 parity chunk 所在的磁盘
			disk, ok := diskById[c.DiskId]
			if !ok {
				writeCh <- writeRes{int64(idx), fmt.Errorf("disk %d not found", c.DiskId)}
				return
			}
			// 找到对应的 handler 并写入磁盘
			h := p.handlerFor(disk.Backend)
			if h == nil {
				writeCh <- writeRes{int64(idx), fmt.Errorf("no handler for backend %d", disk.Backend)}
				return
			}
			writeCh <- writeRes{int64(idx), h.Write(disk, c.Path, parityData)}
		}(i)
	}
	// 等待所有并发写入完成
	writeWg.Wait()
	close(writeCh)

	// ---------------------------------------------------------------
	// 第六步：收集写入结果，写入成功则计算 BLAKE3 哈希一并返回。
	// ---------------------------------------------------------------
	var results []stripeResult
	for r := range writeCh {
		if r.err != nil {
			log.Printf("parity: stripe %d write parity %d failed: %v", stripeId, r.idx, r.err)
			return nil, false
		}
		parityData := shards[dataShards+int(r.idx)]
		results = append(results, stripeResult{
			parityIdx:  r.idx,
			parityData: parityData,
			parityHash: blake3.Sum256(parityData),
		})
	}
	return results, true
}

// processWriteQueue 是 parity worker 的核心调度函数。
//
// 整体流程：
//  1. 清理上次意外残留的 QueueProcessing 条目
//  2. 在一个事务中原子地领走一批 QueuePending 条目（SKIP LOCKED 避免竞争）
//  3. 按 stripe 去重，加载 stripe/pool/disk/chunk 元数据
//  4. Phase 1: 锁定 parity chunk 并标记为 Pending（防止并发写入）
//  5. 并发调用 computeStripe 对每个 stripe 执行 RS 编码（最多 4 路并发）
//  6. Phase 2: 在一个事务中批量更新 data chunk→Active、parity chunk→Active、删除已处理的 WriteQueue
//
// 返回 true 表示处理了至少一个条目，false 表示空闲。
func (p *PoolManager) processWriteQueue() bool {
	// ---------------------------------------------------------------
	// Step 1: 清理上次异常中断残留的 QueueProcessing 条目。
	// 如果进程在上次批次中间崩溃，这些条目会永远卡在 Processing 状态。
	// 将它们重置为 Pending，让本次重新处理。
	// ---------------------------------------------------------------
	p.DbManager.DB.Model(&db.WriteQueue{}).
		Where("status = ?", db.QueueProcessing).Update("status", db.QueuePending)

	// ---------------------------------------------------------------
	// Step 2: 在一个事务中原子地领走一批 QueuePending 条目。
	// 使用 SKIP LOCKED 避免多个 parity worker（如果有）之间的锁竞争。
	// 领走后将状态改为 QueueProcessing，防止被其他 worker 重复领取。
	// ---------------------------------------------------------------
	var entries []db.WriteQueue
	err := p.DbManager.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status = ?", db.QueuePending).
			Find(&entries).Error; err != nil {
			return err
		}
		if len(entries) == 0 {
			return nil
		}
		ids := make([]int64, len(entries))
		for i, e := range entries {
			ids[i] = e.Id
		}
		return tx.Model(&db.WriteQueue{}).
			Where("id IN ?", ids).Update("status", db.QueueProcessing).Error
	})
	if err != nil {
		log.Printf("parity: claim pending entries failed: %v", err)
		return false
	}
	if len(entries) == 0 {
		return false
	}

	// ---------------------------------------------------------------
	// Step 3: 从 WriteQueue 条目中提取出所有涉及的 stripe ID（去重）。
	// ---------------------------------------------------------------
	stripeSet := make(map[int64]struct{})
	for _, e := range entries {
		stripeSet[e.StripeId] = struct{}{}
	}
	stripeIds := make([]int64, 0, len(stripeSet))
	for id := range stripeSet {
		stripeIds = append(stripeIds, id)
	}

	// ---------------------------------------------------------------
	// Step 4: 加载 stripe → pool → disk 的完整元数据链。
	//
	// 4a. 加载 stripe 列表，建立 stripeId → poolId 映射
	// ---------------------------------------------------------------
	var stripes []db.Stripe
	if err := p.DbManager.DB.Where("id IN ?", stripeIds).Find(&stripes).Error; err != nil {
		log.Printf("parity: query stripes failed: %v", err)
		return false
	}
	poolMap := make(map[int64]int64)
	poolSet := make(map[int64]struct{})
	for _, s := range stripes {
		poolMap[s.Id] = s.PoolId
		poolSet[s.PoolId] = struct{}{}
	}
	poolIds := make([]int64, 0, len(poolSet))
	for id := range poolSet {
		poolIds = append(poolIds, id)
	}

	// 4b. 加载 pool 配置（DataShards / ParityShards）
	var pools []db.Pool
	if err := p.DbManager.DB.Where("id IN ?", poolIds).Find(&pools).Error; err != nil {
		log.Printf("parity: query pools failed: %v", err)
		return false
	}
	poolConfig := make(map[int64]struct{ DataShards, ParityShards int64 })
	for _, pl := range pools {
		poolConfig[pl.Id] = struct{ DataShards, ParityShards int64 }{pl.DataShards, pl.ParityShards}
	}

	// 4c. 加载所有涉及的磁盘信息，建立 diskId → Disk 映射
	var disks []db.Disk
	if err := p.DbManager.DB.Where("pool_id IN ?", poolIds).Find(&disks).Error; err != nil {
		log.Printf("parity: query disks failed: %v", err)
		return false
	}
	diskById := make(map[int64]db.Disk, len(disks))
	for i := range disks {
		diskById[disks[i].Id] = disks[i]
	}

	// 4d. 一次性加载所有 stripe 的全部 chunk 元数据
	var allChunks []db.Chunk
	if err := p.DbManager.DB.Where("stripe_id IN ?", stripeIds).Find(&allChunks).Error; err != nil {
		log.Printf("parity: query chunks failed: %v", err)
		return false
	}

	// ---------------------------------------------------------------
	// Step 5: 按 stripe 组织 chunk 数据。
	// 每个 stripeJob 包含 dataChunks(待编码的 data chunk) 和
	// parityByIndex(parity chunk 按 index 索引)。
	// ---------------------------------------------------------------
	type stripeJob struct {
		dataChunks    []db.Chunk
		parityByIndex map[int64]db.Chunk
		skip          bool            // 标记该 stripe 是否应跳过
		ds, ps        int             // data shards / parity shards 数量
	}
	jobs := make(map[int64]*stripeJob, len(stripeIds))

	for _, c := range allChunks {
		j, ok := jobs[c.StripeId]
		if !ok {
			// 首次遇到该 stripe，创建 job 并从 poolConfig 读取 RS 参数
			pid := poolMap[c.StripeId]
			cfg := poolConfig[pid]
			j = &stripeJob{ds: int(cfg.DataShards), ps: int(cfg.ParityShards)}
			jobs[c.StripeId] = j
		}
		// 按 chunk 类型分别收集
		if c.Type == db.DataChunk {
			// data chunk 处于 Pending 或 Error 状态时，跳过整个 stripe
			if c.Status == db.ChunkPending || c.Status == db.ChunkError {
				log.Printf("parity: stripe %d data index %d status=%d, skip", c.StripeId, c.Index, c.Status)
				j.skip = true
				continue
			}
			// 排除预留的但未写入的 slot
			if c.Status != db.ChunkReserved {
				j.dataChunks = append(j.dataChunks, c)
			}
		} else if c.Type == db.ParityChunk {
			if j.parityByIndex == nil {
				j.parityByIndex = make(map[int64]db.Chunk)
			}
			j.parityByIndex[c.Index] = c
		}
	}

	// 对每个 stripe 的 data chunks 按 Index 排序（确保 RS 编码的顺序正确）
	for _, j := range jobs {
		if j.skip {
			continue
		}
		idxMap := make(map[int64]db.Chunk, len(j.dataChunks))
		for _, c := range j.dataChunks {
			idxMap[c.Index] = c
		}
		ordered := make([]db.Chunk, 0, j.ds)
		for i := int64(0); i < int64(j.ds); i++ {
			if c, ok := idxMap[i]; ok {
				ordered = append(ordered, c)
			}
		}
		j.dataChunks = ordered
	}

	// ---------------------------------------------------------------
	// Phase 1: 统一锁住所有待处理 stripe 的 parity chunk，标记为 Pending。
	//
	// 为什么锁全部而非逐条：
	//   - 防止两个批次同时处理同一个 stripe 的 parity
	//   - 如果某个 stripe 的 parity 已经是 Pending（来自上一批），跳过整个 stripe
	// ---------------------------------------------------------------
	err = p.DbManager.DB.Transaction(func(tx *gorm.DB) error {
		var allParity []db.Chunk
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("stripe_id IN ? AND type = ?", stripeIds, db.ParityChunk).
			Find(&allParity).Error; err != nil {
			return fmt.Errorf("lock parity: %w", err)
		}
		// 检查哪些 stripe 的 parity 已经是 Pending 状态
		pendingStripes := make(map[int64]bool)
		for _, c := range allParity {
			if c.Status == db.ChunkPending {
				pendingStripes[c.StripeId] = true
			}
		}
		// 标记这些 stripe 为跳过
		for sid := range pendingStripes {
			if j, ok := jobs[sid]; ok {
				j.skip = true
				log.Printf("parity: stripe %d parity pending, skip", sid)
			}
		}
		// 收集非跳过 stripe 的 parity id，统一更新为 Pending
		var toUpdate []int64
		for _, c := range allParity {
			if j, ok := jobs[c.StripeId]; ok && !j.skip {
				toUpdate = append(toUpdate, c.Id)
			}
		}
		if len(toUpdate) == 0 {
			return nil
		}
		return tx.Model(&db.Chunk{}).Where("id IN ?", toUpdate).Update("status", db.ChunkPending).Error
	})
	if err != nil {
		log.Printf("parity: phase1 failed: %v", err)
		return false
	}

	// ---------------------------------------------------------------
	// 并发计算 RS parity：对每个待处理的 stripe 启动一个 goroutine，
	// 最多同时运行 4 个（信号量控制），防止内存被大量 stripe 撑爆。
	// ---------------------------------------------------------------
	type jobResult struct {
		stripeId int64
		ok       bool
		results  []stripeResult
	}
	resultCh := make(chan jobResult, len(jobs))
	var computeWg sync.WaitGroup
	sem := make(chan struct{}, 4) // 并发上限 4

	for _, sid := range stripeIds {
		j, ok := jobs[sid]
		if !ok || j.skip || len(j.dataChunks) == 0 {
			continue
		}
		computeWg.Add(1)
		go func(sid int64, j *stripeJob) {
			defer computeWg.Done()
			sem <- struct{}{}       // 获取信号量
			defer func() { <-sem }() // 释放信号量
			res, ok := p.computeStripe(sid, j.ds, j.ps, j.dataChunks, j.parityByIndex, diskById)
			resultCh <- jobResult{stripeId: sid, ok: ok, results: res}
		}(sid, j)
	}
	computeWg.Wait()
	close(resultCh)

	// ---------------------------------------------------------------
	// 收集计算结果：
	//   - allDataIds: 成功计算 parity 的 data chunk ID 列表
	//   - allParityUpdates: parity chunk 的 ID / 大小 / 哈希
	// ---------------------------------------------------------------
	type parityUpdate struct {
		id   int64
		size int64
		hash []byte
	}
	var allDataIds []int64
	var allParityUpdates []parityUpdate

	for r := range resultCh {
		if !r.ok {
			// 计算失败的 stripe 标记为跳过，不更新元数据
			if j, ok := jobs[r.stripeId]; ok {
				j.skip = true
			}
			continue
		}
		j := jobs[r.stripeId]
		for _, c := range j.dataChunks {
			allDataIds = append(allDataIds, c.Id)
		}
		for _, sr := range r.results {
			c := j.parityByIndex[sr.parityIdx]
			allParityUpdates = append(allParityUpdates, parityUpdate{
				id: c.Id, size: int64(len(sr.parityData)), hash: sr.parityHash[:],
			})
		}
	}

	// ---------------------------------------------------------------
	// Phase 2: 在一个事务中批量更新数据库。
	//
	// 更新项：
	//   - data chunk 状态 → Active（parity 已就绪）
	//   - parity chunk 状态 → Active，并更新 size 和 checksum
	//   - 删除已处理的 WriteQueue 条目
	// ---------------------------------------------------------------
	allParityIds := make([]int64, len(allParityUpdates))
	for i, pu := range allParityUpdates {
		allParityIds[i] = pu.id
	}

	err = p.DbManager.DB.Transaction(func(tx *gorm.DB) error {
		// 将所有成功计算的 data chunk 标记为 Active
		if len(allDataIds) > 0 {
			if err := tx.Model(&db.Chunk{}).Where("id IN ?", allDataIds).
				Update("status", db.ChunkActive).Error; err != nil {
				return fmt.Errorf("update data: %w", err)
			}
		}
		// 加载 parity chunk 并更新大小和校验和
		if len(allParityIds) > 0 {
			var parity []db.Chunk
			if err := tx.Where("id IN ?", allParityIds).Find(&parity).Error; err != nil {
				return fmt.Errorf("load parity: %w", err)
			}
			puMap := make(map[int64]parityUpdate, len(allParityUpdates))
			for _, pu := range allParityUpdates {
				puMap[pu.id] = pu
			}
			for i := range parity {
				pu := puMap[parity[i].Id]
				parity[i].Status = db.ChunkActive
				parity[i].Size = pu.size
				parity[i].Checksum = pu.hash
			}
			if err := tx.Save(&parity).Error; err != nil {
				return fmt.Errorf("save parity: %w", err)
			}
		}
		// 删除已处理完毕的 WriteQueue 条目
		return tx.Where("status = ?", db.QueueProcessing).Delete(&db.WriteQueue{}).Error
	})
	if err != nil {
		log.Printf("parity: phase2 failed: %v", err)
		return false
	}

	log.Printf("parity: batch done (%d stripes, %d data, %d parity)",
		len(stripeIds), len(allDataIds), len(allParityUpdates))
	return true
}

// StartParityWorker 启动后台 goroutine，按上下文退避轮询 DB 处理 parity。
//
// 轮询策略：
//   - 初始间隔 1 秒
//   - 每次有任务处理后重置为 1 秒
//   - 连续空闲时退避到最大 5 秒（减少空轮询开销）
//
// ctx 取消时等待当前批次完成后退出 goroutine。
func (p *PoolManager) StartParityWorker(ctx context.Context) {
	go func() {
		interval := 1 * time.Second
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				// 收到退出信号，goroutine 立即返回
				return
			case <-ticker.C:
				// 执行一次 parity 处理
				hadWork := p.processWriteQueue()
				if hadWork {
					// 有任务处理，恢复为 1 秒间隔
					interval = 1 * time.Second
				} else {
					// 空闲，指数退避到最大 5 秒
					interval *= 2
					if interval > 5*time.Second {
						interval = 5 * time.Second
					}
				}
				ticker.Reset(interval)
			}
		}
	}()
	log.Printf("parity worker started (interval=1s, max_backoff=5s)")
}
