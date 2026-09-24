package pool

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/klauspost/reedsolomon"
	"github.com/zeebo/blake3"
	"github.com/zzc53/zenofs/internal/db"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// maxParityConcurrency 限制单批 parity 计算同时运行的 stripe 数，
// 防止一次拉入过多 stripe 的 shard 数据把内存撑爆。
const maxParityConcurrency = 4

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
	// 第二步：并发读取所有 data chunk 的数据，按 index 填入 shards 数组。
	// ---------------------------------------------------------------
	readErrs := parallelEach(len(dataChunks), 0, func(i int) error {
		c := dataChunks[i]
		// 根据 chunk.DiskId 找到对应的物理磁盘
		disk, ok := diskById[c.DiskId]
		if !ok {
			return fmt.Errorf("index %d: disk %d not found", c.Index, c.DiskId)
		}
		// 根据磁盘后端类型找到对应的读写 handler
		h := p.handlerFor(disk.Backend)
		if h == nil {
			return fmt.Errorf("index %d: no handler for backend %d", c.Index, disk.Backend)
		}
		// 从磁盘完整读取 chunk 数据
		data, err := h.Read(disk, c.Path)
		if err != nil {
			return fmt.Errorf("index %d: %w", c.Index, err)
		}
		shards[c.Index] = data
		return nil
	})
	if err := firstErr(readErrs); err != nil {
		log.Printf("parity: stripe %d read data failed: %v", stripeId, err)
		return nil, false
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
	// 第五步：并发将 parity shard 写入磁盘，并计算各 shard 的 BLAKE3 哈希。
	// ---------------------------------------------------------------
	results := make([]stripeResult, parityShards)
	writeErrs := parallelEach(parityShards, 0, func(idx int) error {
		parityData := shards[dataShards+idx] // 取第 idx 个 parity shard 的数据
		// 根据 index 查找预分配的 parity chunk 元数据
		c, ok := parityByIndex[int64(idx)]
		if !ok {
			return fmt.Errorf("parity %d not found", idx)
		}
		// 找到 parity chunk 所在的磁盘
		disk, ok := diskById[c.DiskId]
		if !ok {
			return fmt.Errorf("parity %d: disk %d not found", idx, c.DiskId)
		}
		// 找到对应的 handler 并写入磁盘
		h := p.handlerFor(disk.Backend)
		if h == nil {
			return fmt.Errorf("parity %d: no handler for backend %d", idx, disk.Backend)
		}
		if err := h.Write(disk, c.Path, parityData); err != nil {
			return fmt.Errorf("parity %d: %w", idx, err)
		}
		// 写入成功，计算该 shard 的 BLAKE3 哈希一并返回
		results[idx] = stripeResult{
			parityIdx:  int64(idx),
			parityData: parityData,
			parityHash: blake3.Sum256(parityData),
		}
		return nil
	})
	if err := firstErr(writeErrs); err != nil {
		log.Printf("parity: stripe %d write parity failed: %v", stripeId, err)
		return nil, false
	}
	return results, true
}

// processStripeQueue 是 parity worker 的核心调度函数。
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
func (p *PoolManager) processStripeQueue() bool {
	// ---------------------------------------------------------------
	// Step 1: 清理上次异常中断残留的 Running 条目。
	// 如果进程在上次批次中间崩溃，这些条目会永远卡在 Running 状态。
	// 将它们重置为 Success，让本次重新处理。
	// ---------------------------------------------------------------
	p.DbManager.DB.Model(&db.WriteQueue{}).
		Where("status = ?", db.TaskRunning).Update("status", db.TaskSuccess)

	// ---------------------------------------------------------------
	// Step 2: 在一个事务中原子地领走一批 Success 条目
	// （Success 表示对应 chunk 数据已成功写入磁盘，可以计算 parity）。
	// 使用 SKIP LOCKED 避免多个 parity worker（如果有）之间的锁竞争。
	// 领走后将状态改为 TaskRunning，防止被其他 worker 重复领取。
	// ---------------------------------------------------------------
	var entries []db.WriteQueue
	err := p.DbManager.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status = ?", db.TaskSuccess).
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
			Where("id IN ?", ids).Update("status", db.TaskRunning).Error
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
		skip          bool // 标记该 stripe 是否应跳过
		ds, ps        int  // data shards / parity shards 数量
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
			if c.Status == db.ChunkAllocated {
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
	// 并发计算 RS parity：对每个待处理的 stripe 并发调用 computeStripe，
	// 并发上限为 maxParityConcurrency，防止内存被大量 stripe 撑爆。
	// ---------------------------------------------------------------
	type stripeJobRef struct {
		stripeId int64
		job      *stripeJob
	}
	var pending []stripeJobRef
	for _, sid := range stripeIds {
		j, ok := jobs[sid]
		if !ok || j.skip || len(j.dataChunks) == 0 {
			continue
		}
		pending = append(pending, stripeJobRef{stripeId: sid, job: j})
	}

	stripeOK := make([]bool, len(pending))
	stripeResults := make([][]stripeResult, len(pending))

	// 各任务的失败原因已由 computeStripe 内部记录，这里只等全部完成。
	parallelEach(len(pending), maxParityConcurrency, func(i int) error {
		ref := pending[i]
		res, ok := p.computeStripe(ref.stripeId, ref.job.ds, ref.job.ps,
			ref.job.dataChunks, ref.job.parityByIndex, diskById)
		stripeOK[i], stripeResults[i] = ok, res
		return nil
	})

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

	for i, ref := range pending {
		if !stripeOK[i] {
			// 计算失败的 stripe 不更新元数据
			continue
		}
		for _, c := range ref.job.dataChunks {
			allDataIds = append(allDataIds, c.Id)
		}
		for _, sr := range stripeResults[i] {
			c := ref.job.parityByIndex[sr.parityIdx]
			allParityUpdates = append(allParityUpdates, parityUpdate{
				id: c.Id, size: int64(len(sr.parityData)), hash: sr.parityHash[:],
			})
		}
	}

	// ---------------------------------------------------------------
	// Phase 2: 在一个事务中批量更新数据库。
	//
	// 更新项：
	//   - parity chunk：更新 size / hash 并置为 Allocated
	//   - 删除已处理的 WriteQueue 条目
	//
	// TODO: data chunk 的 parity 就绪态（Active）尚未引入，写完即为 Allocated，
	// 因此这里暂时没有 data chunk 的状态更新。
	// ---------------------------------------------------------------
	allParityIds := make([]int64, len(allParityUpdates))
	for i, pu := range allParityUpdates {
		allParityIds[i] = pu.id
	}

	err = p.DbManager.DB.Transaction(func(tx *gorm.DB) error {
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
				parity[i].Status = db.ChunkAllocated
				parity[i].Size = pu.size
				parity[i].Hash = pu.hash
			}
			if err := tx.Save(&parity).Error; err != nil {
				return fmt.Errorf("save parity: %w", err)
			}
		}
		// 删除已处理完毕的 WriteQueue 条目
		return tx.Where("status = ?", db.TaskRunning).Delete(&db.WriteQueue{}).Error
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
				hadWork := p.processStripeQueue()
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
