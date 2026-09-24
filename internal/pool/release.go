package pool

import (
	"fmt"
	"log"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// releaseBatchSize 是单条 SQL 一次处理的 chunk 数上限。
// 与 rebuildBatchSize 同源：SQL 的绑定变量数量有限（SQLite 尤其保守），
// 而这里的候选集来自整棵文件树的版本引用，可能上万，必须分批。
const releaseBatchSize = 500

// StaleFile 是一个"应当被物理删除"的文件落点（disk.Path 下的相对路径）。
//
// 它由 ReleaseChunks 在事务里产出（旧 chunk 文件、读缓存副本、已空置条带的
// parity 文件），事务提交后交给 DeleteStaleFiles 执行。分成两步是因为事务里
// 删不了磁盘文件：先让元数据自洽（这些路径已经不被引用），再删就不会有竞态。
type StaleFile struct {
	diskId int64
	path   string
}

// ReleaseResult 是一次释放的结果。
type ReleaseResult struct {
	// Stale 是需要在事务提交后物理删除的旧文件。
	Stale []StaleFile
	// Chunks 是真正被回滚成空槽的 data 分片数。
	Chunks int
	// Bytes 是这些分片释放出来的数据字节数（不含 parity 的摊销）。
	Bytes int64
}

// ReleaseChunks 把一批"已经没有任何 version_chunk 引用"的 data chunk 回滚成
// 预分配空槽（ChunkReserved），并清理依附在这些 chunk 上的其它状态。
//
// 必须在调用方的事务里执行（tx）；返回的 StaleFile 必须在**事务提交之后**
// 交给 DeleteStaleFiles——顺序反了会删掉并发写入刚写进同一路径的新数据。
//
// 为什么是"回滚空槽"而不是删掉 chunks 行：
//   - 条带的槽位布局（data/parity 数量、盘分布、index）是 getNewChunks 分配、
//     rebuildStripe 重建的前提，删行会让它与 pool 的 data+parity 配置对不上；
//   - ChunkReserved 是现成的"可用空槽"语义：getNewChunks 的 Phase 1 会复用它，
//     buildStripeJobs / rebuildStripeJob 都会跳过它，天然不会读到空槽。
//
// 幂等：只有 type=data 且 status=Allocated 的行会被处理，重复调用是空操作。
func (p *PoolManager) ReleaseChunks(tx *gorm.DB, chunkIds []int64) (ReleaseResult, error) {
	ids := uniqueIds(chunkIds)
	if len(ids) == 0 {
		return ReleaseResult{}, nil
	}

	// 0. 引用检查：只碰"已经没有任何 version_chunk 引用"的分片。
	//
	// 调用方（彻底删除 / 孤儿 GC）负责把候选缩到"可能无引用"的集合，这里再做
	// 一次权威判定——它是释放动作的安全底线，必须和回滚在同一个事务里，否则
	// 写时复制（fileHandle.Close 把旧版本未触及的切片继承成新版本）可能正好在
	// 这中间把某个 chunk 重新引用上。
	//
	// 为什么由池层来做（它会读 Share 层的 version_chunks 表）：释放是这里独有的
	// 能力，引用检查是它不可分割的前提；散在每个调用点去写，早晚会漏一处。
	ids, err := filterUnreferenced(tx, ids)
	if err != nil {
		return ReleaseResult{}, err
	}
	if len(ids) == 0 {
		return ReleaseResult{}, nil
	}

	res := ReleaseResult{Stale: make([]StaleFile, 0, len(ids))}
	stripes := make(map[int64]struct{}, len(ids))

	for start := 0; start < len(ids); start += releaseBatchSize {
		end := start + releaseBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[start:end]

		// 1. 只挑已写入的 data chunk：
		//    - type 过滤挡住 parity：它永远不出现在 version_chunks 里，只按
		//      "无引用"判断会把它一起误放；
		//    - status 过滤保证幂等（已回滚过的行不再处理）。
		var chunks []db.Chunk
		if err := tx.Where("id IN ? AND type = ? AND status = ?",
			batch, db.DataChunk, db.ChunkAllocated).Find(&chunks).Error; err != nil {
			return ReleaseResult{}, errs.DBQuery(err)
		}
		if len(chunks) == 0 {
			continue
		}

		// 2. 顺手换一个新路径：引用点从旧文件上挪开之后，第 4 步删旧文件就
		//    不可能误删"刚被并发写入复用此槽位"写进去的新数据。
		paths, err := generateChunkPaths(len(chunks))
		if err != nil {
			return ReleaseResult{}, errs.FromError(err, errs.ECODE_CRYPTO_ERROR, errs.ESTR_CRYPTO_ERROR)
		}
		for i := range chunks {
			res.Stale = append(res.Stale, StaleFile{diskId: chunks[i].DiskId, path: chunks[i].Path})
			res.Chunks++
			res.Bytes += chunks[i].Size
			stripes[chunks[i].StripeId] = struct{}{}
			chunks[i].Status = db.ChunkReserved
			chunks[i].Size = 0
			chunks[i].Hash = nil
			chunks[i].Path = paths[i]
		}
		// 3. 批量回滚槽位。这里用 AssignmentColumns 而不是 Updates(struct)：
		//    GORM 会跳过结构体里的零值，size=0 / hash=NULL 根本写不进去。
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "id"}},
			DoUpdates: clause.AssignmentColumns([]string{"status", "size", "hash", "path"}),
		}).Create(&chunks).Error; err != nil {
			return ReleaseResult{}, errs.DBQuery(err)
		}

		// 4. 清掉依附状态：读缓存（记录 + 缓存副本文件）与写队列残留。
		//    写队列留着的话，Flush 之后仍会按这些 chunk_id 搬运任务。
		cacheFiles, err := dropCacheRows(tx, batch)
		if err != nil {
			return ReleaseResult{}, err
		}
		res.Stale = append(res.Stale, cacheFiles...)
		if err := tx.Where("chunk_id IN ?", batch).Delete(&db.WriteQueue{}).Error; err != nil {
			return ReleaseResult{}, errs.DBQuery(err)
		}
	}

	parityStale, err := p.requeueParity(tx, stripes)
	if err != nil {
		return ReleaseResult{}, err
	}
	res.Stale = append(res.Stale, parityStale...)
	return res, nil
}

