package pool

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/testutil"
	"gorm.io/gorm"
)

// releaseInTx 按调用方（vfs.Purge / GC）的方式走一遍释放：
// 事务内回滚槽位，提交后删文件。返回事务里产出的待删文件列表。
func releaseInTx(t *testing.T, pm *PoolManager, ids []int64) []StaleFile {
	t.Helper()
	var res ReleaseResult
	err := pm.DbManager.Tx(func(tx *gorm.DB) error {
		var err error
		res, err = pm.ReleaseChunks(tx, ids)
		return err
	})
	if err != nil {
		t.Fatalf("ReleaseChunks: %v", err)
	}
	pm.DeleteStaleFiles(res.Stale)
	return res.Stale
}

func TestReleaseRollsBackSlotAndKeepsParity(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 2, 1, 4096)

	chunks, err := pm.AddChunks(p.Id, [][]byte{
		testutil.RandBytes(1, 4096), testutil.RandBytes(2, 4096),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := pm.Flush(chunkIds(chunks)); err != nil {
		t.Fatal(err)
	}
	if !pm.calculateStripeParity() {
		t.Fatal("calculateStripeParity 没有算 parity")
	}

	target := chunks[0]
	oldFile := chunkFile(t, env, target)
	if _, err := os.Stat(oldFile); err != nil {
		t.Fatalf("写入后 chunk 文件应存在: %v", err)
	}

	stale := releaseInTx(t, pm, []int64{target.Id})
	if len(stale) == 0 {
		t.Fatal("释放应当返回待删除的旧文件")
	}

	// 元数据回滚成预分配空槽：状态、大小、哈希、路径都要变
	var got db.Chunk
	if err := env.DB.DB.First(&got, target.Id).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != db.ChunkReserved {
		t.Errorf("status = %d, want ChunkReserved", got.Status)
	}
	if got.Size != 0 {
		t.Errorf("size = %d, want 0", got.Size)
	}
	if len(got.Hash) != 0 {
		t.Errorf("hash 未清空: %x", got.Hash)
	}
	if got.Path == target.Path {
		t.Error("path 未更换：并发复用槽位后可能误删新写入的数据")
	}
	// 旧文件必须消失，且新路径上不该有任何文件
	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Errorf("旧 chunk 文件应当被删除, err=%v", err)
	}
	if _, err := os.Stat(chunkFile(t, env, got)); !os.IsNotExist(err) {
		t.Errorf("新路径上不该有文件, err=%v", err)
	}

	// 条带里还有另一个已写入 data：parity 必须保留（恢复能力优先）并排队重算
	data, parity := stripeChunks(t, env, target.StripeId)
	if len(data) != 1 {
		t.Errorf("条带里已写入 data = %d, want 1（回滚的那个应被排除）", len(data))
	}
	if len(parity) != 1 || parity[0].Status != db.ChunkAllocated {
		t.Errorf("仍有数据的条带不该回滚 parity: %+v", parity)
	}
	var queued int64
	if err := env.DB.DB.Model(&db.StripeQueue{}).
		Where("stripe_id = ? AND type = ?", target.StripeId, db.StripeQueueParity).
		Count(&queued).Error; err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Errorf("parity 重算任务 = %d, want 1（陈旧 parity 会解出错误的旧数据）", queued)
	}
}

