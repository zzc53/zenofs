package pool

import (
	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
)

// PoolUsage 是一个存储池的真实占用，全部按分片在盘上的实际大小统计
// （与 Share 配额那套"当前版本大小之和"无关）。
type PoolUsage struct {
	DataBytes   int64 // 已写入的 data 分片大小之和
	ParityBytes int64 // parity 分片大小之和
	UsedSlots   int64 // 已占用的槽位（data + parity）
	FreeSlots   int64 // 预分配的空槽，后续写入可直接复用
}

// Usage 统计这个池的真实占用。池不存在时返回与 GetPool 一致的错误。
func (p *PoolManager) Usage(poolId int64) (*PoolUsage, error) {
	if _, err := p.GetPool(poolId); err != nil {
		return nil, err
	}

	var row PoolUsage
	err := p.DbManager.DB.Table("chunks").
		Select(`COALESCE(SUM(CASE WHEN type = ? AND status = ? THEN size ELSE 0 END), 0) AS data_bytes,
		        COALESCE(SUM(CASE WHEN type = ? AND status = ? THEN size ELSE 0 END), 0) AS parity_bytes,
		        COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0) AS used_slots,
		        COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0) AS free_slots`,
			db.DataChunk, db.ChunkAllocated,
			db.ParityChunk, db.ChunkAllocated,
			db.ChunkAllocated, db.ChunkReserved).
		Where("pool_id = ?", poolId).
		Scan(&row).Error
	if err != nil {
		return nil, errs.DBQuery(err)
	}
	return &row, nil
}
