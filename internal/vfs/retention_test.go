package vfs

import (
	"testing"
	"time"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/testutil"
)

// ageInode 把 inode 的 updated_at 挪到 n 小时前（TTL 判据就是它）。
// 用 UpdateColumns：更新走 Updates 时 GORM 会用 autoUpdateTime 覆盖掉这个值。
func ageInode(t *testing.T, env *testutil.Env, inodeId int64, hours int64) {
	t.Helper()
	ts := time.Now().Unix() - hours*3600
	if err := env.DB.DB.Model(&db.Inode{}).Where("id = ?", inodeId).
		UpdateColumns(map[string]any{"updated_at": ts}).Error; err != nil {
		t.Fatal(err)
	}
}

// TestSweepRecycleRemovesExpiredTrash：回收站 TTL 只对开了 TTL 的 Share 生效。
func TestSweepRecycleRemovesExpiredTrash(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 2, 1, 4096)
	ctx := t.Context()

	ttlFS, ttlShare := newShareFS(t, env, pm, p.Id, testutil.ShareOpts{
		Name: "ttl", UserID: 1, Permission: db.ShareWrite, RecycleTtlHours: 1,
	})
	keepFS, keepShare := newShareFS(t, env, pm, p.Id, testutil.ShareOpts{
		Name: "keep", UserID: 1, Permission: db.ShareWrite, // TTL = 0：不自动清除
	})

	writeFile(t, ttlFS, "/expired.txt", []byte("bye"))
	writeFile(t, keepFS, "/keep.txt", []byte("hi"))
	if err := ttlFS.Remove(ctx, "/expired.txt"); err != nil {
		t.Fatal(err)
	}
	if err := keepFS.Remove(ctx, "/keep.txt"); err != nil {
		t.Fatal(err)
	}

	expired := lookupInode(t, env, ttlShare.Id, "expired.txt")
	kept := lookupInode(t, env, keepShare.Id, "keep.txt")
	expiredChunks := chunksOfVersion(t, env, expired.Id)
	if len(expiredChunks) == 0 {
		t.Fatal("写入后应当有 chunk")
	}
	// 两个都"删了很久"，但只有一个 Share 开了 TTL
	ageInode(t, env, expired.Id, 2)
	ageInode(t, env, kept.Id, 2)

	n, err := SweepRecycle(pm, 0)
	if err != nil {
		t.Fatalf("SweepRecycle: %v", err)
	}
	if n != 1 {
		t.Fatalf("清掉 %d 条, want 1", n)
	}

	// 超期条目：元数据没了，分片也回收了
	var count int64
	if err := env.DB.DB.Model(&db.Inode{}).Where("id = ?", expired.Id).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Error("超期条目应当被彻底删除")
	}
	for _, c := range expiredChunks {
		var got db.Chunk
		if err := env.DB.DB.First(&got, c.Id).Error; err != nil {
			t.Fatalf("查 chunk %d: %v", c.Id, err)
		}
		if got.Status != db.ChunkReserved {
			t.Errorf("超期条目的 chunk %d 应当被回收成空槽，status = %d", c.Id, got.Status)
		}
	}
	// 没开 TTL 的 Share：原样不动
	if err := env.DB.DB.Model(&db.Inode{}).Where("id = ?", kept.Id).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Error("没开 TTL 的 Share 不该被清理")
	}
}

// TestSweepVersionsTrimsHistory：版本上限把多余的历史版本连同分片一起收掉。
func TestSweepVersionsTrimsHistory(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 2, 1, 4096)
	fs, share := newShareFS(t, env, pm, p.Id, testutil.ShareOpts{
		Name: "s", UserID: 1, Permission: db.ShareWrite, VersionKeep: 2,
	})

	writeFile(t, fs, "/v.txt", []byte("one"))
	overwriteFile(t, fs, "/v.txt", []byte("two-222"))
	overwriteFile(t, fs, "/v.txt", []byte("three-3"))

	inode := lookupInode(t, env, share.Id, "v.txt")
	var before int64
	if err := env.DB.DB.Model(&db.Version{}).Where("inode_id = ?", inode.Id).
		Count(&before).Error; err != nil {
		t.Fatal(err)
	}
	if before != 3 {
		t.Fatalf("前置条件：应有 3 个版本，实际 %d", before)
	}

	n, err := SweepVersions(pm, 0)
	if err != nil {
		t.Fatalf("SweepVersions: %v", err)
	}
	if n != 1 {
		t.Fatalf("裁掉 %d 个版本, want 1", n)
	}

	var after int64
	if err := env.DB.DB.Model(&db.Version{}).Where("inode_id = ?", inode.Id).
		Count(&after).Error; err != nil {
		t.Fatal(err)
	}
	if after != 2 {
		t.Fatalf("裁剪后应剩 2 个版本，实际 %d", after)
	}

	// 内容仍是当前版本，且当前版本那一行还在
	if got := readFile(t, fs, "/v.txt", 7); string(got) != "three-3" {
		t.Errorf("裁剪后内容 = %q, want %q", got, "three-3")
	}
	var fresh db.Inode
	if err := env.DB.DB.First(&fresh, inode.Id).Error; err != nil {
		t.Fatal(err)
	}
	var current int64
	if err := env.DB.DB.Model(&db.Version{}).Where("id = ?", fresh.VersionId.Int64).
		Count(&current).Error; err != nil {
		t.Fatal(err)
	}
	if current != 1 {
		t.Error("当前版本被裁掉了")
	}
}

// TestSweepVersionsKeepsRestoredCurrentVersion：当前版本不一定是 id 最大的那个
// （RestoreVersion 会把旧版本指回去），裁剪必须认指针而不是"留最新的 N 个"。
func TestSweepVersionsKeepsRestoredCurrentVersion(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 2, 1, 4096)
	fs, share := newShareFS(t, env, pm, p.Id, testutil.ShareOpts{
		Name: "s", UserID: 1, Permission: db.ShareWrite, VersionKeep: 1,
	})
	ctx := t.Context()

	writeFile(t, fs, "/v.txt", []byte("v1"))
	overwriteFile(t, fs, "/v.txt", []byte("v222"))
	overwriteFile(t, fs, "/v.txt", []byte("v33333"))

	inode := lookupInode(t, env, share.Id, "v.txt")
	var versions []db.Version
	if err := env.DB.DB.Where("inode_id = ?", inode.Id).Order("id").Find(&versions).Error; err != nil {
		t.Fatal(err)
	}
	if len(versions) != 3 {
		t.Fatalf("前置条件：应有 3 个版本，实际 %d", len(versions))
	}
	first := versions[0].Id

	if _, err := fs.RestoreVersion(ctx, first); err != nil {
		t.Fatalf("RestoreVersion: %v", err)
	}
	if _, err := SweepVersions(pm, 0); err != nil {
		t.Fatalf("SweepVersions: %v", err)
	}

	var fresh db.Inode
	if err := env.DB.DB.First(&fresh, inode.Id).Error; err != nil {
		t.Fatal(err)
	}
	if fresh.VersionId.Int64 != first {
		t.Fatalf("当前版本指针 = %d, want %d", fresh.VersionId.Int64, first)
	}
	if got := readFile(t, fs, "/v.txt", 2); string(got) != "v1" {
		t.Errorf("回滚后的内容 = %q, want v1（当前版本被误裁）", got)
	}
	var left int64
	if err := env.DB.DB.Model(&db.Version{}).Where("inode_id = ?", inode.Id).
		Count(&left).Error; err != nil {
		t.Fatal(err)
	}
	if left != 1 {
		t.Errorf("保留 %d 个版本, want 1", left)
	}
}
