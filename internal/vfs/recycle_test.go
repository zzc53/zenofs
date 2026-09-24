package vfs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/testutil"
)

// lookupInode 按名字取一个 inode（测试里要拿到 id 做恢复/清除）。
func lookupInode(t *testing.T, env *testutil.Env, shareId int64, name string) db.Inode {
	t.Helper()
	var in db.Inode
	if err := env.DB.DB.Where("share_id = ? AND name = ?", shareId, name).First(&in).Error; err != nil {
		t.Fatalf("查 inode %q: %v", name, err)
	}
	return in
}

func TestRecycleListAndRestore(t *testing.T) {
	_, _, fs, share := newWritable(t)
	ctx := t.Context()
	writeFile(t, fs, "/a.txt", []byte("hello"))

	if err := fs.Remove(ctx, "/a.txt"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := fs.Stat(ctx, "/a.txt"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("删除后 Stat err = %v，期望 ErrNotExist", err)
	}

	entries, err := fs.ListDeleted(ctx)
	if err != nil || len(entries) != 1 {
		t.Fatalf("ListDeleted = %d 条, err=%v", len(entries), err)
	}
	e := entries[0]
	if e.Name != "a.txt" || e.Path != "/a.txt" || e.Kind != KindFile || e.Size != 5 {
		t.Fatalf("回收站条目 = %+v", e)
	}
	if e.DeletedBy != fs.userID || e.DeletedAt == 0 {
		t.Fatalf("删除者/时间不对: %+v", e)
	}
	_ = share

	info, err := fs.Restore(ctx, e.Id)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if info.Path != "/a.txt" || info.Kind != KindFile {
		t.Fatalf("恢复后元数据 = %+v", info)
	}
	if got := readFile(t, fs, "/a.txt", 5); string(got) != "hello" {
		t.Fatalf("恢复后内容 = %q", got)
	}
	if left, err := fs.ListDeleted(ctx); err != nil || len(left) != 0 {
		t.Fatalf("恢复后回收站应清空: %d 条, err=%v", len(left), err)
	}

	// 没被删过的条目不能"恢复"，不存在的条目报 NotExist
	if _, err := fs.Restore(ctx, info.Id); !errors.Is(err, ErrInvalid) {
		t.Fatalf("恢复未删除条目 err = %v，期望 ErrInvalid", err)
	}
	if _, err := fs.Restore(ctx, 9999); !errors.Is(err, ErrNotExist) {
		t.Fatalf("恢复不存在条目 err = %v，期望 ErrNotExist", err)
	}
}

func TestRestoreConflictAndOrphan(t *testing.T) {
	_, _, fs, _ := newWritable(t)
	ctx := t.Context()

	// 同名冲突：删掉 a.txt 后又建了一个同名文件
	writeFile(t, fs, "/a.txt", []byte("one"))
	if err := fs.Remove(ctx, "/a.txt"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, fs, "/a.txt", []byte("two"))

	entries, err := fs.ListDeleted(ctx)
	if err != nil || len(entries) != 1 {
		t.Fatalf("回收站 = %d 条, err=%v", len(entries), err)
	}
	if _, err := fs.Restore(ctx, entries[0].Id); !errors.Is(err, ErrExist) {
		t.Fatalf("同名冲突 err = %v，期望 ErrExist", err)
	}

	// 父目录也被删：恢复到 Share 根（先删文件、再删空目录是常见顺序）
	if err := fs.Mkdir(ctx, "/dir", 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, fs, "/dir/b.txt", []byte("bb"))
	if err := fs.Remove(ctx, "/dir/b.txt"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Remove(ctx, "/dir"); err != nil {
		t.Fatalf("删除空目录: %v", err)
	}

	entries, err = fs.ListDeleted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var orphan *DeletedEntry
	for i := range entries {
		if entries[i].Name == "b.txt" {
			orphan = &entries[i]
		}
	}
	if orphan == nil {
		t.Fatalf("回收站里没有 b.txt: %+v", entries)
	}
	if orphan.Path != "/dir/b.txt" {
		t.Fatalf("删除前的路径 = %q，期望 /dir/b.txt", orphan.Path)
	}
	info, err := fs.Restore(ctx, orphan.Id)
	if err != nil {
		t.Fatalf("恢复孤儿: %v", err)
	}
	if info.Path != "/b.txt" {
		t.Fatalf("父目录已删时应恢复到根，实际 %q", info.Path)
	}
	if got := readFile(t, fs, "/b.txt", 2); string(got) != "bb" {
		t.Fatalf("恢复后内容 = %q", got)
	}
}

func TestPurgeRemovesRecords(t *testing.T) {
	env, _, fs, share := newWritable(t)
	ctx := t.Context()

	writeFile(t, fs, "/a.txt", []byte("hello"))
	inode := lookupInode(t, env, share.Id, "a.txt")
	if err := fs.Remove(ctx, "/a.txt"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Purge(ctx, inode.Id); err != nil {
		t.Fatalf("Purge: %v", err)
	}

	var n int64
	env.DB.DB.Model(&db.Inode{}).Where("id = ?", inode.Id).Count(&n)
	if n != 0 {
		t.Fatalf("inode 记录未删除")
	}
	env.DB.DB.Model(&db.Version{}).Where("inode_id = ?", inode.Id).Count(&n)
	if n != 0 {
		t.Fatalf("version 记录未删除")
	}
	if entries, err := fs.ListDeleted(ctx); err != nil || len(entries) != 0 {
		t.Fatalf("清除后回收站 = %d 条, err=%v", len(entries), err)
	}
	if err := fs.Purge(ctx, inode.Id); !errors.Is(err, ErrNotExist) {
		t.Fatalf("重复 Purge err = %v，期望 ErrNotExist", err)
	}
}

func TestPurgeAllAndPermission(t *testing.T) {
	_, pm, fs, share := newWritable(t)
	ctx := t.Context()

	for _, name := range []string{"/a.txt", "/b.txt"} {
		writeFile(t, fs, name, []byte("x"))
		if err := fs.Remove(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	n, err := fs.PurgeAll(ctx)
	if err != nil || n != 2 {
		t.Fatalf("PurgeAll = %d 条, err=%v", n, err)
	}
	if entries, _ := fs.ListDeleted(ctx); len(entries) != 0 {
		t.Fatalf("清空后仍有 %d 条", len(entries))
	}

	// 只读挂载点：恢复与清除都要被拒
	ro := NewShareFS(pm, share, fs.userID, db.ShareRead)
	if _, err := ro.Restore(ctx, 1); !errors.Is(err, ErrPermission) {
		t.Fatalf("只读挂载 Restore err = %v，期望 ErrPermission", err)
	}
	if err := ro.Purge(ctx, 1); !errors.Is(err, ErrPermission) {
		t.Fatalf("只读挂载 Purge err = %v，期望 ErrPermission", err)
	}
	if _, err := ro.PurgeAll(ctx); !errors.Is(err, ErrPermission) {
		t.Fatalf("只读挂载 PurgeAll err = %v，期望 ErrPermission", err)
	}
}

// chunksOfVersion 返回某个 inode 最新一个版本引用到的 chunk。
func chunksOfVersion(t *testing.T, env *testutil.Env, inodeId int64) []db.Chunk {
	t.Helper()
	var v db.Version
	if err := env.DB.DB.Where("inode_id = ?", inodeId).Order("id DESC").First(&v).Error; err != nil {
		t.Fatalf("查版本: %v", err)
	}
	var ids []int64
	if err := env.DB.DB.Model(&db.VersionChunk{}).Where("version_id = ?", v.Id).
		Pluck("chunk_id", &ids).Error; err != nil {
		t.Fatalf("查版本切片: %v", err)
	}
	var out []db.Chunk
	if err := env.DB.DB.Where("id IN ?", ids).Find(&out).Error; err != nil {
		t.Fatalf("查 chunk: %v", err)
	}
	return out
}

// chunkAbsPath 返回 chunk 落在磁盘上的绝对路径。
func chunkAbsPath(t *testing.T, env *testutil.Env, c db.Chunk) string {
	t.Helper()
	var disk db.Disk
	if err := env.DB.DB.First(&disk, c.DiskId).Error; err != nil {
		t.Fatalf("查 chunk %d 的磁盘: %v", c.Id, err)
	}
	return filepath.Join(disk.Path, c.Path)
}

// TestPurgeReleasesUnreferencedChunks：彻底删除要把没人引用的分片真的从盘上删掉，
// 并把槽位回滚成空槽交还存储池（原来只删元数据，盘上的数据一直留着）。
func TestPurgeReleasesUnreferencedChunks(t *testing.T) {
	env, _, fs, share := newWritable(t)
	ctx := t.Context()

	writeFile(t, fs, "/a.txt", []byte("hello"))
	inode := lookupInode(t, env, share.Id, "a.txt")
	chunks := chunksOfVersion(t, env, inode.Id)
	if len(chunks) == 0 {
		t.Fatal("写入后应当有 chunk")
	}
	oldPaths := make(map[int64]string, len(chunks))
	for _, c := range chunks {
		oldPaths[c.Id] = chunkAbsPath(t, env, c)
		if _, err := os.Stat(oldPaths[c.Id]); err != nil {
			t.Fatalf("写入后 chunk 文件应存在: %v", err)
		}
	}

	if err := fs.Remove(ctx, "/a.txt"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Purge(ctx, inode.Id); err != nil {
		t.Fatalf("Purge: %v", err)
	}

	for _, c := range chunks {
		// chunk 行要留着：条带的槽位布局是分配与重建的前提
		var got db.Chunk
		if err := env.DB.DB.First(&got, c.Id).Error; err != nil {
			t.Fatalf("chunk 行不该被删除: %v", err)
		}
		if got.Status != db.ChunkReserved {
			t.Errorf("chunk %d status = %d, want ChunkReserved", c.Id, got.Status)
		}
		if got.Size != 0 || len(got.Hash) != 0 {
			t.Errorf("chunk %d 未清空 size/hash: size=%d hash=%x", c.Id, got.Size, got.Hash)
		}
		if got.Path == c.Path {
			t.Errorf("chunk %d 的 path 未更换（并发复用后会误删新数据）", c.Id)
		}
		if _, err := os.Stat(oldPaths[c.Id]); !os.IsNotExist(err) {
			t.Errorf("chunk %d 的旧文件应当被删除, err=%v", c.Id, err)
		}
	}

	var n int64
	if err := env.DB.DB.Model(&db.Version{}).Where("inode_id = ?", inode.Id).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("version 记录未删除: %d", n)
	}
}

// TestPurgeKeepsChunksStillReferenced：只要还有别的版本引用同一个分片，就不能释放它。
func TestPurgeKeepsChunksStillReferenced(t *testing.T) {
	env, _, fs, share := newWritable(t)
	ctx := t.Context()

	writeFile(t, fs, "/a.txt", []byte("keepme"))
	writeFile(t, fs, "/b.txt", []byte("other"))
	aInode := lookupInode(t, env, share.Id, "a.txt")
	bInode := lookupInode(t, env, share.Id, "b.txt")

	aChunks := chunksOfVersion(t, env, aInode.Id)
	if len(aChunks) != 1 {
		t.Fatalf("a.txt 有 %d 个 chunk, want 1", len(aChunks))
	}
	target := aChunks[0]
	targetFile := chunkAbsPath(t, env, target)

	// 手工让 b.txt 的版本也引用 a.txt 的分片：现有写路径只在同一 inode 内共享
	// 切片（写时复制），跨文件的共享不会自然产生，所以直接造库。
	var bVer db.Version
	if err := env.DB.DB.Where("inode_id = ?", bInode.Id).Order("id DESC").First(&bVer).Error; err != nil {
		t.Fatal(err)
	}
	if err := env.DB.DB.Create(&db.VersionChunk{
		VersionId: bVer.Id, Idx: 99, ChunkId: target.Id, Size: target.Size, Hash: target.Hash,
	}).Error; err != nil {
		t.Fatal(err)
	}

	if err := fs.Remove(ctx, "/a.txt"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Purge(ctx, aInode.Id); err != nil {
		t.Fatalf("Purge: %v", err)
	}

	var got db.Chunk
	if err := env.DB.DB.First(&got, target.Id).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != db.ChunkAllocated {
		t.Errorf("仍被别的版本引用的 chunk 不该被释放，status = %d", got.Status)
	}
	if got.Path != target.Path || got.Size != target.Size {
		t.Errorf("被引用的 chunk 元数据不该被改动: path=%q size=%d", got.Path, got.Size)
	}
	if _, err := os.Stat(targetFile); err != nil {
		t.Errorf("被引用的 chunk 文件不该被删除: %v", err)
	}
}

// TestPurgeShareReleasesChunks：删除整个 Share 时它的分片也要真的从盘上消失
// （以前只删元数据，整个 Share 的数据会永久留在盘上）。
func TestPurgeShareReleasesChunks(t *testing.T) {
	env, pm, fs, share := newWritable(t)

	writeFile(t, fs, "/a.txt", []byte("hello"))
	inode := lookupInode(t, env, share.Id, "a.txt")
	chunks := chunksOfVersion(t, env, inode.Id)
	if len(chunks) == 0 {
		t.Fatal("写入后应当有 chunk")
	}
	oldPaths := make(map[int64]string, len(chunks))
	for _, c := range chunks {
		oldPaths[c.Id] = chunkAbsPath(t, env, c)
	}

	if _, err := PurgeShare(pm, share.Id); err != nil {
		t.Fatalf("PurgeShare: %v", err)
	}

	for _, c := range chunks {
		var got db.Chunk
		if err := env.DB.DB.First(&got, c.Id).Error; err != nil {
			t.Fatal(err)
		}
		if got.Status != db.ChunkReserved {
			t.Errorf("chunk %d 未回收: status = %d", c.Id, got.Status)
		}
		if _, err := os.Stat(oldPaths[c.Id]); !os.IsNotExist(err) {
			t.Errorf("chunk %d 的文件应当被删除, err=%v", c.Id, err)
		}
	}

	var n int64
	if err := env.DB.DB.Model(&db.Inode{}).Where("share_id = ?", share.Id).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("inode 记录未删除: %d", n)
	}
	if err := env.DB.DB.Model(&db.VersionChunk{}).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("version_chunk 记录未删除: %d", n)
	}
}
