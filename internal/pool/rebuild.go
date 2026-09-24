package pool

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/klauspost/reedsolomon"
	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
	"github.com/zzc53/zenofs/internal/hash"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ---------------------------------------------------------------
// 重建作业投递
// ---------------------------------------------------------------

const (
	// taskNameRebuildPool 重建作业的 Task.Name 前缀，实际名称会带上 poolId。
	taskNameRebuildPool = "rebuild-pool"
	// rebuildBatchSize 单次批量插入的子任务条数上限，避免撞上 SQL 变量数量限制。
	rebuildBatchSize = 500
)

// rebuildTaskName 返回某个池的重建作业名，同时用于作业级去重。
func rebuildTaskName(poolId int64) string {
	return fmt.Sprintf("%s:%d", taskNameRebuildPool, poolId)
}

// RebuildPool 为池内所有待修复（status = Repair）磁盘上的分片投递条带重建作业。
//
// 流程（单个事务内批量完成）：
//  1. 该池已有进行中的重建作业时直接返回 0（作业级去重）；
//  2. 找出该池下所有 Repair 磁盘；
//  3. 取出这些磁盘上已写入的分片所属的 stripe（去重；预分配未写入的 slot 不算）；
//  4. 建一条 Task 作为作业，再分批写入带 TaskId 的 StripeQueue 子任务。
//
// 返回本次投递的子任务数。重建由后台 worker 消费；作业下最后一个子任务处理完时，
// 会校验磁盘数据并把磁盘与 pool 恢复为 Online，作业状态置为 Success / Fail。
func (p *PoolManager) RebuildPool(poolId int64) (int, error) {
	var queued int

	err := p.DbManager.Tx(func(tx *gorm.DB) error {
		// 1. 作业级去重：同一个 pool 同时只允许一个进行中的重建作业
		var active int64
		if err := tx.Model(&db.Task{}).
			Where("name = ? AND status IN ?", rebuildTaskName(poolId),
				[]db.TaskStatus{db.TaskPending, db.TaskRunning}).
			Count(&active).Error; err != nil {
			return errs.DBQuery(err)
		}
		if active > 0 {
			return nil
		}

		// 2. 待修复的磁盘
		var diskIds []int64
		if err := tx.Model(&db.Disk{}).
			Where("pool_id = ? AND status = ?", poolId, db.Repair).
			Pluck("id", &diskIds).Error; err != nil {
			return errs.DBQuery(err)
		}
		if len(diskIds) == 0 {
			return nil // 没有待修复的盘
		}

		// 3. 这些盘上已写入的分片涉及的条带
		var stripeIds []int64
		if err := tx.Model(&db.Chunk{}).
			Distinct("stripe_id").
			Where("disk_id IN ? AND status != ?", diskIds, db.ChunkReserved).
			Pluck("stripe_id", &stripeIds).Error; err != nil {
			return errs.DBQuery(err)
		}
		if len(stripeIds) == 0 {
			return nil
		}

		// 4. 建作业 + 分批建子任务
		metadata, err := json.Marshal(map[string]any{
			"pool_id": poolId,
			"disks":   diskIds,
			"stripes": len(stripeIds),
		})
		if err != nil {
			return errs.DBQuery(err)
		}
		task := db.Task{
			Name:     rebuildTaskName(poolId),
			Status:   db.TaskPending,
			Message:  fmt.Sprintf("rebuild %d stripe(s) on %d disk(s)", len(stripeIds), len(diskIds)),
			Metadata: datatypes.JSON(metadata),
		}
		if err := tx.Create(&task).Error; err != nil {
			return errs.DBQuery(err)
		}

		for start := 0; start < len(stripeIds); start += rebuildBatchSize {
			end := start + rebuildBatchSize
			if end > len(stripeIds) {
				end = len(stripeIds)
			}
			batch := make([]db.StripeQueue, 0, end-start)
			for _, sid := range stripeIds[start:end] {
				batch = append(batch, db.StripeQueue{
					StripeId: sid,
					Type:     db.StripeQueueRebuild,
					TaskId:   task.Id,
					Status:   db.TaskPending,
				})
			}
			if err := tx.Create(&batch).Error; err != nil {
				return errs.DBQuery(err)
			}
			queued += len(batch)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return queued, nil
}

// ---------------------------------------------------------------
// 条带重建
// ---------------------------------------------------------------

// rebuiltChunk 记录一个刚由 RS 恢复出来、已写回磁盘的分片。
type rebuiltChunk struct {
	chunkId int64
	size    int64
	hash    []byte
}

// RebuildByChunks 为这些分片所属的条带投递重建任务。
//
// 与 RebuildPool 的区别：它不依赖"磁盘处于 Repair 状态"，而是直接为指定分片
// 所在的条带建任务（TaskId 为 0，不挂作业），供读路径发现坏分片时即时修复。
func (p *PoolManager) RebuildByChunks(chunkIds []int64) (int, error) {
	var queued int
	err := p.DbManager.Tx(func(tx *gorm.DB) error {
		var stripeIds []int64
		if err := tx.Model(&db.Chunk{}).Distinct("stripe_id").
			Where("id IN ?", chunkIds).Pluck("stripe_id", &stripeIds).Error; err != nil {
			return errs.DBQuery(err)
		}
		if len(stripeIds) == 0 {
			return nil
		}
		// 队列里已有同名条带的任务就不重复投递
		var existing []int64
		if err := tx.Model(&db.StripeQueue{}).
			Where("stripe_id IN ? AND type = ?", stripeIds, db.StripeQueueRebuild).
			Pluck("stripe_id", &existing).Error; err != nil {
			return errs.DBQuery(err)
		}
		seen := make(map[int64]struct{}, len(existing))
		for _, id := range existing {
			seen[id] = struct{}{}
		}
		var batch []db.StripeQueue
		for _, sid := range stripeIds {
			if _, ok := seen[sid]; ok {
				continue
			}
			batch = append(batch, db.StripeQueue{
				StripeId: sid,
				Type:     db.StripeQueueRebuild,
				Status:   db.TaskPending,
			})
		}
		if len(batch) == 0 {
			return nil
		}
		if err := tx.Create(&batch).Error; err != nil {
			return errs.DBQuery(err)
		}
		queued = len(batch)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return queued, nil
}

// rebuildStripe 处理一批条带重建任务。
//
// 与 calculateStripeParity 的区别：
//   - 任务来自 StripeQueue 中 type = StripeQueueRebuild 的条目；
//   - 把该条带的 data 与 parity shard 一起交给 RS 解码器，缺失的一并恢复，
//     而不是只计算 parity；
//   - 恢复结果写回磁盘并批量更新 chunk 元数据；数据补齐后把涉及的磁盘与
//     所属 pool 恢复为 Online。
//
// 返回 true 表示处理了至少一个条目。
func (p *PoolManager) rebuildStripe() bool {
	entries := p.claimStripeTasks(db.StripeQueueRebuild)
	if len(entries) == 0 {
		return false
	}

	stripeIds := stripeIdsOf(entries)
	meta, err := p.loadStripeMeta(stripeIds)
	if err != nil {
		log.Printf("rebuild: load metadata failed: %v", err)
		return false
	}
	jobs := buildStripeJobs(stripeIds, meta)

	var pending []stripeJobRef
	for _, sid := range stripeIds {
		if j, ok := jobs[sid]; ok {
			pending = append(pending, stripeJobRef{stripeId: sid, job: j})
		}
	}

	stripeOK := make([]bool, len(pending))
	stripeRebuilt := make([][]rebuiltChunk, len(pending))

	// 各条带的失败原因已由 rebuildStripeJob 内部记录，这里只等全部完成。
	parallelEach(len(pending), maxParityConcurrency, func(i int) error {
		ref := pending[i]
		res, ok := p.rebuildStripeJob(ref.stripeId, ref.job, meta.diskById)
		stripeOK[i], stripeRebuilt[i] = ok, res
		return nil
	})

	// 汇总所有重建成功的分片，一次 upsert 批量写回元数据
	var updates []rebuiltChunk
	for i, ok := range stripeOK {
		if ok {
			updates = append(updates, stripeRebuilt[i]...)
		}
	}
	if len(updates) > 0 {
		if err := p.saveRebuiltChunks(updates); err != nil {
			log.Printf("rebuild: save chunk metadata failed: %v", err)
		}
	}

	// 删除已处理的 StripeQueue 条目
	if err := p.DbManager.DB.
		Where("status = ? AND type = ?", db.TaskRunning, db.StripeQueueRebuild).
		Delete(&db.StripeQueue{}).Error; err != nil {
		log.Printf("rebuild: delete queue entries failed: %v", err)
	}

	// 作业收尾：子任务全部处理完时校验数据，并把磁盘与 pool 恢复 Online
	p.finishRebuildTasks(entries, stripeIds, meta)

	log.Printf("rebuild: batch done (%d stripes, %d shards recovered)", len(stripeIds), len(updates))
	return true
}

// finishRebuildTasks 收尾已经跑完的重建作业。
//
// 某个作业（Task）名下的子任务全部处理完后：
//  1. 校验相关磁盘的数据完整性，把磁盘与 pool 恢复为 Online（见 recoverAfterRebuild）；
//  2. 该 pool 下仍有非 Online 的盘则把作业标记为 TaskFail，否则标记 TaskSuccess。
//
// 作业状态以最终的数据状态为准，而不是以"子任务是否跑完"为准。
func (p *PoolManager) finishRebuildTasks(entries []db.StripeQueue, stripeIds []int64, meta *stripeMeta) {
	taskSet := make(map[int64]struct{})
	for _, e := range entries {
		if e.TaskId > 0 {
			taskSet[e.TaskId] = struct{}{}
		}
	}
	if len(taskSet) == 0 {
		return // 本批没有属于任何作业的任务
	}

	poolSet := make(map[int64]struct{})
	for _, sid := range stripeIds {
		if pid, ok := meta.poolMap[sid]; ok {
			poolSet[pid] = struct{}{}
		}
	}

	for taskId := range taskSet {
		var left int64
		if err := p.DbManager.DB.Model(&db.StripeQueue{}).
			Where("task_id = ?", taskId).Count(&left).Error; err != nil {
			log.Printf("rebuild: count sub tasks of task %d failed: %v", taskId, err)
			continue
		}
		if left > 0 {
			continue // 作业还没跑完
		}

		// 数据校验 + 恢复 Online
		p.recoverAfterRebuild(meta)

		status, message := db.TaskSuccess, "all disks recovered"
		for poolId := range poolSet {
			var bad int64
			if err := p.DbManager.DB.Model(&db.Disk{}).
				Where("pool_id = ? AND status != ?", poolId, db.Online).
				Count(&bad).Error; err != nil {
				log.Printf("rebuild: count disks of pool %d failed: %v", poolId, err)
				status, message = db.TaskFail, "check disk status failed"
				break
			}
			if bad > 0 {
				status = db.TaskFail
				message = fmt.Sprintf("%d disk(s) still not online", bad)
				break
			}
		}

		if err := p.DbManager.DB.Model(&db.Task{}).Where("id = ?", taskId).
			Updates(map[string]any{"status": status, "message": message}).Error; err != nil {
			log.Printf("rebuild: update task %d failed: %v", taskId, err)
			continue
		}
		log.Printf("rebuild: task %d finished (status=%d): %s", taskId, status, message)
	}
}

// rebuildStripeJob 用一个条带的全部 data + parity shard 做一次 RS 恢复。
//
// 缺失（读失败，或内容 BLAKE3 与元数据不符）的 shard 交给 Reed-Solomon 重建，
// 重建结果写回磁盘并返回，供调用方批量更新元数据。
// 条带完整时返回 (nil, true)；缺失数超过 parity shard 数无法恢复时返回 (nil, false)。
func (p *PoolManager) rebuildStripeJob(stripeId int64, job *stripeJob, diskById map[int64]db.Disk) ([]rebuiltChunk, bool) {
	total := job.ds + job.ps
	shards := make([][]byte, total)

	// data 与 parity shard 一起参与：data 占 [0, ds)，parity 占 [ds, ds+ps)
	type shardRef struct {
		chunk    db.Chunk
		shardIdx int
	}
	refs := make([]shardRef, 0, total)
	for _, c := range job.dataChunks {
		refs = append(refs, shardRef{chunk: c, shardIdx: int(c.Index)})
	}
	for idx, c := range job.parityByIndex {
		if c.Status == db.ChunkReserved {
			continue // 还没算过 parity 的 slot，本来就没有数据
		}
		refs = append(refs, shardRef{chunk: c, shardIdx: job.ds + int(idx)})
	}

	// 并发读取并校验：读不到或内容对不上 hash 的记为缺失
	missing := make([]bool, total)
	parallelEach(len(refs), 0, func(i int) error {
		ref := refs[i]
		data, ok := p.readVerifiedChunk(ref.chunk, diskById)
		if !ok {
			missing[ref.shardIdx] = true
			return nil
		}
		shards[ref.shardIdx] = data
		return nil
	})

	missingCount := 0
	for _, m := range missing {
		if m {
			missingCount++
		}
	}
	if missingCount == 0 {
		return nil, true // 条带完整，无需重建
	}
	if missingCount > job.ps {
		log.Printf("rebuild: stripe %d has %d missing shards, more than %d parity",
			stripeId, missingCount, job.ps)
		return nil, false
	}

	// padding 到等长（RS 要求）；缺失的 shard 保持 nil，交给 Reconstruct 填充
	var maxSize int
	for _, s := range shards {
		if len(s) > maxSize {
			maxSize = len(s)
		}
	}
	if maxSize == 0 {
		log.Printf("rebuild: stripe %d has no readable shard to size against", stripeId)
		return nil, false
	}
	for i := range shards {
		if shards[i] == nil || len(shards[i]) == maxSize {
			continue
		}
		padded := make([]byte, maxSize)
		copy(padded, shards[i])
		shards[i] = padded
	}

	enc, err := reedsolomon.New(job.ds, job.ps)
	if err != nil {
		log.Printf("rebuild: new encoder failed: %v", err)
		return nil, false
	}
	if err := enc.Reconstruct(shards); err != nil {
		log.Printf("rebuild: reconstruct stripe %d failed: %v", stripeId, err)
		return nil, false
	}

	// 把恢复出来的分片写回磁盘
	rebuilt := make([]rebuiltChunk, len(refs))
	written := make([]bool, len(refs))
	writeErrs := parallelEach(len(refs), 0, func(i int) error {
		ref := refs[i]
		if !missing[ref.shardIdx] {
			return nil // 本来就有的分片不用重写
		}
		data := shards[ref.shardIdx]
		// data chunk 按元数据里的原始大小截断，去掉 RS 的 padding；
		// parity shard 本身就是满长的，原样写回。
		if ref.chunk.Type == db.DataChunk && ref.chunk.Size > 0 && ref.chunk.Size <= int64(len(data)) {
			data = data[:ref.chunk.Size]
		}
		disk, ok := diskById[ref.chunk.DiskId]
		if !ok {
			return fmt.Errorf("chunk %d: disk %d not found", ref.chunk.Id, ref.chunk.DiskId)
		}
		h := p.handlerFor(disk.Backend)
		if h == nil {
			return fmt.Errorf("chunk %d: no handler for backend %d", ref.chunk.Id, disk.Backend)
		}
		if err := h.Write(disk, ref.chunk.Path, data); err != nil {
			return fmt.Errorf("chunk %d: %w", ref.chunk.Id, err)
		}
		rebuilt[i] = rebuiltChunk{chunkId: ref.chunk.Id, size: int64(len(data)), hash: hash.Sum(data)}
		written[i] = true
		return nil
	})

	out := make([]rebuiltChunk, 0, missingCount)
	failed := 0
	for i, err := range writeErrs {
		if err != nil {
			log.Printf("rebuild: stripe %d write chunk %d failed: %v", stripeId, refs[i].chunk.Id, err)
			failed++
			continue
		}
		if written[i] {
			out = append(out, rebuilt[i])
		}
	}
	return out, failed == 0
}

// saveRebuiltChunks 一次 upsert 批量写回重建出来的 chunk 元数据（size / hash / status）。
func (p *PoolManager) saveRebuiltChunks(updates []rebuiltChunk) error {
	chunks := make([]db.Chunk, len(updates))
	for i, u := range updates {
		chunks[i] = db.Chunk{
			Id:     u.chunkId,
			Size:   u.size,
			Hash:   u.hash,
			Status: db.ChunkAllocated,
		}
	}
	if err := p.DbManager.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"size", "hash", "status"}),
	}).Create(&chunks).Error; err != nil {
		return errs.DBQuery(err)
	}
	return nil
}

// ---------------------------------------------------------------
// 重建后的 Online 恢复
// ---------------------------------------------------------------

// recoverAfterRebuild 在重建后尝试把磁盘与存储池恢复为 Online。
//
// 判定方式（数据驱动，不看任务队列）：
//   - 磁盘上所有已写入的 chunk 都能读出且 BLAKE3 与元数据一致，才把该盘从
//     Repair 恢复为 Online；
//   - 该 pool 下所有磁盘都 Online 时，才把 pool 恢复为 Online。
//
// 人为置为 Offline 的盘不会被这里的判定碰到（只处理 Repair）。
func (p *PoolManager) recoverAfterRebuild(meta *stripeMeta) {
	diskIds := make([]int64, 0, len(meta.diskById))
	for id := range meta.diskById {
		diskIds = append(diskIds, id)
	}
	if len(diskIds) == 0 {
		return
	}

	var repairDisks []db.Disk
	if err := p.DbManager.DB.Where("id IN ? AND status = ?", diskIds, db.Repair).
		Find(&repairDisks).Error; err != nil {
		log.Printf("rebuild: query repair disks failed: %v", err)
		return
	}
	if len(repairDisks) == 0 {
		return // 本次没牵扯到处于 Repair 的盘
	}

	var recovered []int64
	poolSet := make(map[int64]struct{}, len(repairDisks))
	for _, d := range repairDisks {
		poolSet[d.PoolId] = struct{}{}
		if p.diskDataComplete(d) {
			recovered = append(recovered, d.Id)
		}
	}

	if len(recovered) > 0 {
		// 一条 SQL 批量把校验通过的盘置回 Online
		if err := p.DbManager.DB.Model(&db.Disk{}).
			Where("id IN ? AND status = ?", recovered, db.Repair).
			Update("status", db.Online).Error; err != nil {
			log.Printf("rebuild: mark disks online failed: %v", err)
			return
		}
		log.Printf("rebuild: %d disk(s) back online", len(recovered))
	}

	// 该 pool 下不再有非 Online 的盘时，pool 恢复 Online
	poolIds := make([]int64, 0, len(poolSet))
	for id := range poolSet {
		poolIds = append(poolIds, id)
	}
	for _, poolId := range poolIds {
		var bad int64
		if err := p.DbManager.DB.Model(&db.Disk{}).
			Where("pool_id = ? AND status != ?", poolId, db.Online).
			Count(&bad).Error; err != nil {
			log.Printf("rebuild: count disks of pool %d failed: %v", poolId, err)
			continue
		}
		if bad > 0 {
			continue
		}
		if err := p.DbManager.DB.Model(&db.Pool{}).Where("id = ?", poolId).
			Update("status", db.Online).Error; err != nil {
			log.Printf("rebuild: pool %d back online failed: %v", poolId, err)
			continue
		}
		log.Printf("rebuild: pool %d back online", poolId)
	}
}

// diskDataComplete 检查磁盘上所有已写入的 chunk 是否都能读出且校验一致。
func (p *PoolManager) diskDataComplete(disk db.Disk) bool {
	var chunks []db.Chunk
	if err := p.DbManager.DB.Where("disk_id = ? AND status != ?", disk.Id, db.ChunkReserved).
		Find(&chunks).Error; err != nil {
		log.Printf("rebuild: query chunks of disk %d failed: %v", disk.Id, err)
		return false
	}
	diskById := map[int64]db.Disk{disk.Id: disk}
	complete := make([]bool, len(chunks))
	parallelEach(len(chunks), 0, func(i int) error {
		_, complete[i] = p.readVerifiedChunk(chunks[i], diskById)
		return nil
	})
	for _, ok := range complete {
		if !ok {
			return false
		}
	}
	return true
}

// readVerifiedChunk 读取一个分片并校验内容与元数据一致。
// 返回 false 表示读不到，或读到的内容 BLAKE3 与 chunks.hash 不符。
func (p *PoolManager) readVerifiedChunk(c db.Chunk, diskById map[int64]db.Disk) ([]byte, bool) {
	disk, ok := diskById[c.DiskId]
	if !ok {
		return nil, false
	}
	h := p.handlerFor(disk.Backend)
	if h == nil {
		return nil, false
	}
	data, err := h.Read(disk, c.Path)
	if err != nil {
		return nil, false
	}
	if !hash.Equal(data, c.Hash) {
		return nil, false
	}
	return data, true
}