// DeleteStaleFiles 并发删除这些落点上的文件。失败只记日志——它们已经不被
// 任何元数据引用，删不掉只是留下垃圾，不该让一次彻底删除因此失败。
func (p *PoolManager) DeleteStaleFiles(stale []StaleFile) {
	if len(stale) == 0 {
		return
	}

	diskIds := make([]int64, len(stale))
	for i, s := range stale {
		diskIds[i] = s.diskId
	}
	diskById, err := loadDisksByIds(p.DbManager.DB, uniqueIds(diskIds))
	if err != nil {
		log.Printf("release: load disks failed: %v", err)
		return
	}

	deleteErrs := parallelEach(len(stale), 0, func(i int) error {
		s := stale[i]
		disk, ok := diskById[s.diskId]
		if !ok {
			return fmt.Errorf("disk %d not found", s.diskId)
		}
		h := p.handlerFor(disk.Backend)
		if h == nil {
			return fmt.Errorf("no handler for backend %d", disk.Backend)
		}
		return h.Delete(disk, s.path)
	})

	failed := 0
	for i, err := range deleteErrs {
		if err == nil {
			continue
		}
		failed++
		if failed <= maxStaleLogLines {
			log.Printf("release: delete %s on disk %d failed: %v", stale[i].path, stale[i].diskId, err)
		}
	}
	if failed > 0 {
		log.Printf("release: %d/%d stale file(s) not deleted (left as garbage)", failed, len(stale))
	}
}

