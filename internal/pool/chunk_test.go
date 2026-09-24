package pool

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
	"github.com/zzc53/zenofs/internal/hash"
	"github.com/zzc53/zenofs/internal/testutil"
)

// chunkFile 返回 chunk 落在磁盘上的绝对路径。
func chunkFile(t *testing.T, env *testutil.Env, chunk db.Chunk) string {
	t.Helper()
	var disk db.Disk
	if err := env.DB.DB.First(&disk, chunk.DiskId).Error; err != nil {
		t.Fatalf("查 chunk %d 的磁盘: %v", chunk.Id, err)
	}
	return filepath.Join(disk.Path, chunk.Path)
}

func chunkIds(chunks []db.Chunk) []int64 {
	ids := make([]int64, len(chunks))
	for i, c := range chunks {
		ids[i] = c.Id
	}
	return ids
}

func TestAddChunksAndReadChunks(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 2, 1, 4096) // 2 数据 + 1 校验 = 3 块盘

	payloads := [][]byte{
		testutil.RandBytes(1, 4096),
		testutil.RandBytes(2, 2048),
		testutil.RandBytes(3, 1),
	}
	chunks, err := pm.AddChunks(p.Id, payloads)
	if err != nil {
		t.Fatalf("AddChunks: %v", err)
	}
	if len(chunks) != len(payloads) {
		t.Fatalf("返回 %d 个 chunk, want %d", len(chunks), len(payloads))
	}

	// 元数据：已分配、size/hash 与输入一致、文件真的落盘
	for i, c := range chunks {
		if c.Status != db.ChunkAllocated {
			t.Errorf("chunk %d status = %d, want Allocated", i, c.Status)
		}
		if c.Size != int64(len(payloads[i])) {
			t.Errorf("chunk %d size = %d, want %d", i, c.Size, len(payloads[i]))
		}
		if !hash.Equal(payloads[i], c.Hash) {
			t.Errorf("chunk %d hash 与数据不符", i)
		}
		if c.StripeId == 0 || c.PoolId != p.Id {
			t.Errorf("chunk %d 归属不对: stripe=%d pool=%d", i, c.StripeId, c.PoolId)
		}
		got, err := os.ReadFile(chunkFile(t, env, c))
		if err != nil {
			t.Fatalf("chunk %d 文件不存在: %v", i, err)
		}
		if !bytes.Equal(got, payloads[i]) {
			t.Errorf("chunk %d 落盘内容不对", i)
		}
	}

	// 读回：顺序与请求一致，内容逐字节相等
	got, err := pm.ReadChunks(p.Id, chunkIds(chunks))
	if err != nil {
		t.Fatalf("ReadChunks: %v", err)
	}
	if len(got) != len(payloads) {
		t.Fatalf("读到 %d 个, want %d", len(got), len(payloads))
	}
	for i := range payloads {
		if !bytes.Equal(got[i], payloads[i]) {
			t.Errorf("第 %d 个 chunk 内容不一致", i)
		}
	}

	// 乱序请求也要按请求顺序返回
	reversed := []int64{chunks[2].Id, chunks[1].Id, chunks[0].Id}
	got, err = pm.ReadChunks(p.Id, reversed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[0], payloads[2]) || !bytes.Equal(got[1], payloads[1]) || !bytes.Equal(got[2], payloads[0]) {
		t.Fatal("乱序读取没有按请求顺序返回")
	}
}

func TestAddChunksReusesReservedSlots(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 2, 1, 4096)

	// 写 1 个：新建一个条带，占 3 个槽位（1 已分配 + 2 预留）
	first, err := pm.AddChunks(p.Id, [][]byte{testutil.RandBytes(1, 64)})
	if err != nil {
		t.Fatal(err)
	}
	var stripes int64
	if err := env.DB.DB.Model(&db.Stripe{}).Count(&stripes).Error; err != nil {
		t.Fatal(err)
	}
	if stripes != 1 {
		t.Fatalf("stripe 数 = %d, want 1", stripes)
	}
	var chunks int64
	if err := env.DB.DB.Model(&db.Chunk{}).Count(&chunks).Error; err != nil {
		t.Fatal(err)
	}
	if chunks != 3 {
		t.Fatalf("槽位数 = %d, want 3 (2 data + 1 parity)", chunks)
	}

	// 再写 1 个：复用同一条带里的预留槽，不新建条带
	second, err := pm.AddChunks(p.Id, [][]byte{testutil.RandBytes(2, 64)})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.DB.DB.Model(&db.Stripe{}).Count(&stripes).Error; err != nil {
		t.Fatal(err)
	}
	if stripes != 1 {
		t.Fatalf("复用后 stripe 数 = %d, want 1", stripes)
	}
	if second[0].StripeId != first[0].StripeId {
		t.Fatalf("新 chunk 落到别的条带: %d vs %d", second[0].StripeId, first[0].StripeId)
	}
	if second[0].Index == first[0].Index {
		t.Fatalf("两个数据 chunk 的 index 相同: %d", second[0].Index)
	}
}

