package pool

import (
	"encoding/binary"
	"fmt"
	"path"
	"strconv"
	"time"

	cryptorand "crypto/rand"
	mrand "math/rand"

	"github.com/zeebo/blake3"
	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type ChunkData struct {
	db.Chunk
	Data []byte
}

// ---------------------------------------------------------------
// 工具函数
// ---------------------------------------------------------------

// generateSecureRandomString 生成一个指定长度的安全随机字符串。
// 字符集为 base32 变体（不含易混淆字符如 0/O/1/l）。
// 用于生成 chunk 文件路径中的随机后缀。
func generateSecureRandomString(length int) (string, error) {
	const charset = "abcdefghijklmnopqrstuvwxyz234567"
	b := make([]byte, length)

	_, err := cryptorand.Read(b)
	if err != nil {
		return "", err
	}

	for i := range b {
		b[i] = charset[int(b[i])%len(charset)]
	}
	return string(b), nil
}

// cryptoRandSeed 用 crypto/rand 生成一个 int64 随机种子。
// 用于磁盘 shuffle 操作，确保各盘写入分布均匀。
func cryptoRandSeed() int64 {
	var buf [8]byte
	cryptorand.Read(buf[:])
	return int64(binary.LittleEndian.Uint64(buf[:]))
}

// ---------------------------------------------------------------
// 步骤 2：预分配 chunk 槽位
// ---------------------------------------------------------------

// getNewChunks 为 items 分配 chunk 槽位（AddChunks 的步骤 2：预分配）。
//
// 三个 Phase 在同一个事务中完成，尽量把 DB 往返压到最少：
//
//	Phase 1 — 复用已有的 Reserved data chunk
//	  一次查询取出该 pool 中状态为 Reserved 的 data chunk slot（SKIP LOCKED 防止并发争抢），
//	  优先填满，复用预留槽位、减少 stripe 碎片。
//
//	Phase 2 — 不够则新建 stripe
//	  一次性批量创建所需的全部 stripe 行，再一次性批量创建对应的全部 chunk 行
//	  （data + parity），磁盘通过 shuffle 均匀分布，路径一次批量生成。
//
//	Phase 3 — 激活复用的 Reserved chunk
//	  对 Phase 1 取到的 slot 补上 size / hash 并置为 Allocated，
//	  通过一次 upsert（INSERT ... ON CONFLICT(id) DO UPDATE）批量写回。
//
// 返回的 chunks 顺序与 items 一一对应（reserved 在前、新建在后），
// 每条都已带自增 Id，可直接用于后续入队与写盘。
func (p *PoolManager) getNewChunks(poolId int64, items []ChunkData) ([]db.Chunk, error) {
	N := len(items)

	// ---------------------------------------------------------------
	// 校验 pool 是否存在且在线
	// ---------------------------------------------------------------
	pool, err := p.GetPool(poolId)
	if err != nil {
		return nil, err
	}
	if pool.Status != db.Online {
		return nil, errs.New(errs.ECODE_POOL_OFFLINE, errs.ESTR_POOL_OFFLINE, "pool is offline", strconv.FormatInt(poolId, 10))
	}

	// ---------------------------------------------------------------
	// 查询 pool 中所有在线的 data 盘，Phase 2 建新 stripe 时会用到。
	// ---------------------------------------------------------------
	var disks []db.Disk
	if err := p.DbManager.DB.Where("pool_id = ? AND status = ? AND type = ?",
		poolId, db.Online, db.DataDisk).Find(&disks).Error; err != nil {
		return nil, errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
	}

	// ---------------------------------------------------------------
	// 校验每个 chunk 大小不超过 pool 设定的 chunk size 上限
	// ---------------------------------------------------------------
	maxSize := pool.ChunkSize * 1024
	for _, it := range items {
		if int64(it.Size) > maxSize {
			return nil, errs.New(errs.ECODE_CHUNK_SIZE_EXCEED, errs.ESTR_CHUNK_SIZE_EXCEED,
				"chunk exceeds pool chunk size", strconv.FormatInt(maxSize, 10))
		}
	}

	var chunks []db.Chunk

	err = p.DbManager.DB.Transaction(func(tx *gorm.DB) error {
		// ---------------------------------------------------------------
		// Phase 1: 消费该 pool 中已预分配的 Reserved data chunks。
		// 使用 SKIP LOCKED 防止并发写入时锁冲突（各自跳过已锁行）。
		// ---------------------------------------------------------------
		var reserved []db.Chunk
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Model(&db.Chunk{}).
			Where("status = ? AND type = ? AND pool_id = ?", db.ChunkReserved, db.DataChunk, poolId).
			Limit(N).
			Find(&reserved).Error; err != nil {
			return errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
		}

		need := N - len(reserved)

		// ---------------------------------------------------------------
		// Phase 2: 如果 Phase 1 不够用，批量新建 stripe 及其 chunk。
		//
		// 每个 stripe 包含 dataShards 个 data slot + parityShards 个 parity slot。
		// 本次实际写入的 data chunk 直接填好 size / hash 并置为 Allocated；
		// 其余 slot 保持 Reserved 供后续批次复用。
		// ---------------------------------------------------------------
		newChunks := make([]db.Chunk, 0, need)
		if need > 0 {
			dataShards := int(pool.DataShards)
			parityShards := int(pool.ParityShards)
			totalSlots := dataShards + parityShards
			if totalSlots <= 0 {
				return errs.New(errs.ECODE_POOL_BAD, errs.ESTR_POOL_BAD, "invalid stripe capacity",
					fmt.Sprintf("data_shards=%d, parity_shards=%d", pool.DataShards, pool.ParityShards))
			}
			if len(disks) != totalSlots {
				return errs.New(errs.ECODE_DISK_OFFLINE, errs.ESTR_DISK_OFFLINE,
					"not all disks online for stripe allocation",
					fmt.Sprintf("online=%d, need=%d", len(disks), totalSlots))
			}

			// 一次算出需要多少个 stripe
			numStripes := (need + dataShards - 1) / dataShards

			// 批量创建 stripe 行（GORM 会回填自增 Id）
			newStripes := make([]db.Stripe, numStripes)
			for i := range newStripes {
				newStripes[i] = db.Stripe{PoolId: poolId}
			}
			if err := tx.Create(&newStripes).Error; err != nil {
				return errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
			}

			// 一次性生成所有新 chunk 的路径
			paths, err := generateChunkPaths(numStripes * totalSlots)
			if err != nil {
				return errs.FromError(err, errs.ECODE_CRYPTO_ERROR, errs.ESTR_CRYPTO_ERROR)
			}

			// 组装所有新 chunk 行
			allNew := make([]db.Chunk, 0, numStripes*totalSlots)
			remaining := need
			newIdx := 0 // 新建部分在 items 中的偏移（从 len(reserved) 起）
			for s := 0; s < numStripes; s++ {
				batch := dataShards
				if remaining < dataShards {
					batch = remaining
				}
				// 对 disks 进行 shuffle，将 chunk 均匀分布到各磁盘
				shuffled := append([]db.Disk{}, disks...)
				mrand.New(mrand.NewSource(cryptoRandSeed())).Shuffle(len(shuffled), func(i, j int) {
					shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
				})
				for i := 0; i < totalSlots; i++ {
					typ := db.ParityChunk
					if i < dataShards {
						typ = db.DataChunk
					}
					status := db.ChunkReserved
					var size int64
					var hash []byte
					if i < batch {
						// 本次 batch 中实际写入的 data chunk，直接填数据
						status = db.ChunkAllocated
						itemIdx := len(reserved) + newIdx + i
						size = int64(items[itemIdx].Size)
						hash = items[itemIdx].Hash
					}
					idx := int64(i)
					if i >= dataShards {
						idx = int64(i - dataShards) // parity 独立编号从 0 开始
					}
					allNew = append(allNew, db.Chunk{
						Status:   status,
						Path:     paths[len(allNew)],
						DiskId:   shuffled[i].Id,
						StripeId: newStripes[s].Id,
						PoolId:   poolId,
						Size:     size,
						Hash:     hash,
						Type:     typ,
						Index:    idx,
					})
				}
				remaining -= batch
				newIdx += batch
			}
			if err := tx.Create(&allNew).Error; err != nil {
				return errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
			}

			// 收集本次实际分配到的 data chunk（按 items 顺序）
			for i := range allNew {
				if allNew[i].Type == db.DataChunk && allNew[i].Status == db.ChunkAllocated {
					newChunks = append(newChunks, allNew[i])
				}
			}
		}

		// ---------------------------------------------------------------
		// Phase 3: 激活 Phase 1 复用的 Reserved chunks。
		// 一次 upsert 批量补上 size / hash 并置为 Allocated。
		// ---------------------------------------------------------------
		if len(reserved) > 0 {
			for i := range reserved {
				reserved[i].Status = db.ChunkAllocated
				reserved[i].Size = int64(items[i].Size)
				reserved[i].Hash = items[i].Hash
			}
			if err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "id"}},
				DoUpdates: clause.AssignmentColumns([]string{"status", "size", "hash"}),
			}).Create(&reserved).Error; err != nil {
				return errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
			}
		}

		// 组装最终结果：reserved 在前，new 在后，保持与 dataList 一致的顺序
		chunks = make([]db.Chunk, 0, N)
		chunks = append(chunks, reserved...)
		chunks = append(chunks, newChunks...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return chunks, nil
}

