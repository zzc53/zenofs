package pool

import (
	"os"
	"testing"
	"time"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/testutil"
)

// ageChunks 把这些分片的创建时间挪到保护期之前，模拟"陈旧孤儿"。
func ageChunks(t *testing.T, env *testutil.Env, ids []int64) {
	t.Helper()
	old := time.Now().Unix() - int64(orphanGracePeriod.Seconds()) - 60
	if err := env.DB.DB.Model(&db.Chunk{}).Where("id IN ?", ids).
		Update("created_at", old).Error; err != nil {
		t.Fatal(err)
	}
}

func TestGarbageCollectReleasesOrphans(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 2, 1, 4096)

	chunks, err := pm.AddChunks(p.Id, [][]byte{testutil.RandBytes(9, 1024)})
	if err != nil {
		t.Fatal(err)
	}
	target := chunks[0]

	// 刚落盘的分片还在保护期内：扫描不能碰它
	res, err := pm.GarbageCollect(p.Id, 0)
	if err != nil {
		t.Fatalf("GarbageCollect: %v", err)
	}
	if res.Released != 0 {
		t.Fatalf("保护期内的分片不该被回收，实际释放 %d", res.Released)
	}

	ageChunks(t, env, []int64{target.Id})
	file := chunkFile(t, env, target)

	n, bytes, err := pm.OrphanStats(p.Id)
	if err != nil {
		t.Fatalf("OrphanStats: %v", err)
	}
	if n != 1 || bytes != 1024 {
		t.Fatalf("OrphanStats = (%d, %d), want (1, 1024)", n, bytes)
	}

	res, err = pm.GarbageCollect(p.Id, 0)
	if err != nil {
		t.Fatalf("GarbageCollect: %v", err)
	}
	if res.Scanned != 1 || res.Released != 1 || res.FreedBytes != 1024 {
		t.Fatalf("回收结果 = %+v, want 1 个候选 / 释放 1 个 / 1024 字节", res)
	}

	var got db.Chunk
	if err := env.DB.DB.First(&got, target.Id).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != db.ChunkReserved {
		t.Errorf("回收后 status = %d, want ChunkReserved", got.Status)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("孤儿的磁盘文件应当被删除, err=%v", err)
	}

	// 幂等：没有候选之后再来一轮什么都不做
	if _, err := pm.GarbageCollect(p.Id, 0); err != nil {
		t.Fatal(err)
	}
	res, err = pm.GarbageCollect(p.Id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Released != 0 {
		t.Fatalf("重复回收应当无事可做，实际释放 %d", res.Released)
	}
}

// TestGarbageCollectSkipsReferencedAndPendingWrites：两种"看着像孤儿但不是"的
// 分片都不能回收——还被版本引用的，以及写队列里还有记录（可能正在写）的。
func TestGarbageCollectSkipsReferencedAndPendingWrites(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 2, 1, 4096)

	chunks, err := pm.AddChunks(p.Id, [][]byte{
		testutil.RandBytes(12, 512), testutil.RandBytes(13, 512),
	})
	if err != nil {
		t.Fatal(err)
	}
	// 先把 AddChunks 自己产生的写队列记录消费掉，免得干扰后面的判定
	if err := pm.Flush(chunkIds(chunks)); err != nil {
		t.Fatal(err)
	}

	// chunks[0]：被某个版本引用（version_chunks 有行，不必真有 version/inode）
	if err := env.DB.DB.Create(&db.VersionChunk{
		VersionId: 1, Idx: 0, ChunkId: chunks[0].Id, Size: 512, Hash: []byte("x"),
	}).Error; err != nil {
		t.Fatal(err)
	}
	// chunks[1]：写队列里还有待处理记录
	if err := env.DB.DB.Create(&db.WriteQueue{
		ChunkId: chunks[1].Id, StripeId: chunks[1].StripeId, Status: db.TaskPending,
	}).Error; err != nil {
		t.Fatal(err)
	}

	ageChunks(t, env, chunkIds(chunks))

	n, _, err := pm.OrphanStats(p.Id)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("仍被引用 / 仍在写队列里的分片不该算孤儿，实际 %d 个", n)
	}

	res, err := pm.GarbageCollect(p.Id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Released != 0 {
		t.Fatalf("不该回收任何分片，实际释放 %d", res.Released)
	}
	for _, c := range chunks {
		var got db.Chunk
		if err := env.DB.DB.First(&got, c.Id).Error; err != nil {
			t.Fatal(err)
		}
		if got.Status != db.ChunkAllocated {
			t.Errorf("chunk %d status = %d, want Allocated（不该被动）", c.Id, got.Status)
		}
	}
}

func TestGarbageCollectScopedToPool(t *testing.T) {
	env, pm := newEnv(t)
	p1 := env.NewPool("p1", 2, 1, 4096)
	p2 := env.NewPool("p2", 2, 1, 4096)

	c1, err := pm.AddChunks(p1.Id, [][]byte{testutil.RandBytes(14, 256)})
	if err != nil {
		t.Fatal(err)
	}
	c2, err := pm.AddChunks(p2.Id, [][]byte{testutil.RandBytes(15, 256)})
	if err != nil {
		t.Fatal(err)
	}
	ageChunks(t, env, []int64{c1[0].Id, c2[0].Id})

	// 只扫 p1：p2 的孤儿要留着
	res, err := pm.GarbageCollect(p1.Id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Released != 1 {
		t.Fatalf("p1 应当释放 1 个，实际 %d", res.Released)
	}
	var got db.Chunk
	if err := env.DB.DB.First(&got, c2[0].Id).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != db.ChunkAllocated {
		t.Errorf("p2 的分片不该被释放，status = %d", got.Status)
	}

	// 全库扫描（poolId = 0）才会带上 p2
	res, err = pm.GarbageCollect(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Released != 1 {
		t.Fatalf("全库扫描应当释放 p2 剩下的 1 个，实际 %d", res.Released)
	}
}

func TestGarbageCollectLimit(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 2, 1, 4096)

	chunks, err := pm.AddChunks(p.Id, [][]byte{
		testutil.RandBytes(16, 128), testutil.RandBytes(17, 128),
	})
	if err != nil {
		t.Fatal(err)
	}
	ageChunks(t, env, chunkIds(chunks))

	res, err := pm.GarbageCollect(p.Id, 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Released != 1 {
		t.Fatalf("limit=1 时应当只释放 1 个，实际 %d", res.Released)
	}
}
