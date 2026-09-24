package pool

import (
	"bytes"
	"os"
	"sort"
	"testing"

	"github.com/klauspost/reedsolomon"
	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/hash"
	"github.com/zzc53/zenofs/internal/testutil"
)

// stripeChunks 取出一条带上已写入的数据分片与校验分片，都按 Index 排序。
func stripeChunks(t *testing.T, env *testutil.Env, stripeId int64) (data, parity []db.Chunk) {
	t.Helper()
	var all []db.Chunk
	if err := env.DB.DB.Where("stripe_id = ? AND status != ?", stripeId, db.ChunkReserved).
		Find(&all).Error; err != nil {
		t.Fatal(err)
	}
	for _, c := range all {
		if c.Type == db.DataChunk {
			data = append(data, c)
		} else {
			parity = append(parity, c)
		}
	}
	sort.Slice(data, func(i, j int) bool { return data[i].Index < data[j].Index })
	sort.Slice(parity, func(i, j int) bool { return parity[i].Index < parity[j].Index })
	return data, parity
}

func TestCalculateStripeParityMatchesReedSolomon(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 2, 1, 4096)
	data := [][]byte{testutil.RandBytes(11, 4096), testutil.RandBytes(12, 4096)}

	chunks, err := pm.AddChunks(p.Id, data)
	if err != nil {
		t.Fatal(err)
	}
	if err := pm.Flush(chunkIds(chunks)); err != nil {
		t.Fatal(err)
	}

	// 写入完成后应恰好有一个条带等待计算 parity
	if !pm.calculateStripeParity() {
		t.Fatal("calculateStripeParity 没有领到任务")
	}
	var left int64
	if err := env.DB.DB.Model(&db.StripeQueue{}).Count(&left).Error; err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("处理完还剩 %d 条任务, want 0", left)
	}
	if pm.calculateStripeParity() {
		t.Fatal("队列已空，calculateStripeParity 不该报处理了任务")
	}

	// parity chunk 元数据补全
	dataChunks, parityChunks := stripeChunks(t, env, chunks[0].StripeId)
	if len(dataChunks) != 2 || len(parityChunks) != 1 {
		t.Fatalf("条带分片数 = %d data + %d parity, want 2+1", len(dataChunks), len(parityChunks))
	}
	pc := parityChunks[0]
	if pc.Status != db.ChunkAllocated {
		t.Fatalf("parity status = %d, want Allocated", pc.Status)
	}
	if pc.Size != 4096 {
		t.Fatalf("parity size = %d, want 4096（与最长的数据分片等长）", pc.Size)
	}

	// 落盘内容必须等于独立用 reedsolomon 复算的结果
	enc, err := reedsolomon.New(2, 1)
	if err != nil {
		t.Fatal(err)
	}
	shards := [][]byte{data[0], data[1], make([]byte, 4096)}
	if err := enc.Encode(shards); err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(chunkFile(t, env, pc))
	if err != nil {
		t.Fatalf("parity 文件不存在: %v", err)
	}
	if !bytes.Equal(onDisk, shards[2]) {
		t.Fatal("parity 内容与 RS 复算不一致")
	}
	if !hash.Equal(onDisk, pc.Hash) {
		t.Fatal("parity hash 与落盘内容不符")
	}
}

func TestCalculateStripeParityPadsUnevenShards(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 2, 1, 4096)
	// 两个数据分片不等长：编码前要按最长的补齐，parity 也是这个长度
	data := [][]byte{testutil.RandBytes(31, 4096), testutil.RandBytes(32, 1024)}

	chunks, err := pm.AddChunks(p.Id, data)
	if err != nil {
		t.Fatal(err)
	}
	if err := pm.Flush(chunkIds(chunks)); err != nil {
		t.Fatal(err)
	}
	if !pm.calculateStripeParity() {
		t.Fatal("没有领到任务")
	}

	_, parityChunks := stripeChunks(t, env, chunks[0].StripeId)
	enc, err := reedsolomon.New(2, 1)
	if err != nil {
		t.Fatal(err)
	}
	padded := make([]byte, 4096)
	copy(padded, data[1])
	shards := [][]byte{data[0], padded, make([]byte, 4096)}
	if err := enc.Encode(shards); err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(chunkFile(t, env, parityChunks[0]))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, shards[2]) {
		t.Fatal("不等长数据分片的 parity 与复算结果不一致")
	}
	if parityChunks[0].Size != 4096 {
		t.Fatalf("parity size = %d, want 4096", parityChunks[0].Size)
	}
}

