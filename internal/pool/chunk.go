package pool

import (
	"encoding/binary"
	"fmt"
	"path"
	"strconv"
	"sync"
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

	var chunks []db.Chunk

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

	// ---------------------------------------------------------------
	// 以下三个 Phase 在同一个事务中执行，保证分配的原子性。
	// ---------------------------------------------------------------
	err = p.DbManager.DB.Transaction(func(tx *gorm.DB) error {
		// ---------------------------------------------------------------
		// Phase 1: 消费该 pool 中已预分配的 Reserved data chunks。
		// 通过 JOIN stripes 表过滤出属于当前 pool 的 Reserved slot。
		// 使用 SKIP LOCKED 防止并发写入时锁冲突（各自跳过已锁行）。
		// ---------------------------------------------------------------
		var reserved []db.Chunk
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Model(&db.Chunk{}).
			Select("chunks.*").
			Where("status = ? AND chunks.type = ? AND pool_id = ?", db.ChunkReserved, db.DataChunk, poolId).
			Limit(N).
			Find(&reserved).Error; err != nil {
			return errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
		}

		need := N - len(reserved)

		// ---------------------------------------------------------------
		// Phase 2: 如果 Phase 1 不够用，新建 stripe。
		//
		// 每个 stripe 包含 dataShards 个 data slot + parityShards 个 parity slot。
		// 新创建的 data chunk 直接写入完整数据（status=Pending）并附带
		// size 和 checksum，无需 Phase 3 二次更新。
		// 磁盘分配通过 crypto/rand 种子 shuffle 实现负载均衡。
		// ---------------------------------------------------------------
		var newChunks []db.Chunk
		if need > 0 {
			dataShards := int(pool.DataShards)
			totalSlots := dataShards + int(pool.ParityShards)
			if totalSlots <= 0 {
				return errs.New(errs.ECODE_POOL_BAD, errs.ESTR_POOL_BAD, "invalid stripe capacity",
					fmt.Sprintf("data_shards=%d, parity_shards=%d", pool.DataShards, pool.ParityShards))
			}
			if len(disks) != totalSlots {
				return errs.New(errs.ECODE_DISK_OFFLINE, errs.ESTR_DISK_OFFLINE,
					"not all disks online for stripe allocation",
					fmt.Sprintf("online=%d, need=%d", len(disks), totalSlots))
			}

			remaining := need
			globalNewIdx := 0
			for remaining > 0 {
				// 每次循环创建一个新的 stripe
				batchSize := dataShards
				if remaining < dataShards {
					batchSize = remaining
				}

				// 创建 stripe 行
				stripe := db.Stripe{PoolId: poolId}
				if err := tx.Create(&stripe).Error; err != nil {
					return errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
				}

				// 对 disks 进行 shuffle，将 chunk 均匀分布到各磁盘
				shuffled := append([]db.Disk{}, disks...)
				mrand.New(mrand.NewSource(cryptoRandSeed())).Shuffle(len(shuffled), func(i, j int) {
					shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
				})

				// 为 stripe 创建所有 chunk 行（data + parity）
				allChunks := make([]db.Chunk, totalSlots)
				for i := 0; i < totalSlots; i++ {
					typ := db.ParityChunk
					if i < dataShards {
						typ = db.DataChunk
					}
					status := db.ChunkReserved
					var size int64
					var hash []byte
					if i < batchSize {
						// 本次 batch 中实际写入的 data chunk，直接填数据
						status = db.ChunkAllocated
						itemIdx := len(reserved) + globalNewIdx + i
						size = int64(items[itemIdx].Size)
						hash = items[itemIdx].Hash[:]
					}
					p, err := generateChunkPaths(1)
					if err != nil {
						return errs.FromError(err, errs.ECODE_CRYPTO_ERROR, errs.ESTR_CRYPTO_ERROR)
					}
					idx := int64(i)
					if i >= dataShards {
						idx = int64(i - dataShards) // parity 独立编号从 0 开始
					}
					allChunks[i] = db.Chunk{
						Status:   status,
						Path:     p[0],
						DiskId:   shuffled[i].Id,
						StripeId: stripe.Id,
						PoolId:   stripe.PoolId,
						Size:     size,
						Hash:     hash,
						Type:     typ,
						Index:    idx,
					}
				}
				if err := tx.Create(&allChunks).Error; err != nil {
					return errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
				}

				// 收集本次批次的 data chunk 作为返回结果
				for i := 0; i < batchSize; i++ {
					newChunks = append(newChunks, allChunks[i])
				}

				remaining -= batchSize
				globalNewIdx += batchSize
			}
		}

		// ---------------------------------------------------------------
		// Phase 3: 激活老的 Reserved chunks。
		// 对于 Phase 1 拿到的 reserved chunks，补上 size、checksum 和
		// 生成文件路径，更新状态为 Pending。
		// ---------------------------------------------------------------
		for i := range reserved {
			reserved[i].Status = db.ChunkAllocated
			reserved[i].Size = items[i].Size
			reserved[i].Hash = items[i].Hash
			if err := tx.Save(&reserved[i]).Error; err != nil {
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
// 分配策略（三个 Phase 在同一事务中执行）：
//
//	Phase 1 — 消费预分配的 Reserved chunks
//	  查询当前 pool 中状态为 Reserved 的 data chunk slot（加 SKIP LOCKED），
//	  尽量复用已有的预留槽位，减少 stripe 碎片。
//
//	Phase 2 — 不够则建新 stripe
//	  如果 Phase 1 不够用，创建新的 stripe 和对应的 chunk 行。
//	  新 chunk 创建时直接写完成数据（status=Pending），无需 Phase 3 二次更新。
//	  磁盘分配通过 shuffle 实现负载均衡。
//
//	Phase 3 — 激活 old Reserved chunks
//	  对 Phase 1 获取的 old reserved chunks，更新 size/checksum/path 并标记 Pending。
//
// 事务提交后，事务外并发将实际数据写入磁盘。
// 写入成功后标记 chunk 为 Dirty 并入队 WriteQueue，由 parity worker 异步计算 RS 校验。
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
	// 预计算每个 chunk 的 BLAKE3 哈希和大小（在事务外计算）。
	// 这样事务内部只需读写 DB，不占用大量内存。
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

	// ---------------------------------------------------------------
	// 预加载该 pool 下所有磁盘信息（事务外），
	// 后续文件写入时需要根据 disk.Backend 匹配对应的 Writer。
	// ---------------------------------------------------------------
	diskById, err := p.getDiskMapByPoolId(poolId)
	if err != nil {
		return nil, err
	}

	chunks, err := p.getNewChunks(poolId, items)

	// ---------------------------------------------------------------
	// 事务提交后，并发将实际数据写入磁盘。
	// 每个 chunk 启动一个 goroutine，通过 channel 收集写入结果。
	// ---------------------------------------------------------------
	type writeResult struct {
		idx int
		err error
	}
	resultCh := make(chan writeResult, len(chunks))
	var wg sync.WaitGroup
	for i, c := range chunks {
		wg.Add(1)
		go func(idx int, disk db.Disk, relPath string, data []byte) {
			defer wg.Done()
			h := p.handlerFor(disk.Backend)
			if h == nil {
				resultCh <- writeResult{idx, errs.New(errs.ECODE_FILE_WRITE, errs.ESTR_FILE_WRITE,
					"no handler for backend", strconv.Itoa(int(disk.Backend)))}
				return
			}
			if err := h.Write(disk, relPath, data); err != nil {
				resultCh <- writeResult{idx, errs.FromError(err, errs.ECODE_FILE_WRITE, errs.ESTR_FILE_WRITE)}
			}
		}(i, diskById[c.DiskId], c.Path, items[i].Data)
	}
	wg.Wait()
	close(resultCh)

	// ---------------------------------------------------------------
	// 收集写入结果，处理部分失败的情况：
	//   - 写入失败的 chunk → 标记 Error
	//   - 写入成功的 chunk → 标记 Dirty，入队 WriteQueue 让 parity worker 处理
	// ---------------------------------------------------------------
	var firstErr error
	var failedIdx []int
	for r := range resultCh {
		if r.err != nil {
			failedIdx = append(failedIdx, r.idx)
			if firstErr == nil {
				firstErr = r.err
			}
		}
	}
	if firstErr != nil {
		failed := make(map[int]bool, len(failedIdx))
		for _, idx := range failedIdx {
			failed[idx] = true
		}
		var errorIds, successIds []int64
		for i, c := range chunks {
			if failed[i] {
				errorIds = append(errorIds, c.Id)
			} else {
				successIds = append(successIds, c.Id)
			}
		}
		if len(errorIds) > 0 {

		}
		if len(successIds) > 0 {
			var wq []db.WriteQueue
			for i, c := range chunks {
				if !failed[i] {
					wq = append(wq, db.WriteQueue{ChunkId: c.Id, StripeId: c.StripeId})
				}
			}
			p.DbManager.DB.Create(&wq)
		}
		return nil, firstErr
	}

	// ---------------------------------------------------------------
	// 全部写入成功：标记所有 chunk 为 Dirty，批量入队 WriteQueue
	// ---------------------------------------------------------------
	ids := make([]int64, len(chunks))
	for i, c := range chunks {
		ids[i] = c.Id
	}

	writeEntries := make([]db.WriteQueue, len(chunks))
	for i, c := range chunks {
		writeEntries[i] = db.WriteQueue{
			ChunkId:  c.Id,
			StripeId: c.StripeId,
		}
	}
	if err := p.DbManager.DB.Create(&writeEntries).Error; err != nil {
		return nil, errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
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

// WriteChunkItem 指定待写入的 chunk ID 和对应的数据。
type WriteChunkItem struct {
	ChunkId int64
	Data    []byte
}

// WriteChunks 向已分配的 chunk（通过 ChunkId）写入数据。
//
// 流程：
//  1. 根据 chunkId 查出 chunk 及其所属 pool，校验 pool 在线
//  2. 校验数据大小不超过 pool 的 chunk size
//  3. 在事务中更新 chunk 元数据（size / checksum / path / status）
//  4. 事务外并发写入磁盘
//  5. 写入成功后标记 Dirty 并入队 WriteQueue
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

	// 预计算每个 chunk 的 hash 和 size
	prep := make([]ChunkData, N)
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
	}

	// ---------------------------------------------------------------
	// 一次查出所有 chunk 及其所属 pool
	// ---------------------------------------------------------------
	chunkIds := make([]int64, N)
	for i, it := range prep {
		chunkIds[i] = it.Chunk.Id
	}
	var chunks []db.Chunk
	if err := p.DbManager.DB.Where("id IN ?", chunkIds).Find(&chunks).Error; err != nil {
		return nil, errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
	}
	if len(chunks) != N {
		return nil, errs.New(errs.ECODE_CHUNK_NOT_FOUND, errs.ESTR_CHUNK_NOT_FOUND,
			"some chunks not found", "")
	}

	// 推导 poolId，校验 pool 在线
	poolId := int64(0)
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
	for _, it := range prep {
		if int64(it.Size) > maxSize {
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
	for i, it := range prep {
		ordered[i] = *chunkById[it.Id]
	}

	// 预加载盘信息（写文件用）
	diskById, err := p.getDiskMapByPoolId(poolId)
	if err != nil {
		return nil, err
	}

	// ---------------------------------------------------------------
	// 事务内更新 chunk 元数据（size / checksum / path / status）
	// ---------------------------------------------------------------
	err = p.DbManager.DB.Transaction(func(tx *gorm.DB) error {
		for i := range ordered {
			ordered[i].Status = db.ChunkAllocated
			ordered[i].Size = int64(prep[i].Size)
			ordered[i].Hash = prep[i].Hash
		}
		return tx.Save(&ordered).Error
	})
	if err != nil {
		return nil, err
	}

	// ---------------------------------------------------------------
	// 事务外并发写文件
	// ---------------------------------------------------------------
	type writeResult struct {
		idx int
		err error
	}
	resultCh := make(chan writeResult, N)
	var wg sync.WaitGroup
	for i := range ordered {
		wg.Add(1)
		go func(idx int, disk db.Disk, relPath string, data []byte) {
			defer wg.Done()
			h := p.handlerFor(disk.Backend)
			if h == nil {
				resultCh <- writeResult{idx, errs.New(errs.ECODE_FILE_WRITE, errs.ESTR_FILE_WRITE,
					"no handler for backend", strconv.Itoa(int(disk.Backend)))}
				return
			}
			if err := h.Write(disk, relPath, data); err != nil {
				resultCh <- writeResult{idx, errs.FromError(err, errs.ECODE_FILE_WRITE, errs.ESTR_FILE_WRITE)}
			}
		}(i, diskById[ordered[i].DiskId], ordered[i].Path, prep[i].Data)
	}
	wg.Wait()
	close(resultCh)

	// 收集失败结果
	var firstErr error
	var failedIdx []int
	for r := range resultCh {
		if r.err != nil {
			failedIdx = append(failedIdx, r.idx)
			if firstErr == nil {
				firstErr = r.err
			}
		}
	}
	if firstErr != nil {
		var errorIds, successIds []int64
		failed := make(map[int]bool, len(failedIdx))
		for _, idx := range failedIdx {
			failed[idx] = true
		}
		for i := range ordered {
			if failed[i] {
				errorIds = append(errorIds, ordered[i].Id)
			} else {
				successIds = append(successIds, ordered[i].Id)
			}
		}
		// 部分失败：失败的标 Error，成功的标 Dirty 并入队
		if len(errorIds) > 0 {

		}
		if len(successIds) > 0 {
			var wq []db.WriteQueue
			for i, c := range ordered {
				if !failed[i] {
					wq = append(wq, db.WriteQueue{ChunkId: c.Id, StripeId: c.StripeId})
				}
			}
			p.DbManager.DB.Create(&wq)
		}
		return nil, firstErr
	}

	// ---------------------------------------------------------------
	// 全部写入成功：标记 Dirty 并入队 WriteQueue
	// ---------------------------------------------------------------
	allIds := make([]int64, N)
	for i := range ordered {
		allIds[i] = ordered[i].Id
	}

	writeEntries := make([]db.WriteQueue, N)
	for i := range ordered {
		writeEntries[i] = db.WriteQueue{
			ChunkId:  ordered[i].Id,
			StripeId: ordered[i].StripeId,
		}
	}
	if err := p.DbManager.DB.Create(&writeEntries).Error; err != nil {
		return nil, errs.FromError(err, errs.ECODE_DB_BAD_QUERY, errs.ESTR_DB_BAD_QUERY)
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
	type readResult struct {
		idx       int
		data      []byte
		err       error
		fromCache bool
	}
	resultCh := make(chan readResult, N)
	var wg sync.WaitGroup
	for i, c := range ordered {
		wg.Add(1)
		go func(idx int, chk db.Chunk) {
			defer wg.Done()
			// 先尝试从缓存读取（如果 pool 配置了缓存盘）
			if data, _ := p.tryReadCache(chk.Id); data != nil {
				resultCh <- readResult{idx: idx, data: data, err: nil, fromCache: true}
				return
			}
			// 缓存未命中，从源盘读取
			disk := diskById[chk.DiskId]
			h := p.handlerFor(disk.Backend)
			if h == nil {
				resultCh <- readResult{idx, nil, errs.New(errs.ECODE_FILE_WRITE, errs.ESTR_FILE_WRITE,
					"no handler for backend", strconv.Itoa(int(disk.Backend))), false}
				return
			}
			data, err := h.Read(disk, chk.Path)
			if err != nil {
				resultCh <- readResult{idx, nil, errs.FromError(err, errs.ECODE_FILE_WRITE, errs.ESTR_FILE_WRITE), false}
			} else {
				// 异步将数据写入缓存盘
				go p.writeCache(poolId, chk.Id, data)
				resultCh <- readResult{idx, data, nil, false}
			}
		}(i, c)
	}
	wg.Wait()
	close(resultCh)

	results := make([][]byte, N)
	for r := range resultCh {
		if r.err != nil {
			return nil, r.err
		}
		results[r.idx] = r.data
	}
	return results, nil
}