func TestAddChunksValidation(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 2, 1, 1) // chunk 上限 1KB

	_, err := pm.AddChunks(p.Id, nil)
	assertCode(t, err, errs.ECODE_CHUNK_EMPTY, "空数据列表")

	_, err = pm.AddChunks(p.Id, [][]byte{{1, 2, 3}, {}})
	assertCode(t, err, errs.ECODE_CHUNK_EMPTY, "含空数据")

	_, err = pm.AddChunks(p.Id, [][]byte{make([]byte, 2*1024)})
	assertCode(t, err, errs.ECODE_CHUNK_SIZE_EXCEED, "超过 chunk 上限")

	_, err = pm.AddChunks(p.Id+1000, [][]byte{[]byte("x")})
	assertCode(t, err, errs.ECODE_POOL_BAD, "不存在的 pool")

	// 池离线后拒绝写入
	if err := pm.OfflinePool(p.Id); err != nil {
		t.Fatal(err)
	}
	_, err = pm.AddChunks(p.Id, [][]byte{[]byte("x")})
	assertCode(t, err, errs.ECODE_POOL_OFFLINE, "离线 pool 写入")

	// 在线数据盘数 != data + parity 时无法分配槽位
	offline := env.NewPool("p2", 2, 1, 1)
	if err := env.DB.DB.Delete(&db.Disk{}, "pool_id = ?", offline.Id).Error; err != nil {
		t.Fatal(err)
	}
	env.NewDisk(offline.Id, "only-one", db.DataDisk)
	_, err = pm.AddChunks(offline.Id, [][]byte{[]byte("x")})
	assertCode(t, err, errs.ECODE_DISK_OFFLINE, "盘数不足")
}

func TestReadChunksValidation(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 1, 0, 4096)
	other := env.NewPool("other", 1, 0, 4096)

	chunk, err := pm.AddChunk(p.Id, testutil.RandBytes(1, 256))
	if err != nil {
		t.Fatal(err)
	}

	_, err = pm.ReadChunks(p.Id, nil)
	assertCode(t, err, errs.ECODE_CHUNK_EMPTY, "空 chunk 列表")

	_, err = pm.ReadChunks(p.Id, []int64{chunk.Id + 1000})
	assertCode(t, err, errs.ECODE_CHUNK_NOT_FOUND, "不存在的 chunk")

	_, err = pm.ReadChunks(other.Id, []int64{chunk.Id})
	assertCode(t, err, errs.ECODE_CHUNK_NOT_FOUND, "chunk 不属于该 pool")

	if err := pm.OfflinePool(p.Id); err != nil {
		t.Fatal(err)
	}
	_, err = pm.ReadChunks(p.Id, []int64{chunk.Id})
	assertCode(t, err, errs.ECODE_POOL_OFFLINE, "离线 pool 读取")
	if err := env.DB.DB.Model(&db.Pool{}).Where("id = ?", p.Id).Update("status", db.Online).Error; err != nil {
		t.Fatal(err)
	}

	// 文件被删：读不到就报文件错误（上层据此触发条带重建）
	if err := os.Remove(chunkFile(t, env, *chunk)); err != nil {
		t.Fatal(err)
	}
	_, err = pm.ReadChunks(p.Id, []int64{chunk.Id})
	assertCode(t, err, errs.ECODE_FILE_WRITE, "磁盘文件缺失")
}

