package pool

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/klauspost/reedsolomon"
	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
	"github.com/zzc53/zenofs/internal/hash"
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

	// 0. 池里没有校验分片（ParityShards == 0，纯条带池）：本来就没有 parity 要算，
	// 直接返回。少了这个早退，下面会把整条带的 data chunk 全读进内存、
	// 再喂给 reedsolomon 编码出 0 个分片——纯粹的 I/O 和内存浪费。
	if parityShards == 0 {
		return nil, true
	}

	// ---------------------------------------------------------------
	// 1. 分配 shard 数组。shards[0..dataShards-1] 放 data，
	// shards[dataShards..] 放 parity。
	// ---------------------------------------------------------------
	shards := make([][]byte, dataShards+parityShards)

	// ---------------------------------------------------------------
	// 2. 并发读取所有 data chunk 的数据，按 index 填入 shards 数组。
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
	// 3. 将所有 data shard padding 到等长。
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
	// 4. 初始化 Reed-Solomon 编码器，执行编码。
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
	// 5. 并发将 parity shard 写入磁盘，并计算各 shard 的 BLAKE3 哈希。
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
			parityHash: hash.SumArray(parityData),
		}
		return nil
	})
	if err := firstErr(writeErrs); err != nil {
		log.Printf("parity: stripe %d write parity failed: %v", stripeId, err)
		return nil, false
	}
	return results, true
}

// calculateStripeParity 是 parity worker 的核心调度函数。
// 任务来自 StripeQueue 中 type = StripeQueueParity 的条目（由 Flush 从 write queue 搬运而来）。
//
// 整体流程：
//  1. 领取一批任务（清理残留 Running、SKIP LOCKED 领走 Pending，见 claimStripeTasks）
//  2. 加载 stripe/pool/disk/chunk 元数据（见 loadStripeMeta）
//  3. 按 stripe 组织待编码的 data chunk 与 parity chunk（见 buildStripeJobs）
//  4. 并发调用 computeStripe 对每个 stripe 执行 RS 编码（并发上限 maxParityConcurrency）
//  5. 在一个事务中批量写回 parity chunk 的 size/hash，并删除已处理的 StripeQueue 条目
//
// 返回 true 表示处理了至少一个条目，false 表示空闲。
func (p *PoolManager) calculateStripeParity() bool {
	entries := p.claimStripeTasks(db.StripeQueueParity)
	if len(entries) == 0 {
		return false
	}

	stripeIds := stripeIdsOf(entries)
	meta, err := p.loadStripeMeta(stripeIds)
	if err != nil {
		log.Printf("parity: load metadata failed: %v", err)
		return false
	}

	jobs := buildStripeJobs(stripeIds, meta)

	// ---------------------------------------------------------------
	// 并发计算 RS parity：对每个待处理的 stripe 并发调用 computeStripe，
	// 并发上限为 maxParityConcurrency，防止内存被大量 stripe 撑爆。
	// ---------------------------------------------------------------
	var pending []stripeJobRef
	for _, sid := range stripeIds {
		j, ok := jobs[sid]
		if !ok || len(j.dataChunks) == 0 {
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
			ref.job.dataChunks, ref.job.parityByIndex, meta.diskById)
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
	// 5. 在一个事务中批量更新数据库。
	//
	// 更新项：
	//   - parity chunk：更新 size / hash 并置为 Allocated
	//   - 删除已处理的 StripeQueue 条目
	//
	// TODO: data chunk 的 parity 就绪态（Active）尚未引入，写完即为 Allocated，
	// 因此这里暂时没有 data chunk 的状态更新。
	// ---------------------------------------------------------------
	allParityIds := make([]int64, len(allParityUpdates))
	for i, pu := range allParityUpdates {
		allParityIds[i] = pu.id
	}

	err = p.DbManager.Tx(func(tx *gorm.DB) error {
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
		// 删除已处理完毕的 StripeQueue 条目
		return tx.Where("status = ?", db.TaskRunning).Delete(&db.StripeQueue{}).Error
	})
	if err != nil {
		log.Printf("parity: update metadata failed: %v", err)
		return false
	}

	log.Printf("parity: batch done (%d stripes, %d data, %d parity)",
		len(stripeIds), len(allDataIds), len(allParityUpdates))
	return true
}

// StartParityWorker 启动后台 goroutine，按上下文退避轮询 DB，
// 依次驱动条带重建（rebuildStripe）与 parity 计算（calculateStripeParity）。
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
				// 每轮两个队列各推进一次：重建优先于 parity 计算
				hadWork := p.rebuildStripe()
				if p.calculateStripeParity() {
					hadWork = true
				}
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

// ---------------------------------------------------------------
// 条带任务队列的公共部分
// ---------------------------------------------------------------

// stripeJob 是单个 stripe 的处理单元。
// dataChunks 是参与编码/重建的 data chunk（按 index 升序），
// parityByIndex 是 parity chunk 按 index 的索引；两者合起来即该条带的全部分片。
type stripeJob struct {
	dataChunks    []db.Chunk
	parityByIndex map[int64]db.Chunk
	ds, ps        int // data shards / parity shards 数量
}

// stripeJobRef 把一个 stripe ID 与它的处理单元绑在一起。
type stripeJobRef struct {
	stripeId int64
	job      *stripeJob
}

// stripeMeta 是处理一批 stripe 任务所需的元数据快照。
type stripeMeta struct {
	poolMap    map[int64]int64 // stripeId → poolId
	poolConfig map[int64]struct{ DataShards, ParityShards int64 }
	diskById   map[int64]db.Disk
	chunks     []db.Chunk // 这批 stripe 的全部 chunk
}

// claimStripeTasks 清理该类型残留的 Running 条目，并在一个事务中原子地领走一批 Pending 任务。
//
// 进程在上次批次中间崩溃时 Running 条目会永远卡住，这里按类型重置为 Pending 重新处理；
// 领取用 SKIP LOCKED 避免多个 worker 之间的锁竞争，领到后置为 TaskRunning。
// 队列为空时返回 nil。
func (p *PoolManager) claimStripeTasks(queueType db.StripeQueueType) []db.StripeQueue {
	p.DbManager.DB.Model(&db.StripeQueue{}).
		Where("status = ? AND type = ?", db.TaskRunning, queueType).
		Update("status", db.TaskPending)

	var entries []db.StripeQueue
	err := p.DbManager.Tx(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status = ? AND type = ?", db.TaskPending, queueType).
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
		return tx.Model(&db.StripeQueue{}).
			Where("id IN ?", ids).Update("status", db.TaskRunning).Error
	})
	if err != nil {
		log.Printf("stripe queue: claim tasks failed: %v", err)
		return nil
	}
	return entries
}