func TestRebuildStripeRecoversLostData(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 2, 1, 4096)
	data := [][]byte{testutil.RandBytes(41, 4096), testutil.RandBytes(42, 4096)}

	chunks, err := pm.AddChunks(p.Id, data)
	if err != nil {
		t.Fatal(err)
	}
	if err := pm.Flush(chunkIds(chunks)); err != nil {
		t.Fatal(err)
	}
	if !pm.calculateStripeParity() {
		t.Fatal("没有算出 parity，无法验证重建")
	}

	// 模拟坏块：删掉一个数据分片的磁盘文件
	victim := chunks[0]
	if err := os.Remove(chunkFile(t, env, victim)); err != nil {
		t.Fatal(err)
	}
	if _, err := pm.ReadChunks(p.Id, []int64{victim.Id}); err == nil {
		t.Fatal("文件已删除，读取不该成功")
	}

	// 投递重建任务（重复投递会被去重），再同步跑一轮
	queued, err := pm.RebuildByChunks([]int64{victim.Id})
	if err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Fatalf("投递任务数 = %d, want 1", queued)
	}
	if again, err := pm.RebuildByChunks([]int64{victim.Id}); err != nil || again != 0 {
		t.Fatalf("重复投递 = (%d, %v), want (0, nil)", again, err)
	}
	if !pm.rebuildStripe() {
		t.Fatal("rebuildStripe 没有领到任务")
	}

	// 数据恢复：内容与原始数据一致，元数据 size/hash 同步更新
	got, err := pm.ReadChunks(p.Id, []int64{victim.Id})
	if err != nil {
		t.Fatalf("重建后读取: %v", err)
	}
	if !bytes.Equal(got[0], data[0]) {
		t.Fatal("重建出来的数据与原始数据不一致")
	}
	var row db.Chunk
	if err := env.DB.DB.First(&row, victim.Id).Error; err != nil {
		t.Fatal(err)
	}
	if row.Size != int64(len(data[0])) {
		t.Fatalf("重建后 size = %d, want %d", row.Size, len(data[0]))
	}
	if !hash.Equal(got[0], row.Hash) {
		t.Fatal("重建后 hash 与数据不符")
	}

	// 同条带的另一个数据分片与校验分片不受影响
	rest, err := pm.ReadChunks(p.Id, []int64{chunks[1].Id})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rest[0], data[1]) {
		t.Fatal("同条带的其它分片被破坏")
	}
}

func TestRebuildPoolQueuesRepairDisks(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 2, 1, 4096)

	// 没有 Repair 盘时无事可做
	if n, err := pm.RebuildPool(p.Id); err != nil || n != 0 {
		t.Fatalf("RebuildPool = (%d, %v), want (0, nil)", n, err)
	}

	payloads := [][]byte{testutil.RandBytes(51, 128), testutil.RandBytes(52, 128)}
	chunks, err := pm.AddChunks(p.Id, payloads)
	if err != nil {
		t.Fatal(err)
	}
	if err := pm.Flush(chunkIds(chunks)); err != nil {
		t.Fatal(err)
	}
	// 先把校验分片算出来，否则条带里只剩 1 个可用分片，RS 无法恢复
	if !pm.calculateStripeParity() {
		t.Fatal("没有算出 parity，无法验证换盘重建")
	}

	// 把承载第一个分片的盘置为 Repair 并换到空目录（模拟换盘后数据丢失）。
	// SwapDisk 会同时把池下线，重建完成后才恢复。
	var row db.Chunk
	if err := env.DB.DB.First(&row, chunks[0].Id).Error; err != nil {
		t.Fatal(err)
	}
	if err := pm.SwapDisk(row.DiskId, env.Path("replacement")); err != nil {
		t.Fatal(err)
	}

	queued, err := pm.RebuildPool(p.Id)
	if err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Fatalf("投递子任务数 = %d, want 1", queued)
	}
	// 作业去重：同一池同时只允许一个进行中的作业
	if again, err := pm.RebuildPool(p.Id); err != nil || again != 0 {
		t.Fatalf("重复投递 = (%d, %v), want (0, nil)", again, err)
	}

	if !pm.rebuildStripe() {
		t.Fatal("rebuildStripe 没有领到任务")
	}

	// 重建完成后池与盘自动恢复 Online，原数据能从校验分片恢复出来
	got, err := pm.GetPool(p.Id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != db.Online {
		t.Fatalf("重建后 pool status = %d, want Online", got.Status)
	}
	data, err := pm.ReadChunks(p.Id, []int64{chunks[0].Id})
	if err != nil {
		t.Fatalf("重建后读取: %v", err)
	}
	if !bytes.Equal(data[0], payloads[0]) {
		t.Fatal("换盘后没有恢复出原数据")
	}
}