// ---------------------------------------------------------------
// 步骤 3 / 5：write queue 入队
// ---------------------------------------------------------------

// createWriteQueue 批量插入 write queue 条目。
func (p *PoolManager) createWriteQueue(entries []db.WriteQueue) error {
	if err := p.DbManager.DB.Create(&entries).Error; err != nil {
		return errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
	}
	return nil
}

// enqueueWriteIntents 批量记录写入意图（步骤 3，TaskPending）。
// 作为 WAL 使用：进程崩溃后残留的 Pending 条目可用于恢复重写。
func (p *PoolManager) enqueueWriteIntents(chunks []db.Chunk) error {
	if len(chunks) == 0 {
		return nil
	}
	entries := make([]db.WriteQueue, len(chunks))
	for i, c := range chunks {
		entries[i] = db.WriteQueue{
			ChunkId:  c.Id,
			StripeId: c.StripeId,
			Status:   db.TaskPending,
		}
	}
	return p.createWriteQueue(entries)
}

// enqueueWriteResults 批量记录写盘结果（步骤 5）。
// writeErrs[i] 为 chunks[i] 的写盘错误，为 nil 记 TaskSuccess，否则记 TaskFail。
// TaskSuccess 会驱动 parity worker 计算 RS 校验。
func (p *PoolManager) enqueueWriteResults(chunks []db.Chunk, writeErrs []error) error {
	if len(chunks) == 0 {
		return nil
	}
	entries := make([]db.WriteQueue, len(chunks))
	for i, c := range chunks {
		status := db.TaskSuccess
		if writeErrs[i] != nil {
			status = db.TaskFail
		}
		entries[i] = db.WriteQueue{
			ChunkId:  c.Id,
			StripeId: c.StripeId,
			Status:   status,
		}
	}
	return p.createWriteQueue(entries)
}

