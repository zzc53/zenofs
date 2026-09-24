package vfs

import (
	"context"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
)

// UsageBreakdown 按"这份数据还能不能被用户找到"把这个 Share 的占用拆成三类。
//
// 口径是**去重后的 chunk 实际大小**，不是 SUM(versions.size)：写时复制让同一
// 文件的多个版本共享同一批 chunk，按版本大小累加会把同一份物理数据重复计算。
//
// 每个 chunk 只落进一类，按优先级判定：
//   - Current：被某个存活文件的**当前**版本引用（真正在用的数据）
//   - History：只被存活文件的历史版本引用（回滚还能用到）
//   - Recycle：只被回收站里的文件引用（清空回收站就能回收）
//
// 三类之外还有"谁都不引用"的孤儿分片：它已经无法归属到任何 Share，只能在
// 池级别统计与回收（见 pool.GarbageCollect）。
type UsageBreakdown struct {
	CurrentBytes int64
	HistoryBytes int64
	RecycleBytes int64
}

// shareUsageQuery 用一个子查询给每个 chunk 定类（MIN 取优先级最高的那一类），
// 再按类别汇总大小。三张关联表都走主键/索引，跨 SQLite/MySQL/PostgreSQL 都是
// 同一套语法。
const shareUsageQuery = `
SELECT
  COALESCE(SUM(CASE WHEN cls = 1 THEN size ELSE 0 END), 0) AS current_bytes,
  COALESCE(SUM(CASE WHEN cls = 2 THEN size ELSE 0 END), 0) AS history_bytes,
  COALESCE(SUM(CASE WHEN cls = 3 THEN size ELSE 0 END), 0) AS recycle_bytes
FROM (
  SELECT c.id AS id, c.size AS size,
    MIN(CASE
      WHEN i.deleted = 0 AND i.version_id = vc.version_id THEN 1
      WHEN i.deleted = 0 THEN 2
      ELSE 3
    END) AS cls
  FROM chunks c
  JOIN version_chunks vc ON vc.chunk_id = c.id
  JOIN versions v ON v.id = vc.version_id
  JOIN inodes i ON i.id = v.inode_id
  WHERE i.share_id = ? AND c.type = ?
  GROUP BY c.id, c.size
) t`

// UsageBreakdown 统计这个 Share 三类占用。任何挂载点（含只读）都能查。
func (fs *ShareFS) UsageBreakdown(_ context.Context) (UsageBreakdown, error) {
	var row UsageBreakdown
	err := fs.pm.DbManager.DB.Raw(shareUsageQuery, fs.share.Id, db.DataChunk).Scan(&row).Error
	if err != nil {
		return UsageBreakdown{}, errs.DBQuery(err)
	}
	return row, nil
}