func TestWriteChunksOverwrites(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 1, 0, 1) // 上限 1KB

	old := testutil.RandBytes(1, 512)
	chunk, err := pm.AddChunk(p.Id, old)
	if err != nil {
		t.Fatal(err)
	}

	fresh := testutil.RandBytes(2, 1024)
	got, err := pm.WriteChunks([]WriteChunkItem{{ChunkId: chunk.Id, Data: fresh}})
	if err != nil {
		t.Fatalf("WriteChunks: %v", err)
	}
	if got[0].Status != db.ChunkAllocated || got[0].Size != int64(len(fresh)) {
		t.Fatalf("覆写后元数据不对: %+v", got[0])
	}
	if !hash.Equal(fresh, got[0].Hash) {
		t.Fatal("覆写后 hash 与数据不符")
	}

	// 元数据与磁盘内容都换成新数据
	var row db.Chunk
	if err := env.DB.DB.First(&row, chunk.Id).Error; err != nil {
		t.Fatal(err)
	}
	if row.Size != int64(len(fresh)) {
		t.Fatalf("库里 size = %d, want %d", row.Size, len(fresh))
	}
	onDisk, err := os.ReadFile(chunkFile(t, env, row))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, fresh) {
		t.Fatal("磁盘内容没有更新")
	}
	read, err := pm.ReadChunks(p.Id, []int64{chunk.Id})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(read[0], fresh) {
		t.Fatal("读回的不是新数据")
	}

	// 错误分支
	_, err = pm.WriteChunks(nil)
	assertCode(t, err, errs.ECODE_CHUNK_EMPTY, "空 items")
	_, err = pm.WriteChunks([]WriteChunkItem{{ChunkId: chunk.Id, Data: nil}})
	assertCode(t, err, errs.ECODE_CHUNK_EMPTY, "空数据")
	_, err = pm.WriteChunks([]WriteChunkItem{{ChunkId: chunk.Id + 1000, Data: []byte("x")}})
	assertCode(t, err, errs.ECODE_CHUNK_NOT_FOUND, "不存在的 chunk")
	_, err = pm.WriteChunks([]WriteChunkItem{{ChunkId: chunk.Id, Data: make([]byte, 2*1024)}})
	assertCode(t, err, errs.ECODE_CHUNK_SIZE_EXCEED, "覆写超过上限")
}

func TestFlushMovesWriteQueueToStripeQueue(t *testing.T) {
	env, pm := newEnv(t)

	// 手工造两条记录对（同一轮写入的意图 + 结果）。
	// 故意不走 AddChunks：写入路径自己就会搬运（见 persistChunkData），
	// 那样就测不到 Flush 本身了。
	if err := env.DB.DB.Create(&[]db.WriteQueue{
		{ChunkId: 1, StripeId: 7, Status: db.TaskPending},
		{ChunkId: 1, StripeId: 7, Status: db.TaskSuccess},
		{ChunkId: 2, StripeId: 7, Status: db.TaskPending},
		{ChunkId: 2, StripeId: 7, Status: db.TaskSuccess},
	}).Error; err != nil {
		t.Fatal(err)
	}

	if err := pm.Flush([]int64{1, 2}); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// 已完成的记录对被搬走，条带进 stripe queue（Parity 任务）
	var queued int64
	if err := env.DB.DB.Model(&db.WriteQueue{}).Count(&queued).Error; err != nil {
		t.Fatal(err)
	}
	if queued != 0 {
		t.Fatalf("Flush 后 write queue = %d, want 0", queued)
	}
	var tasks []db.StripeQueue
	if err := env.DB.DB.Find(&tasks).Error; err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("stripe queue = %d 条, want 1（同一条带只入队一次）", len(tasks))
	}
	if tasks[0].Type != db.StripeQueueParity || tasks[0].Status != db.TaskPending {
		t.Fatalf("任务字段不对: %+v", tasks[0])
	}
	if tasks[0].StripeId != 7 {
		t.Fatalf("任务指向的条带不对: %d", tasks[0].StripeId)
	}

	// 没有新写入时再 Flush 是空操作
	if err := pm.Flush(nil); err != nil {
		t.Fatal(err)
	}
}