// ---------------------------------------------------------------
// 步骤 4：批量写盘
// ---------------------------------------------------------------

// writeChunkFiles 并发把每个 chunk 的数据写到其所在磁盘。
// data[i] 对应 chunks[i]。返回与 chunks 等长的错误切片（成功为 nil）以及其中一个错误。
func (p *PoolManager) writeChunkFiles(chunks []db.Chunk, data [][]byte, diskById map[int64]db.Disk) ([]error, error) {
	writeErrs := parallelEach(len(chunks), 0, func(i int) error {
		chk := chunks[i]
		disk, ok := diskById[chk.DiskId]
		if !ok {
			return errs.New(errs.ECODE_DISK_OFFLINE, errs.ESTR_DISK_OFFLINE,
				"disk not found", strconv.FormatInt(chk.DiskId, 10))
		}
		h := p.handlerFor(disk.Backend)
		if h == nil {
			return errs.New(errs.ECODE_FILE_WRITE, errs.ESTR_FILE_WRITE,
				"no handler for backend", strconv.Itoa(int(disk.Backend)))
		}
		if err := h.Write(disk, chk.Path, data[i]); err != nil {
			return errs.FromError(err, errs.ECODE_FILE_WRITE, errs.ESTR_FILE_WRITE)
		}
		return nil
	})
	return writeErrs, firstErr(writeErrs)
}

// ---------------------------------------------------------------
// AddChunk / AddChunks
// ---------------------------------------------------------------

