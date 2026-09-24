package pool

import (
	"context"
	"log"
	"time"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
	"gorm.io/gorm"
)

const (
	// orphanGracePeriod 是"刚写好的分片"的保护期。
	//
	// 写入路径是"分配槽位 → 落盘 → 写 version_chunks 引用"，中间那一小段分片
	// 天然无引用。引用其实紧接着就落库（putSlice 会同步 upsert），窗口极小，
	// 但扫描无法区分"真孤儿"和"刚写进盘、引用还没落库"，所以必须留这道兜底。
	orphanGracePeriod = 10 * time.Minute

	// gcBatchLimit 是单轮回收的分片数上限，避免一次事务太长
	// （DbManager.Tx 有 15 秒超时）。
	gcBatchLimit = 5000

	// gcInterval 是后台孤儿回收的轮询间隔。
	gcInterval = 30 * time.Minute

	// gcFirstRunDelay 是后台孤儿回收首轮之前等的时间。
	// 只影响"多久看到效果"，不影响正确性——保护期由查询条件保证。
	gcFirstRunDelay = 1 * time.Minute
)

// GCResult 是一次孤儿回收的结果。
type GCResult struct {
	Scanned    int   // 候选（无引用且过了保护期）分片数
	Released   int   // 实际回滚成空槽的分片数
	FreedBytes int64 // 释放出来的数据字节数
}

// GarbageCollect 回收孤儿分片：没有任何版本引用、且已过保护期的 data 分片。
//
// poolId > 0 时只扫这个池，否则扫全库。回收动作与彻底删除共用同一个内核
// （ReleaseChunks）：回滚空槽、清读缓存与写队列、按条带处理 parity、删磁盘文件。
//
// 与 Purge 的区别只在候选从哪来：Purge 的候选来自刚删掉的版本，这里靠全库扫描
// ——这也是它存在的意义：历史 Purge、删除 Share 留下的存量孤儿只有扫描能捞回来。
func (p *PoolManager) GarbageCollect(poolId int64, limit int) (*GCResult, error) {
	if limit <= 0 || limit > gcBatchLimit {
		limit = gcBatchLimit
	}

	var ids []int64
	if err := p.orphanQuery(poolId).Limit(limit).Pluck("chunks.id", &ids).Error; err != nil {
		return nil, errs.DBQuery(err)
	}
	res := &GCResult{Scanned: len(ids)}
	if len(ids) == 0 {
		return res, nil
	}

	// 候选在事务外选出，事务内 ReleaseChunks 会再确认一次引用，
	// 所以即使这中间有版本把某个分片引用回来，也不会被误放。
	var released ReleaseResult
	err := p.DbManager.Tx(func(tx *gorm.DB) error {
		var err error
		released, err = p.ReleaseChunks(tx, ids)
		return err
	})
	if err != nil {
		return nil, err
	}
	p.DeleteStaleFiles(released.Stale)

	res.Released = released.Chunks
	res.FreedBytes = released.Bytes
	if res.Released > 0 {
		log.Printf("gc: released %d orphan chunk(s) (%d bytes)", res.Released, res.FreedBytes)
	}
	return res, nil
}

// OrphanStats 报告当前可回收的孤儿分片数与字节数（与 GC 用同一套判据，因此
// 同样排除了保护期内的分片与写队列里还有记录的分片）。
func (p *PoolManager) OrphanStats(poolId int64) (int64, int64, error) {
	var row struct {
		Chunks int64
		Bytes  int64
	}
	err := p.orphanQuery(poolId).
		Select("COUNT(*) AS chunks, COALESCE(SUM(chunks.size), 0) AS bytes").
		Scan(&row).Error
	if err != nil {
		return 0, 0, errs.DBQuery(err)
	}
	return row.Chunks, row.Bytes, nil
}

// StartOrphanGC 启动后台孤儿回收：首轮等 gcFirstRunDelay，之后每 gcInterval 扫一轮。
func (p *PoolManager) StartOrphanGC(ctx context.Context) {
	go func() {
		wait := gcFirstRunDelay
		for {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			if _, err := p.GarbageCollect(0, gcBatchLimit); err != nil {
				log.Printf("gc: orphan collection failed: %v", err)
			}
			wait = gcInterval
		}
	}()
	log.Printf("orphan gc started (interval=%s, grace=%s)", gcInterval, orphanGracePeriod)
}

// orphanQuery 是孤儿判定的唯一来源，回收与统计都用它，避免两处判据漂移。
//
// "孤儿"= 没有任何 version_chunks 引用、已写入、且过了保护期的 data 分片。
// 三个限定条件各有原因：
//   - type=data：parity 永远不出现在 version_chunks 里，只看引用会把它全判成孤儿；
//   - created_at < cutoff：避开"已落盘但引用记录还没写"的瞬间；
//   - 排除 write_queues 里还有记录的分片：那是写入意图/结果（WAL），可能正在写。
func (p *PoolManager) orphanQuery(poolId int64) *gorm.DB {
	cutoff := time.Now().Unix() - int64(orphanGracePeriod.Seconds())
	q := p.DbManager.DB.Table("chunks").
		Joins("LEFT JOIN version_chunks vc ON vc.chunk_id = chunks.id").
		Where("chunks.type = ? AND chunks.status = ?", db.DataChunk, db.ChunkAllocated).
		Where("vc.chunk_id IS NULL").
		Where("chunks.created_at < ?", cutoff).
		Where("NOT EXISTS (SELECT 1 FROM write_queues w WHERE w.chunk_id = chunks.id)")
	if poolId > 0 {
		q = q.Where("chunks.pool_id = ?", poolId)
	}
	return q
}
