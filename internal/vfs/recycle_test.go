package vfs

import (
	"errors"
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