// AddChunk 兼容包装：单 chunk 写入。
// 委托给 AddChunks 处理，返回第一个（也是唯一一个）chunk。
func (p *PoolManager) AddChunk(poolId int64, bytes []byte) (*db.Chunk, error) {
	chunks, err := p.AddChunks(poolId, [][]byte{bytes})
	if err != nil {
		return nil, err
	}
	return &chunks[0], nil
}

// AddChunks 批量写入 chunks，支持任意数量（1 ≤ N）。
//
// 入参 poolId 与数据列表，出参为分配到的 chunk 元数据（顺序与 dataList 一致）。
//
// 流程：
//
//	步骤 1 — 计算 size 和 hash
//	  在事务外对每个 chunk 计算 BLAKE3 哈希与大小（纯 CPU，不占 DB）。
//
//	步骤 2 — 预分配 chunks
//	  从该 pool 中未分配的 Reserved data chunk 取用；不够则新建 stripe 和对应的
//	  chunk 再取用。整段在一个事务里批量完成（见 getNewChunks），DB 往返最少。
//
//	步骤 3 — 批量插入 write queue（Pending）
//	  作为写入意图（WAL）记录，崩溃后可用于恢复重写。
//
//	步骤 4 — 批量写入文件
//	  并发将数据写到各自磁盘。
//
//	步骤 5 — 批量插入 write queue（Success / Fail）
//	  写盘结果记录；Success 会驱动 parity worker 计算 RS 校验。
//
// 写盘全部成功时返回 (chunks, nil)；存在失败时返回 (nil, 首个错误)。
// 失败的 chunk 元数据保持已分配状态不动，由上层依据 Fail 记录重试。
func (p *PoolManager) AddChunks(poolId int64, dataList [][]byte) ([]db.Chunk, error) {
	// ---------------------------------------------------------------
	// 前置校验：输入不能为空
	// ---------------------------------------------------------------
	if len(dataList) == 0 {
		return nil, errs.New(errs.ECODE_CHUNK_EMPTY, errs.ESTR_CHUNK_EMPTY, "empty data list", "")
	}
	for i, data := range dataList {
		if len(data) == 0 {
			return nil, errs.New(errs.ECODE_CHUNK_EMPTY, errs.ESTR_CHUNK_EMPTY, "empty data", strconv.Itoa(i))
		}
	}

	N := len(dataList)

	// ---------------------------------------------------------------
	// 步骤 1: 计算 size 和 hash（事务外）
	// ---------------------------------------------------------------
	items := make([]ChunkData, N)
	for i, data := range dataList {
		hash := blake3.Sum256(data)
		items[i] = ChunkData{
			Data: data,
			Chunk: db.Chunk{
				Hash: hash[:],
				Size: int64(len(data)),
			},
		}
	}

	// 预加载该 pool 下所有磁盘信息（写文件时按 disk.Backend 匹配 Writer）
	diskById, err := p.getDiskMapByPoolId(poolId)
	if err != nil {
		return nil, err
	}

	// ---------------------------------------------------------------
	// 步骤 2: 预分配 chunks
	// ---------------------------------------------------------------
	chunks, err := p.getNewChunks(poolId, items)
	if err != nil {
		return nil, err
	}

	// ---------------------------------------------------------------
	// 步骤 3: 记录写入意图（write queue, Pending）
	// ---------------------------------------------------------------
	if err := p.enqueueWriteIntents(chunks); err != nil {
		return nil, err
	}

	// ---------------------------------------------------------------
	// 步骤 4: 批量写入文件
	// ---------------------------------------------------------------
	writeErrs, firstErr := p.writeChunkFiles(chunks, dataList, diskById)

	// ---------------------------------------------------------------
	// 步骤 5: 批量插入 write queue（Success / Fail）
	// ---------------------------------------------------------------
	if err := p.enqueueWriteResults(chunks, writeErrs); err != nil {
		return nil, err
	}

	if firstErr != nil {
		return nil, firstErr
	}
	return chunks, nil
}

// generateChunkPaths 生成 chunk 文件在磁盘上的相对路径。
// 按时分秒分层目录组织，末尾加 8 位随机字符串防冲突。
// 格式: YYYY/MM/DD/HH/MM/<random8>
func generateChunkPaths(count int) ([]string, error) {
	dateStr := time.Now().Format("2006/01/02/15/04")
	stringArr := make([]string, count)
	for i := range stringArr {
		randomStr, err := generateSecureRandomString(11)
		if err != nil {
			return nil, err
		}
		suffix := randomStr[0:8] + "." + randomStr[8:10]
		stringArr[i] = path.Join(dateStr, suffix)
	}
	return stringArr, nil
}

