package pool

import (
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

// handlerFor 返回匹配 disk.Backend 的 ChunkHandler。
func (p *PoolManager) handlerFor(backend db.DiskBackend) ChunkHandler {
	for _, h := range p.Handlers {
		if h.Type() == backend {
			return h
		}
	}
	return nil
}

// computeStripe 纯计算函数：读取 data chunk → RS 编码 → 写入 parity 到磁盘。
// 不涉及任何 DB 操作，调用方负责提供元数据并处理结果更新。
type stripeResult struct {
	parityIdx  int64 // parity index
	parityData []byte
	parityHash [32]byte
}

func (p *PoolManager) computeStripe(stripeId int64, dataShards, parityShards int,
	dataChunks []db.Chunk, parityByIndex map[int64]db.Chunk, diskById map[int64]db.Disk) ([]stripeResult, bool) {

	shards := make([][]byte, dataShards+parityShards)

	// 并行读 data chunk
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
			disk, ok := diskById[c.DiskId]
			if !ok {
				readCh <- readRes{c.Index, nil, fmt.Errorf("disk %d not found", c.DiskId)}
				return
			}
			h := p.handlerFor(disk.Backend)
			if h == nil {
				readCh <- readRes{c.Index, nil, fmt.Errorf("no handler for backend %d", disk.Backend)}
				return
			}
			data, err := h.Read(disk, c.Path)
			readCh <- readRes{c.Index, data, err}
		}(c)
	}
	readWg.Wait()
	close(readCh)

	for r := range readCh {
		if r.err != nil {
			log.Printf("parity: stripe %d read data index %d failed: %v", stripeId, r.idx, r.err)
			return nil, false
		}
		shards[r.idx] = r.data
	}

	// 补齐 data shard 到等长（RS 要求所有 shard 等长）
	var maxSize int
	for _, s := range shards[:dataShards] {
		if len(s) > maxSize {
			maxSize = len(s)
		}
	}
	for i := 0; i < dataShards; i++ {
		if len(shards[i]) < maxSize {
			padded := make([]byte, maxSize)
			copy(padded, shards[i])
			shards[i] = padded
		}
	}
	// 未写入 slot 用全零填充
	for i := 0; i < dataShards; i++ {
		if shards[i] == nil {
			shards[i] = make([]byte, maxSize)
		}
	}
	// parity slot 预分配
	for i := dataShards; i < dataShards+parityShards; i++ {
		shards[i] = make([]byte, maxSize)
	}

	// RS 编码
	enc, err := reedsolomon.New(dataShards, parityShards)
	if err != nil {
		log.Printf("parity: new encoder failed: %v", err)
		return nil, false
	}
	if err := enc.Encode(shards); err != nil {
		log.Printf("parity: encode stripe %d failed: %v", stripeId, err)
		return nil, false
	}

	// 并行写 parity 到磁盘
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
			parityData := shards[dataShards+idx]
			c, ok := parityByIndex[int64(idx)]
			if !ok {
				writeCh <- writeRes{int64(idx), fmt.Errorf("parity %d not found", idx)}
				return
			}
			disk, ok := diskById[c.DiskId]
			if !ok {
				writeCh <- writeRes{int64(idx), fmt.Errorf("disk %d not found", c.DiskId)}
				return
			}
			h := p.handlerFor(disk.Backend)
			if h == nil {
				writeCh <- writeRes{int64(idx), fmt.Errorf("no handler for backend %d", disk.Backend)}
				return
			}
			writeCh <- writeRes{int64(idx), h.Write(disk, c.Path, parityData)}
		}(i)
	}
	writeWg.Wait()
	close(writeCh)

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