// maxStaleLogLines 单次释放最多逐条打印多少个失败，避免几万个残留把日志刷爆。
const maxStaleLogLines = 5

// ---------------------------------------------------------------
// 内部
// ---------------------------------------------------------------

// dropCacheRows 在事务里删掉这批 chunk 的读缓存记录，返回需要在事务提交后
// 删除的缓存副本文件。
//
// 缓存是数据的副本：记录必须随事务一起消失（否则复用槽位后读会命中旧内容），
// 而缓存文件的删除失败只是垃圾。
func dropCacheRows(tx *gorm.DB, chunkIds []int64) ([]StaleFile, error) {
	var entries []db.ReadCache
	if err := tx.Where("chunk_id IN ?", chunkIds).Find(&entries).Error; err != nil {
		return nil, errs.DBQuery(err)
	}
	if len(entries) == 0 {
		return nil, nil
	}

	ids := make([]int64, 0, len(entries))
	files := make([]StaleFile, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.Id)
		if e.Status == db.Cached && e.Path != "" {
			files = append(files, StaleFile{diskId: e.DiskId, path: e.Path})
		}
	}
	if err := tx.Where("id IN ?", ids).Delete(&db.ReadCache{}).Error; err != nil {
		return nil, errs.DBQuery(err)
	}
	return files, nil
}

// filterUnreferenced 从候选里挑出"已经没有任何 version_chunk 引用"的 chunk。
//
// 必须在同一个事务里、且调用方已经删掉自己那些 version_chunks 之后执行：那时
// 剩下的引用只可能来自别的版本（同一文件的旧版本、别的文件，甚至别的 Share）。
// 查询覆盖整张表，所以任何一处引用都能挡住释放。
func filterUnreferenced(tx *gorm.DB, chunkIds []int64) ([]int64, error) {
	if len(chunkIds) == 0 {
		return nil, nil
	}
	referenced := make(map[int64]struct{}, len(chunkIds))
	for start := 0; start < len(chunkIds); start += releaseBatchSize {
		end := start + releaseBatchSize
		if end > len(chunkIds) {
			end = len(chunkIds)
		}
		var got []int64
		if err := tx.Model(&db.VersionChunk{}).Where("chunk_id IN ?", chunkIds[start:end]).
			Distinct("chunk_id").Pluck("chunk_id", &got).Error; err != nil {
			return nil, errs.DBQuery(err)
		}
		for _, id := range got {
			referenced[id] = struct{}{}
		}
	}
	out := make([]int64, 0, len(chunkIds))
	for _, id := range chunkIds {
		if _, ok := referenced[id]; ok {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}

// requeueParity 处理本次释放涉及条带的 parity，返回需要物理删除的 parity 文件。
//
// 两种情况分开处理，取舍依据是"恢复数据比删除数据重要"：
//
//   - 条带里还有其它已写入的 data chunk：parity 原样保留（任何时刻都有校验
//     可读，盘故障时仍能重建），只投递一次重算。必须重算——槽位释放后 parity
//     已与现存数据不一致，拿陈旧的它去重建会解出错误的旧数据且不留痕迹。
//   - 条带里已经没有任何已写入的 data（整条被释放空）：没有数据可保护，parity
//     文件直接删掉、槽位回滚。这里不能指望后台重算：calculateStripeParity 会
//     跳过没有 data 的条带，那样旧 parity 会把被删内容的痕迹永久留在盘上。
func (p *PoolManager) requeueParity(tx *gorm.DB, stripes map[int64]struct{}) ([]StaleFile, error) {
	if len(stripes) == 0 {
		return nil, nil
	}
	stripeIds := make([]int64, 0, len(stripes))
	for id := range stripes {
		stripeIds = append(stripeIds, id)
	}

	// 还有已写入 data 的条带
	liveSet := make(map[int64]struct{}, len(stripeIds))
	for start := 0; start < len(stripeIds); start += releaseBatchSize {
		end := start + releaseBatchSize
		if end > len(stripeIds) {
			end = len(stripeIds)
		}
		var live []int64
		if err := tx.Model(&db.Chunk{}).Distinct("stripe_id").
			Where("stripe_id IN ? AND type = ? AND status = ?",
				stripeIds[start:end], db.DataChunk, db.ChunkAllocated).
			Pluck("stripe_id", &live).Error; err != nil {
			return nil, errs.DBQuery(err)
		}
		for _, id := range live {
			liveSet[id] = struct{}{}
		}
	}

	// 已空置的条带：删掉 parity 文件并回滚槽位（path 不变，将来重算会覆盖写回）
	var empty []int64
	for _, id := range stripeIds {
		if _, ok := liveSet[id]; !ok {
			empty = append(empty, id)
		}
	}
	var stale []StaleFile
	for start := 0; start < len(empty); start += releaseBatchSize {
		end := start + releaseBatchSize
		if end > len(empty) {
			end = len(empty)
		}
		var parities []db.Chunk
		if err := tx.Where("stripe_id IN ? AND type = ?", empty[start:end], db.ParityChunk).
			Find(&parities).Error; err != nil {
			return nil, errs.DBQuery(err)
		}
		pending := make([]db.Chunk, 0, len(parities))
		for i := range parities {
			if parities[i].Status == db.ChunkReserved {
				continue // 本来就没算过 parity，没有文件要删
			}
			stale = append(stale, StaleFile{diskId: parities[i].DiskId, path: parities[i].Path})
			parities[i].Status = db.ChunkReserved
			parities[i].Size = 0
			parities[i].Hash = nil
			pending = append(pending, parities[i])
		}
		if len(pending) == 0 {
			continue
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "id"}},
			DoUpdates: clause.AssignmentColumns([]string{"status", "size", "hash"}),
		}).Create(&pending).Error; err != nil {
			return nil, errs.DBQuery(err)
		}
	}

	// 仍有数据的条带：投递 parity 重算（队列里已有待处理任务的不重复投）
	if len(liveSet) > 0 {
		live := make([]int64, 0, len(liveSet))
		for id := range liveSet {
			live = append(live, id)
		}
		if err := p.enqueueParityRecompute(tx, live); err != nil {
			return nil, err
		}
	}
	return stale, nil
}

