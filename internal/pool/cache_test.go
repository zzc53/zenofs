package pool

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/testutil"
)

func TestPickCacheDisk(t *testing.T) {
	disks := []db.Disk{{Id: 11}, {Id: 22}, {Id: 33}}

	if got := pickCacheDisk(nil, 1); got != nil {
		t.Fatalf("没有缓存盘时应返回 nil，得到 %+v", got)
	}

	// 同一 chunk 每次选到同一块盘，不同 chunk 平均分配
	for chunkId, want := range map[int64]int64{0: 11, 1: 22, 2: 33, 3: 11, 4: 22} {
		for i := 0; i < 3; i++ {
			if got := pickCacheDisk(disks, chunkId); got == nil || got.Id != want {
				t.Fatalf("chunk %d 选中 %+v, want disk %d", chunkId, got, want)
			}
		}
	}
}

func TestReadCachePromotesAndEvicts(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 1, 0, 4096)
	cacheDisk := env.NewDisk(p.Id, "cache0", db.CacheDisk)

	data := testutil.RandBytes(7, 4096)
	chunk, err := pm.AddChunk(p.Id, data)
	if err != nil {
		t.Fatal(err)
	}

	// 读够阈值 + 1 次，缓存应该落到缓存盘
	for i := 0; i < int(cachePromoteThreshold)+2; i++ {
		got, err := pm.ReadChunks(p.Id, []int64{chunk.Id})
		if err != nil {
			t.Fatalf("第 %d 次读: %v", i+1, err)
		}
		if !bytes.Equal(got[0], data) {
			t.Fatalf("第 %d 次读内容不一致", i+1)
		}
	}
	var entry db.ReadCache
	if err := env.DB.DB.Where("chunk_id = ?", chunk.Id).First(&entry).Error; err != nil {
		t.Fatalf("没有生成缓存记录: %v", err)
	}
	if entry.Status != db.Cached {
		t.Fatalf("缓存状态 = %d, want Cached", entry.Status)
	}
	if entry.DiskId != cacheDisk.Id {
		t.Fatalf("缓存落在 disk %d, want %d", entry.DiskId, cacheDisk.Id)
	}
	if entry.AccessCount <= cachePromoteThreshold {
		t.Fatalf("访问计数 = %d, 应该超过阈值 %d", entry.AccessCount, cachePromoteThreshold)
	}
	cachedPath := filepath.Join(cacheDisk.Path, entry.Path)
	if _, err := os.Stat(cachedPath); err != nil {
		t.Fatalf("缓存文件不存在: %v", err)
	}

	// 命中缓存后内容仍正确（此时走的是缓存盘）
	got, err := pm.ReadChunks(p.Id, []int64{chunk.Id})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[0], data) {
		t.Fatal("缓存命中的内容不对")
	}

	// 冷却淘汰：把 updated_at 拨回到 TTL 之前（直接用 SQL，避免 gorm 自动更新时间戳）
	old := time.Now().Add(-2 * cacheIdleTTL).Unix()
	if err := env.DB.DB.Exec("UPDATE read_caches SET updated_at = ? WHERE chunk_id = ?", old, chunk.Id).Error; err != nil {
		t.Fatal(err)
	}
	pm.cleanupColdCache()

	var count int64
	if err := env.DB.DB.Model(&db.ReadCache{}).Where("chunk_id = ?", chunk.Id).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("冷却的缓存记录没有被淘汰")
	}
	if _, err := os.Stat(cachedPath); !os.IsNotExist(err) {
		t.Fatalf("冷却的缓存文件没有被删除: %v", err)
	}
	// 缓存文件删掉后源数据照旧可读
	got, err = pm.ReadChunks(p.Id, []int64{chunk.Id})
	if err != nil || !bytes.Equal(got[0], data) {
		t.Fatalf("淘汰缓存后读取失败: %v", err)
	}
}

func TestWriteChunksInvalidatesCache(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 1, 0, 4096)
	cacheDisk := env.NewDisk(p.Id, "cache0", db.CacheDisk)

	old := testutil.RandBytes(8, 1024)
	chunk, err := pm.AddChunk(p.Id, old)
	if err != nil {
		t.Fatal(err)
	}

	// 直接造一条已落盘的缓存记录，再写新数据
	cachePath := filepath.Join("cached", "stale")
	if err := os.MkdirAll(filepath.Join(cacheDisk.Path, "cached"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDisk.Path, cachePath), old, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := env.DB.DB.Create(&db.ReadCache{
		ChunkId: chunk.Id, DiskId: cacheDisk.Id, Path: cachePath,
		AccessCount: cachePromoteThreshold + 1, Status: db.Cached,
	}).Error; err != nil {
		t.Fatal(err)
	}

	fresh := testutil.RandBytes(9, 1024)
	if _, err := pm.WriteChunks([]WriteChunkItem{{ChunkId: chunk.Id, Data: fresh}}); err != nil {
		t.Fatal(err)
	}

	// 记录与缓存文件都要清掉，否则后续读会命中过期内容
	var count int64
	if err := env.DB.DB.Model(&db.ReadCache{}).Where("chunk_id = ?", chunk.Id).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("覆写后缓存记录没有清除")
	}
	if _, err := os.Stat(filepath.Join(cacheDisk.Path, cachePath)); !os.IsNotExist(err) {
		t.Fatalf("覆写后缓存文件没有删除: %v", err)
	}
	got, err := pm.ReadChunks(p.Id, []int64{chunk.Id})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[0], fresh) {
		t.Fatal("覆写后读到的是旧数据")
	}
}

func TestReadChunksFallsBackWhenCacheFileMissing(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 1, 0, 4096)
	cacheDisk := env.NewDisk(p.Id, "cache0", db.CacheDisk)

	data := testutil.RandBytes(10, 512)
	chunk, err := pm.AddChunk(p.Id, data)
	if err != nil {
		t.Fatal(err)
	}
	// 记录说"已缓存"，但文件其实不存在 —— 读缓存失败要回退到源盘
	if err := env.DB.DB.Create(&db.ReadCache{
		ChunkId: chunk.Id, DiskId: cacheDisk.Id, Path: "missing/file",
		AccessCount: cachePromoteThreshold + 1, Status: db.Cached,
	}).Error; err != nil {
		t.Fatal(err)
	}
	got, err := pm.ReadChunks(p.Id, []int64{chunk.Id})
	if err != nil {
		t.Fatalf("缓存文件缺失时应回退源盘读取: %v", err)
	}
	if !bytes.Equal(got[0], data) {
		t.Fatal("回退读取的内容不对")
	}
}