// 写入之后必须立刻把校验块算出来。
//
// 这条链是：写盘 → write queue →（Flush 搬运）→ stripe queue → parity worker。
// 曾经 Flush 是个没人调用的死代码，于是 parity 永远停在 Reserved 空占位，
// 校验盘上只有一个预分配的空文件——冗余保护看着有、其实完全没生效。
func TestWriteEnqueuesParityAndComputesIt(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 2, 1, 4096)

	chunks, err := pm.AddChunks(p.Id, [][]byte{testutil.RandBytes(51, 128), testutil.RandBytes(52, 128)})
	if err != nil {
		t.Fatal(err)
	}

	// 1) write queue 应当被搬运干净
	var queued int64
	if err := env.DB.DB.Model(&db.WriteQueue{}).Count(&queued).Error; err != nil {
		t.Fatal(err)
	}
	if queued != 0 {
		t.Fatalf("写入后 write queue 还剩 %d 条，Flush 没被调用", queued)
	}

	// 2) 该条带应当进了 stripe queue（Parity、Pending），去重后只有一条
	var tasks []db.StripeQueue
	if err := env.DB.DB.Where("type = ?", db.StripeQueueParity).Find(&tasks).Error; err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("stripe queue 里 parity 任务 = %d 条, want 1", len(tasks))
	}
	if tasks[0].Status != db.TaskPending {
		t.Fatalf("任务状态 = %v, want Pending", tasks[0].Status)
	}
	if tasks[0].StripeId != chunks[0].StripeId {
		t.Fatalf("任务指向的条带 = %d, want %d", tasks[0].StripeId, chunks[0].StripeId)
	}

	// 3) 跑一次 parity 计算（worker 是后台的，测试里直接推一轮）
	if !pm.calculateStripeParity() {
		t.Fatalf("calculateStripeParity 应当处理这批任务")
	}
	var parity db.Chunk
	if err := env.DB.DB.Where("stripe_id = ? AND type = ?", chunks[0].StripeId, db.ParityChunk).
		First(&parity).Error; err != nil {
		t.Fatal(err)
	}
	if parity.Status != db.ChunkAllocated || parity.Size == 0 {
		t.Fatalf("校验块没被算出来: status=%v size=%d", parity.Status, parity.Size)
	}

	// 4) 校验块的字节要真的能读回来
	data, err := pm.ReadChunks(p.Id, []int64{parity.Id})
	if err != nil {
		t.Fatalf("读校验块: %v", err)
	}
	if len(data) != 1 || len(data[0]) != int(parity.Size) {
		t.Fatalf("校验块内容长度不对: %d vs %d", len(data[0]), parity.Size)
	}
}

// 纯条带池（ParityShards == 0）没有 parity 要算，worker 不该去读 data chunk。
//
// 测试手法：把 data chunk 的文件从磁盘上删掉。如果 computeStripe 真的跳过了，
// 它就不会去读文件，任务照常成功；没跳过的话会因读不到文件而失败。
func TestParitySkippedWhenPoolHasNoParityShards(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 2, 0, 4096) // 2 data + 0 parity：纯条带

	chunks, err := pm.AddChunks(p.Id, [][]byte{testutil.RandBytes(61, 64), testutil.RandBytes(62, 64)})
	if err != nil {
		t.Fatal(err)
	}

	// 写入路径已经 Flush 过，条带在 parity 队列里了；现在把文件删掉
	for _, c := range chunks {
		var disk db.Disk
		if err := env.DB.DB.First(&disk, c.DiskId).Error; err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(disk.Path, c.Path)); err != nil {
			t.Fatal(err)
		}
	}

	if !pm.calculateStripeParity() {
		t.Fatalf("纯条带池的任务也应当被处理（并在编码前直接跳过）")
	}

	var left int64
	if err := env.DB.DB.Model(&db.StripeQueue{}).Count(&left).Error; err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("任务没有被消费完，还剩 %d 条", left)
	}
}

// 启动清理：已经写完的记录搬进 stripe queue（该算的校验块不能因为一次重启就漏），
// 半途中断的孤片（只有 Pending、没有结果）直接清掉。
func TestCleanupWriteQueueOnStartup(t *testing.T) {
	env, pm := newEnv(t)

	if err := env.DB.DB.Create(&[]db.WriteQueue{
		{ChunkId: 1, StripeId: 7, Status: db.TaskPending},
		{ChunkId: 1, StripeId: 7, Status: db.TaskSuccess},
		{ChunkId: 2, StripeId: 8, Status: db.TaskPending}, // 崩在这一步：只有意图没有结果
	}).Error; err != nil {
		t.Fatal(err)
	}

	removed, err := pm.CleanupWriteQueueOnStartup()
	if err != nil {
		t.Fatalf("CleanupWriteQueueOnStartup: %v", err)
	}
	if removed != 1 {
		t.Fatalf("清理掉的残留记录 = %d, want 1（孤立的 Pending）", removed)
	}

	var left int64
	if err := env.DB.DB.Model(&db.WriteQueue{}).Count(&left).Error; err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("write queue 还剩 %d 条", left)
	}

	// 该进 stripe queue 的进去了：只有 stripe 7，且是 parity 任务
	var tasks []db.StripeQueue
	if err := env.DB.DB.Where("type = ?", db.StripeQueueParity).Find(&tasks).Error; err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].StripeId != 7 {
		t.Fatalf("stripe queue = %+v, want 一条 stripe 7 的 parity 任务", tasks)
	}
}