// enqueueParityRecompute 给这批条带投递 parity 重算任务（去重）。
//
// 这是 Flush 之外的第二个 stripe_queues 写入点：Flush 覆盖"有新数据写入"，
// 这里覆盖"槽位被释放、parity 需要跟着变"。
func (p *PoolManager) enqueueParityRecompute(tx *gorm.DB, stripeIds []int64) error {
	var existing []int64
	for start := 0; start < len(stripeIds); start += releaseBatchSize {
		end := start + releaseBatchSize
		if end > len(stripeIds) {
			end = len(stripeIds)
		}
		if err := tx.Model(&db.StripeQueue{}).
			Where("stripe_id IN ? AND type = ? AND status IN ?",
				stripeIds[start:end], db.StripeQueueParity,
				[]db.TaskStatus{db.TaskPending, db.TaskRunning}).
			Pluck("stripe_id", &existing).Error; err != nil {
			return errs.DBQuery(err)
		}
	}
	seen := make(map[int64]struct{}, len(existing))
	for _, id := range existing {
		seen[id] = struct{}{}
	}

	batch := make([]db.StripeQueue, 0, len(stripeIds))
	for _, id := range stripeIds {
		if _, ok := seen[id]; ok {
			continue
		}
		batch = append(batch, db.StripeQueue{
			StripeId: id,
			Type:     db.StripeQueueParity,
			Status:   db.TaskPending,
		})
	}
	if len(batch) == 0 {
		return nil
	}
	if err := tx.Create(&batch).Error; err != nil {
		return errs.DBQuery(err)
	}
	return nil
}