// stripeIdsOf 提取这批任务涉及的 stripe ID（去重）。
func stripeIdsOf(entries []db.StripeQueue) []int64 {
	set := make(map[int64]struct{}, len(entries))
	for _, e := range entries {
		set[e.StripeId] = struct{}{}
	}
	ids := make([]int64, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	return ids
}

// loadStripeMeta 加载 stripe → pool → disk → chunk 的完整元数据链。
func (p *PoolManager) loadStripeMeta(stripeIds []int64) (*stripeMeta, error) {
	var stripes []db.Stripe
	if err := p.DbManager.DB.Where("id IN ?", stripeIds).Find(&stripes).Error; err != nil {
		return nil, errs.DBQuery(err)
	}
	poolMap := make(map[int64]int64, len(stripes))
	poolSet := make(map[int64]struct{})
	for _, s := range stripes {
		poolMap[s.Id] = s.PoolId
		poolSet[s.PoolId] = struct{}{}
	}
	poolIds := make([]int64, 0, len(poolSet))
	for id := range poolSet {
		poolIds = append(poolIds, id)
	}

	var pools []db.Pool
	if err := p.DbManager.DB.Where("id IN ?", poolIds).Find(&pools).Error; err != nil {
		return nil, errs.DBQuery(err)
	}
	poolConfig := make(map[int64]struct{ DataShards, ParityShards int64 }, len(pools))
	for _, pl := range pools {
		poolConfig[pl.Id] = struct{ DataShards, ParityShards int64 }{pl.DataShards, pl.ParityShards}
	}

	var disks []db.Disk
	if err := p.DbManager.DB.Where("pool_id IN ?", poolIds).Find(&disks).Error; err != nil {
		return nil, errs.DBQuery(err)
	}
	diskById := make(map[int64]db.Disk, len(disks))
	for i := range disks {
		diskById[disks[i].Id] = disks[i]
	}

	var allChunks []db.Chunk
	if err := p.DbManager.DB.Where("stripe_id IN ?", stripeIds).Find(&allChunks).Error; err != nil {
		return nil, errs.DBQuery(err)
	}

	return &stripeMeta{
		poolMap:    poolMap,
		poolConfig: poolConfig,
		diskById:   diskById,
		chunks:     allChunks,
	}, nil
}

// buildStripeJobs 把 chunk 元数据按 stripe 组织成处理单元。
// dataChunks 只收已写入的 data chunk（Reserved 是预分配但没写数据的 slot），并按 index 升序排列。
func buildStripeJobs(stripeIds []int64, meta *stripeMeta) map[int64]*stripeJob {
	jobs := make(map[int64]*stripeJob, len(stripeIds))
	for _, c := range meta.chunks {
		j, ok := jobs[c.StripeId]
		if !ok {
			cfg := meta.poolConfig[meta.poolMap[c.StripeId]]
			j = &stripeJob{ds: int(cfg.DataShards), ps: int(cfg.ParityShards)}
			jobs[c.StripeId] = j
		}
		switch c.Type {
		case db.DataChunk:
			if c.Status == db.ChunkReserved {
				continue // 预分配但还没写入数据的 slot，不参与编码
			}
			j.dataChunks = append(j.dataChunks, c)
		case db.ParityChunk:
			if j.parityByIndex == nil {
				j.parityByIndex = make(map[int64]db.Chunk)
			}
			j.parityByIndex[c.Index] = c
		}
	}

	// 对每个 stripe 的 data chunks 按 Index 排序（确保 RS 编码的顺序正确）
	for _, j := range jobs {
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
	return jobs
}