// processWriteQueue 处理 parity：一次加载所有数据 → 并发计算 RS → 一次批量写库。
func (p *PoolManager) processWriteQueue() {
	// 事务内原子地领走 QueuePending 条目
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
		return
	}
	if len(entries) == 0 {
		return
	}

	// 收集去重的 stripe ID
	stripeSet := make(map[int64]struct{})
	for _, e := range entries {
		stripeSet[e.StripeId] = struct{}{}
	}
	stripeIds := make([]int64, 0, len(stripeSet))
	for id := range stripeSet {
		stripeIds = append(stripeIds, id)
	}

	// 加载 stripes → pools → disks
	var stripes []db.Stripe
	if err := p.DbManager.DB.Where("id IN ?", stripeIds).Find(&stripes).Error; err != nil {
		log.Printf("parity: query stripes failed: %v", err)
		return
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

	var pools []db.Pool
	if err := p.DbManager.DB.Where("id IN ?", poolIds).Find(&pools).Error; err != nil {
		log.Printf("parity: query pools failed: %v", err)
		return
	}
	poolConfig := make(map[int64]struct{ DataShards, ParityShards int64 })
	for _, pl := range pools {
		poolConfig[pl.Id] = struct{ DataShards, ParityShards int64 }{pl.DataShards, pl.ParityShards}
	}

	var disks []db.Disk
	if err := p.DbManager.DB.Where("pool_id IN ?", poolIds).Find(&disks).Error; err != nil {
		log.Printf("parity: query disks failed: %v", err)
		return
	}
	diskById := make(map[int64]db.Disk, len(disks))
	for i := range disks {
		diskById[disks[i].Id] = disks[i]
	}

	// ── 一次性加载所有 stripe 的 chunk 元数据 ──
	var allChunks []db.Chunk
	if err := p.DbManager.DB.Where("stripe_id IN ?", stripeIds).Find(&allChunks).Error; err != nil {
		log.Printf("parity: query chunks failed: %v", err)
		return
	}

	type stripeJob struct {
		dataChunks    []db.Chunk
		parityByIndex map[int64]db.Chunk
		skip          bool
		ds, ps        int
	}
	jobs := make(map[int64]*stripeJob, len(stripeIds))

	// 按 stripe 组织
	for _, c := range allChunks {
		j, ok := jobs[c.StripeId]
		if !ok {
			pid := poolMap[c.StripeId]
			cfg := poolConfig[pid]
			j = &stripeJob{ds: int(cfg.DataShards), ps: int(cfg.ParityShards)}
			jobs[c.StripeId] = j
		}
		if c.Type == db.DataChunk {
			if c.Status == db.ChunkPending || c.Status == db.ChunkError {
				log.Printf("parity: stripe %d data index %d status=%d, skip", c.StripeId, c.Index, c.Status)
				j.skip = true
				continue
			}
			if c.Status != db.ChunkReserved {
				// collect ordered by index later
				j.dataChunks = append(j.dataChunks, c)
			}
		} else if c.Type == db.ParityChunk {
			if j.parityByIndex == nil {
				j.parityByIndex = make(map[int64]db.Chunk)
			}
			j.parityByIndex[c.Index] = c
		}
	}
	// 排序 data chunks by Index（用 map O(n)）
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

	// ── Phase 1: 统一锁所有 parity chunk，标记 Pending ──
	err = p.DbManager.DB.Transaction(func(tx *gorm.DB) error {
		var allParity []db.Chunk
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("stripe_id IN ? AND type = ?", stripeIds, db.ParityChunk).
			Find(&allParity).Error; err != nil {
			return fmt.Errorf("lock parity: %w", err)
		}
		// 按 stripe 分组检查 Pending
		pendingStripes := make(map[int64]bool)
		for _, c := range allParity {
			if c.Status == db.ChunkPending {
				pendingStripes[c.StripeId] = true
			}
		}
		for sid := range pendingStripes {
			if j, ok := jobs[sid]; ok {
				j.skip = true
				log.Printf("parity: stripe %d parity pending, skip", sid)
			}
		}
		// 收集非 skip stripe 的 parity ids
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
		return
	}

	// ── 并发计算 RS parity（无 DB 操作）──
	type jobResult struct {
		stripeId int64
		ok       bool
		results  []stripeResult
	}
	resultCh := make(chan jobResult, len(jobs))
	var computeWg sync.WaitGroup
	sem := make(chan struct{}, 4)

	for _, sid := range stripeIds {
		j, ok := jobs[sid]
		if !ok || j.skip || len(j.dataChunks) == 0 {
			continue
		}
		computeWg.Add(1)
		go func(sid int64, j *stripeJob) {
			defer computeWg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res, ok := p.computeStripe(sid, j.ds, j.ps, j.dataChunks, j.parityByIndex, diskById)
			resultCh <- jobResult{stripeId: sid, ok: ok, results: res}
		}(sid, j)
	}
	computeWg.Wait()
	close(resultCh)

	// 收集计算结果
	type parityUpdate struct {
		id     int64
		size   int64
		hash   []byte
	}
	var allDataIds []int64
	var allParityUpdates []parityUpdate

	for r := range resultCh {
		if !r.ok {
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

	// ── Phase 2: 批量更新 DB + 标记 QueueDone（同一事务）──
	allParityIds := make([]int64, len(allParityUpdates))
	for i, pu := range allParityUpdates {
		allParityIds[i] = pu.id
	}

	err = p.DbManager.DB.Transaction(func(tx *gorm.DB) error {
		// data chunks → Active
		if len(allDataIds) > 0 {
			if err := tx.Model(&db.Chunk{}).Where("id IN ?", allDataIds).
				Update("status", db.ChunkActive).Error; err != nil {
				return fmt.Errorf("update data: %w", err)
			}
		}
		// parity chunks 一次性加载后 Save
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
		// 标记 WriteQueue 完成
		return tx.Model(&db.WriteQueue{}).
			Where("status = ?", db.QueueProcessing).Update("status", db.QueueDone).Error
	})
	if err != nil {
		log.Printf("parity: phase2 failed: %v", err)
		return
	}

	log.Printf("parity: batch done (%d stripes, %d data, %d parity)",
		len(stripeIds), len(allDataIds), len(allParityUpdates))
}

// StartParityWorker 启动后台 goroutine，监听 WriteQueue channel。
func (p *PoolManager) StartParityWorker(batchSize int) {
	if batchSize <= 0 {
		batchSize = 100
	}
	interval := 1 * time.Second

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		var pending int
		for {
			select {
			case entries := <-p.WriteQueue:
				pending += len(entries)
				if pending >= batchSize {
					p.processWriteQueue()
					pending = 0
				}
			case <-ticker.C:
				if pending > 0 {
					p.processWriteQueue()
					pending = 0
				}
			}
		}
	}()
	log.Printf("parity worker started (batch=%d, interval=%v)", batchSize, interval)
}