// ---------------------------------------------------------------
// WriteChunks
// ---------------------------------------------------------------

// WriteChunkItem 指定待写入的 chunk ID 和对应的数据。
type WriteChunkItem struct {
	ChunkId int64
	Data    []byte
}

// WriteChunks 向已分配的 chunk（通过 ChunkId）写入数据。
//
// 流程与 AddChunks 保持一致：
//  1. 计算 size 和 hash
//  2. 查出 chunk 元数据、推导 poolId 并校验；批量更新 chunk 元数据
//  3. 批量插入 write queue（Pending）
//  4. 批量写入文件
//  5. 批量插入 write queue（Success / Fail）
//
// poolId 从 chunk 所属 stripe 自动推导，不跨 pool。
func (p *PoolManager) WriteChunks(items []WriteChunkItem) ([]db.Chunk, error) {
	// ---------------------------------------------------------------
	// 前置校验
	// ---------------------------------------------------------------
	if len(items) == 0 {
		return nil, errs.New(errs.ECODE_CHUNK_EMPTY, errs.ESTR_CHUNK_EMPTY, "empty items", "")
	}

	N := len(items)

	// 步骤 1: 计算 size 和 hash
	prep := make([]ChunkData, N)
	chunkIds := make([]int64, N)
	for i, it := range items {
		if len(it.Data) == 0 {
			return nil, errs.New(errs.ECODE_CHUNK_EMPTY, errs.ESTR_CHUNK_EMPTY, "empty data", strconv.Itoa(i))
		}
		hash := blake3.Sum256(it.Data)
		prep[i] = ChunkData{
			Data: it.Data,
			Chunk: db.Chunk{
				Id:   it.ChunkId,
				Hash: hash[:],
				Size: int64(len(it.Data)),
			},
		}
		chunkIds[i] = it.ChunkId
	}

	// ---------------------------------------------------------------
	// 步骤 2: 一次查出所有 chunk，推导 pool 并校验
	// ---------------------------------------------------------------
	var chunks []db.Chunk
	if err := p.DbManager.DB.Where("id IN ?", chunkIds).Find(&chunks).Error; err != nil {
		return nil, errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
	}
	if len(chunks) != N {
		return nil, errs.New(errs.ECODE_CHUNK_NOT_FOUND, errs.ESTR_CHUNK_NOT_FOUND,
			"some chunks not found", "")
	}

	// 推导 poolId，校验 pool 在线
	var poolId int64
	{
		var stripe db.Stripe
		if err := p.DbManager.DB.First(&stripe, chunks[0].StripeId).Error; err != nil {
			return nil, errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
		}
		poolId = stripe.PoolId
	}
	pool, err := p.GetPool(poolId)
	if err != nil {
		return nil, err
	}
	if pool.Status != db.Online {
		return nil, errs.New(errs.ECODE_POOL_OFFLINE, errs.ESTR_POOL_OFFLINE, "pool is offline", strconv.FormatInt(poolId, 10))
	}

	// 校验数据大小不超过 pool 的 chunk size
	maxSize := pool.ChunkSize * 1024
	for i := range prep {
		if prep[i].Size > maxSize {
			return nil, errs.New(errs.ECODE_CHUNK_SIZE_EXCEED, errs.ESTR_CHUNK_SIZE_EXCEED,
				"data exceeds pool chunk size", strconv.FormatInt(maxSize, 10))
		}
	}

	// 按 chunk ID 建索引保持请求顺序
	chunkById := make(map[int64]*db.Chunk, N)
	for i := range chunks {
		chunkById[chunks[i].Id] = &chunks[i]
	}
	ordered := make([]db.Chunk, N)
	for i := range prep {
		ordered[i] = *chunkById[prep[i].Id]
	}

	// 预加载盘信息（写文件用）
	diskById, err := p.getDiskMapByPoolId(poolId)
	if err != nil {
		return nil, err
	}

	// 批量更新 chunk 元数据（status / size / hash），一次 upsert 完成
	for i := range ordered {
		ordered[i].Status = db.ChunkAllocated
		ordered[i].Size = int64(prep[i].Size)
		ordered[i].Hash = prep[i].Hash
	}
	if err := p.DbManager.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"status", "size", "hash"}),
	}).Create(&ordered).Error; err != nil {
		return nil, errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
	}

	// 步骤 3: 批量插入 write queue（Pending）
	if err := p.enqueueWriteIntents(ordered); err != nil {
		return nil, err
	}

	// 步骤 4: 批量写入文件
	data := make([][]byte, N)
	for i := range prep {
		data[i] = prep[i].Data
	}
	writeErrs, firstErr := p.writeChunkFiles(ordered, data, diskById)

	// 步骤 5: 批量插入 write queue（Success / Fail）
	if err := p.enqueueWriteResults(ordered, writeErrs); err != nil {
		return nil, err
	}

	if firstErr != nil {
		return nil, firstErr
	}
	return ordered, nil
}