func TestReleaseFreesParityOfEmptyStripe(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 2, 1, 4096)

	chunks, err := pm.AddChunks(p.Id, [][]byte{
		testutil.RandBytes(3, 4096), testutil.RandBytes(4, 4096),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := pm.Flush(chunkIds(chunks)); err != nil {
		t.Fatal(err)
	}
	if !pm.calculateStripeParity() {
		t.Fatal("calculateStripeParity 没有算 parity")
	}
	_, parity := stripeChunks(t, env, chunks[0].StripeId)
	if len(parity) != 1 {
		t.Fatalf("条带里有 %d 个 parity, want 1", len(parity))
	}
	stripeId := chunks[0].StripeId
	parityFile := chunkFile(t, env, parity[0])

	// 释放该条带全部 data chunk —— 条带空了
	stale := releaseInTx(t, pm, chunkIds(chunks))

	var got db.Chunk
	if err := env.DB.DB.First(&got, parity[0].Id).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != db.ChunkReserved {
		t.Errorf("空条带的 parity status = %d, want ChunkReserved", got.Status)
	}
	if got.Size != 0 || len(got.Hash) != 0 {
		t.Errorf("空条带的 parity 应当清空 size/hash，实际 size=%d hash=%x", got.Size, got.Hash)
	}
	// 关键：parity 文件必须被删掉，否则被删内容的痕迹会永久留在盘上
	// （worker 不会重算没有 data 的条带）
	if _, err := os.Stat(parityFile); !os.IsNotExist(err) {
		t.Errorf("空条带的 parity 文件应当被删除, err=%v", err)
	}
	wantStale := len(chunks) + len(parity)
	if len(stale) != wantStale {
		t.Errorf("待删文件 = %d, want %d（data + parity）", len(stale), wantStale)
	}

	// 没有 data 可算，就不该投递重算任务
	var queued int64
	if err := env.DB.DB.Model(&db.StripeQueue{}).
		Where("stripe_id = ? AND type = ?", stripeId, db.StripeQueueParity).
		Count(&queued).Error; err != nil {
		t.Fatal(err)
	}
	if queued != 0 {
		t.Errorf("空条带不该有 parity 重算任务, 实际 %d", queued)
	}
}

func TestReleaseIsIdempotentAndSkipsParity(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 2, 1, 4096)

	chunks, err := pm.AddChunks(p.Id, [][]byte{testutil.RandBytes(5, 512)})
	if err != nil {
		t.Fatal(err)
	}
	if err := pm.Flush(chunkIds(chunks)); err != nil {
		t.Fatal(err)
	}
	if !pm.calculateStripeParity() {
		t.Fatal("calculateStripeParity 没有算 parity")
	}
	_, parity := stripeChunks(t, env, chunks[0].StripeId)

	// parity chunk 即使被传进来也不该被动：它没有 version_chunks 引用，
	// 只按"无引用"判断会把它误放。
	if stale := releaseInTx(t, pm, []int64{parity[0].Id}); len(stale) != 0 {
		t.Errorf("parity 不该被释放，实际返回 %d 个待删文件", len(stale))
	}
	var after db.Chunk
	if err := env.DB.DB.First(&after, parity[0].Id).Error; err != nil {
		t.Fatal(err)
	}
	if after.Status == db.ChunkReserved {
		t.Error("parity 被误回滚了")
	}

	// 同一个 data chunk 释放两次：第二次是空操作
	if stale := releaseInTx(t, pm, []int64{chunks[0].Id}); len(stale) == 0 {
		t.Fatal("首次释放应当产出待删文件")
	}
	if stale := releaseInTx(t, pm, []int64{chunks[0].Id}); len(stale) != 0 {
		t.Errorf("重复释放应当无事可做，实际返回 %d 个", len(stale))
	}
}

func TestReleaseDropsCacheAndWriteQueue(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 2, 1, 4096)
	cacheDisk := env.NewDisk(p.Id, "cache", db.CacheDisk)

	chunks, err := pm.AddChunks(p.Id, [][]byte{testutil.RandBytes(6, 2048)})
	if err != nil {
		t.Fatal(err)
	}
	c := chunks[0]

	// 造一个已落盘的读缓存副本（真实文件 + Cached 记录）
	rel := "cache/entry"
	abs := filepath.Join(cacheDisk.Path, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte("cached"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := env.DB.DB.Create(&db.ReadCache{
		ChunkId: c.Id, Path: rel, DiskId: cacheDisk.Id, Status: db.Cached,
	}).Error; err != nil {
		t.Fatal(err)
	}
	// 再加一条写队列残留（模拟 Flush 还没搬运）
	if err := env.DB.DB.Create(&db.WriteQueue{
		ChunkId: c.Id, StripeId: c.StripeId, Status: db.TaskPending,
	}).Error; err != nil {
		t.Fatal(err)
	}

	releaseInTx(t, pm, []int64{c.Id})

	var n int64
	if err := env.DB.DB.Model(&db.ReadCache{}).Where("chunk_id = ?", c.Id).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("读缓存记录未清理: %d 条", n)
	}
	if err := env.DB.DB.Model(&db.WriteQueue{}).Where("chunk_id = ?", c.Id).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("写队列残留未清理: %d 条", n)
	}
	if _, err := os.Stat(abs); !os.IsNotExist(err) {
		t.Errorf("缓存副本文件应当被删除, err=%v", err)
	}
}

func TestReleaseEmptyInput(t *testing.T) {
	_, pm := newEnv(t)
	if stale := releaseInTx(t, pm, nil); len(stale) != 0 {
		t.Errorf("空输入应当无事可做，实际返回 %d 个", len(stale))
	}
}
