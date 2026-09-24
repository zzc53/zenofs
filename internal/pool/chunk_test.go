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
	p := env.NewPool("p", 2, 1, 4096)

	chunks, err := pm.AddChunks(p.Id, [][]byte{testutil.RandBytes(1, 64), testutil.RandBytes(2, 64)})
	if err != nil {
		t.Fatal(err)
	}

	// 每次写盘留下 Pending（意图）+ Success（结果）两条记录
	var queued int64
	if err := env.DB.DB.Model(&db.WriteQueue{}).Count(&queued).Error; err != nil {
		t.Fatal(err)
	}
	if queued != int64(2*len(chunks)) {
		t.Fatalf("write queue = %d, want %d", queued, 2*len(chunks))
	}

	if err := pm.Flush(chunkIds(chunks)); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// 已完成的记录对被搬走，stripe 进 stripe queue（Parity 任务）
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
	if tasks[0].StripeId != chunks[0].StripeId {
		t.Fatalf("任务指向的条带不对: %d", tasks[0].StripeId)
	}

	// 没有新写入时再 Flush 是空操作
	if err := pm.Flush(nil); err != nil {
		t.Fatal(err)
	}
}