// ReadChunks 批量读取 chunk 数据，返回顺序与 chunkIds 一致。
//
// 流程：
//  1. 校验 pool 在线
//  2. 一次查出所有 chunk 元数据，验证属于该 pool
//  3. 按 chunkId 排序保持顺序
//  4. 预加载磁盘信息
//  5. 并发读取（优先尝试缓存，未命中则从源盘读取并异步缓存）
func (p *PoolManager) ReadChunks(poolId int64, chunkIds []int64) ([][]byte, error) {
	if len(chunkIds) == 0 {
		return nil, errs.New(errs.ECODE_CHUNK_EMPTY, errs.ESTR_CHUNK_EMPTY, "empty chunk ids", "")
	}

	// 校验 pool 在线
	pool, err := p.GetPool(poolId)
	if err != nil {
		return nil, err
	}
	if pool.Status != db.Online {
		return nil, errs.New(errs.ECODE_POOL_OFFLINE, errs.ESTR_POOL_OFFLINE, "pool is offline", strconv.FormatInt(poolId, 10))
	}

	N := len(chunkIds)

	// 一次查出所有 chunk，验证属于该 pool
	var chunks []db.Chunk
	if err := p.DbManager.DB.Where("id IN ? AND pool_id = ?", chunkIds, poolId).
		Find(&chunks).Error; err != nil {
		return nil, errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
	}
	if len(chunks) != N {
		return nil, errs.New(errs.ECODE_CHUNK_NOT_FOUND, errs.ESTR_CHUNK_NOT_FOUND,
			"some chunks not found or not in pool", strconv.FormatInt(poolId, 10))
	}

	// 按 chunk ID 建索引保持顺序
	chunkById := make(map[int64]db.Chunk, N)
	for _, c := range chunks {
		chunkById[c.Id] = c
	}
	ordered := make([]db.Chunk, N)
	for i, id := range chunkIds {
		ordered[i] = chunkById[id]
	}

	// 预加载盘信息
	diskById, err := p.getDiskMapByPoolId(poolId)
	if err != nil {
		return nil, err
	}
	// ---------------------------------------------------------------
	// 并发读取（优先走缓存，未命中则从源盘读取并异步写入缓存盘）
	// ---------------------------------------------------------------
	results := make([][]byte, N)
	readErrs := parallelEach(N, 0, func(i int) error {
		c := ordered[i]
		// 先尝试从缓存读取（如果 pool 配置了缓存盘）
		if cached, _ := p.tryReadCache(c.Id); cached != nil {
			results[i] = cached
			return nil
		}
		// 缓存未命中，从源盘读取
		disk := diskById[c.DiskId]
		h := p.handlerFor(disk.Backend)
		if h == nil {
			return errs.New(errs.ECODE_FILE_WRITE, errs.ESTR_FILE_WRITE,
				"no handler for backend", strconv.Itoa(int(disk.Backend)))
		}
		data, err := h.Read(disk, c.Path)
		if err != nil {
			return errs.FromError(err, errs.ECODE_FILE_WRITE, errs.ESTR_FILE_WRITE)
		}
		// 异步将数据写入缓存盘
		go p.writeCache(poolId, c.Id, data)
		results[i] = data
		return nil
	})
	if err := firstErr(readErrs); err != nil {
		return nil, err
	}
	return results, nil
}
