package vfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sort"
	"sync"
	"testing"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/testutil"
)

// newRootEnv 建一个池与若干 Share，返回绑定 user 1 的 RootFS。
func newRootEnv(t *testing.T) (*testutil.Env, *RootFS) {
	t.Helper()
	env, pm := newEnv(t)
	p := env.NewPool("p", 2, 1, 8192)
	env.NewShare(p.Id, testutil.ShareOpts{
		Name: "work", UserID: 1, Permission: db.ShareWrite,
		Compression: CompressionZstd, SliceSizeKB: 1024,
	})
	env.NewShare(p.Id, testutil.ShareOpts{
		Name: "pub", UserID: 1, Permission: db.ShareRead, Compression: CompressionZstd,
	})
	// 别人的 Share：user 1 没有授权
	env.NewShare(p.Id, testutil.ShareOpts{
		Name: "secret", UserID: 2, Permission: db.ShareWrite,
	})
	return env, NewRootFS(pm, 1)
}

func rootDirNames(t *testing.T, root *RootFS, path string) []string {
	t.Helper()
	entries, err := root.ReadDir(context.Background(), path)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", path, err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name
	}
	sort.Strings(names)
	return names
}

func TestRootReadDirListsVisibleShares(t *testing.T) {
	env, root := newRootEnv(t)
	ctx := context.Background()

	// 名字不能作为目录名的 Share 没有可寻址路径，根下隐藏
	var pool db.Pool
	if err := env.DB.DB.First(&pool).Error; err != nil {
		t.Fatal(err)
	}
	env.NewShare(pool.Id, testutil.ShareOpts{
		Name: "bad/name", UserID: 1, Permission: db.ShareWrite,
	})

	entries, err := root.ReadDir(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	names := rootDirNames(t, root, "/")
	if len(names) != 2 || names[0] != "pub" || names[1] != "work" {
		t.Fatalf("ReadDir(/) = %v, want [pub work]（未授权与非法名都要隐藏）", names)
	}

	for _, fi := range entries {
		if fi.Kind != KindDir {
			t.Errorf("%s: kind = %v, want dir", fi.Name, fi.Kind)
		}
		if fi.Path != "/"+fi.Name || fi.Name == "/" {
			t.Errorf("%s: path = %q", fi.Name, fi.Path)
		}
		// 根层条目的 Id 取 -shares.id：与 Share 内 inode 编号不会撞
		if fi.Id >= 0 {
			t.Errorf("%s: id = %d, want 负数", fi.Name, fi.Id)
		}
		if !fi.Mode.IsDir() || fi.Mode.Perm()&0o111 == 0 {
			t.Errorf("%s: mode = %v, 目录必须带 x 位", fi.Name, fi.Mode)
		}
		// 只读 Share 的目录不给写位
		wantWritable := fi.Name == "work"
		if got := fi.Mode.Perm()&0o200 != 0; got != wantWritable {
			t.Errorf("%s: mode = %v, 写位与 share_users.permission 不符", fi.Name, fi.Mode)
		}
	}

	// 根是只读的合成目录
	fi, err := root.Stat(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Kind != KindDir || fi.Id != 0 || fi.Path != "/" {
		t.Fatalf("根条目 = %+v", fi)
	}
	if fi.Mode.Perm()&0o222 != 0 || fi.Mode.Perm()&0o555 != 0o555 {
		t.Fatalf("根权限 = %v, want 0555（只读视图）", fi.Mode)
	}

	if _, err := root.Stat(ctx, "/pub"); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Lstat(ctx, "/work"); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Stat(ctx, "/secret"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("未授权的 Share = %v, want ErrNotExist", err)
	}
	if _, err := root.Stat(ctx, "/bad/name"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("非法名 Share = %v, want ErrNotExist", err)
	}
	if _, err := root.Stat(ctx, "/nosuch"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("不存在的名字 = %v, want ErrNotExist", err)
	}

	// 回收授权后立刻不可见（每次解析路径都重新确认）
	if err := env.DB.DB.Where("user_id = ?", 1).Delete(&db.ShareUser{}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := root.Stat(ctx, "/work"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("授权回收后 = %v, want ErrNotExist", err)
	}
	if names := rootDirNames(t, root, "/"); len(names) != 0 {
		t.Fatalf("授权回收后 ReadDir(/) = %v, want 空", names)
	}
}

func TestRootForwardsToShare(t *testing.T) {
	_, root := newRootEnv(t)
	ctx := context.Background()

	// 写入 /work/a.txt
	payload := []byte("hello root aggregation")
	f, err := root.Create(ctx, "/work/a.txt", 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	rf, err := root.Open(ctx, "/work/a.txt", OpenFlags{Read: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(payload))
	if _, err := rf.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	if err := rf.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, payload) {
		t.Fatalf("读回 = %q", buf)
	}

	// 目录 / 改名 / 复制 / 符号链接 / 删除 / 属性
	if err := root.Mkdir(ctx, "/work/dir", 0755); err != nil {
		t.Fatal(err)
	}
	if err := root.Rename(ctx, "/work/a.txt", "/work/dir/b.txt"); err != nil {
		t.Fatal(err)
	}
	if err := root.Copy(ctx, "/work/dir/b.txt", "/work/c.txt", false); err != nil {
		t.Fatal(err)
	}
	if err := root.Symlink(ctx, "/work/c.txt", "/work/link"); err != nil {
		t.Fatal(err)
	}
	target, err := root.Readlink(ctx, "/work/link")
	if err != nil {
		t.Fatal(err)
	}
	if target != "/work/c.txt" {
		t.Fatalf("Readlink = %q, want /work/c.txt（要带挂载点前缀）", target)
	}
	lfi, err := root.Lstat(ctx, "/work/link")
	if err != nil {
		t.Fatal(err)
	}
	if lfi.Kind != KindSymlink || lfi.Target != "/work/c.txt" {
		t.Fatalf("Lstat = %+v, Target 也要带挂载点前缀", lfi)
	}
	sfi, err := root.Stat(ctx, "/work/link")
	if err != nil {
		t.Fatal(err)
	}
	if sfi.Kind != KindFile || sfi.Size != int64(len(payload)) {
		t.Fatalf("Stat 跟随链接 = %+v", sfi)
	}

	entries, err := root.ReadDir(ctx, "/work")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("ReadDir(/work) = %d 条, want 3（dir/c.txt/link）", len(entries))
	}
	for _, e := range entries {
		if e.Kind == KindSymlink && e.Target != "/work/c.txt" {
			t.Fatalf("ReadDir 里链接的 Target = %q", e.Target)
		}
	}

	if err := root.Remove(ctx, "/work/c.txt"); err != nil {
		t.Fatal(err)
	}
	exec := os.FileMode(0755)
	if err := root.SetAttr(ctx, "/work/dir", Attrs{Mode: &exec}); err != nil {
		t.Fatal(err)
	}

	// 只读 Share：读没问题，写被 ShareFS 拒掉
	if _, err := root.ReadDir(ctx, "/pub"); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Create(ctx, "/pub/x.txt", 0644); !errors.Is(err, ErrPermission) {
		t.Fatalf("只读 Share 写入 = %v, want ErrPermission", err)
	}
	if err := root.Mkdir(ctx, "/pub/d", 0755); !errors.Is(err, ErrPermission) {
		t.Fatalf("只读 Share Mkdir = %v, want ErrPermission", err)
	}

	// 挂载点内的错误照旧透出
	if _, err := root.Stat(ctx, "/work/nope"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Share 内不存在 = %v, want ErrNotExist", err)
	}

	// StatFS：根汇总已用量，Share 内转发（pub 是空的，两者应相等）
	fsRoot, err := root.StatFS(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	fsWork, err := root.StatFS(ctx, "/work")
	if err != nil {
		t.Fatal(err)
	}
	if fsRoot.UsedBytes != fsWork.UsedBytes || fsRoot.UsedBytes == 0 {
		t.Fatalf("根已用量 = %d, /work = %d", fsRoot.UsedBytes, fsWork.UsedBytes)
	}
	if fsRoot.TotalBytes != 0 {
		t.Fatalf("根没有统一配额，TotalBytes = %d, want 0", fsRoot.TotalBytes)
	}

	// 直接取单个 Share
	mounted, err := root.Mount("work")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mounted.Stat(ctx, "/dir"); err != nil {
		t.Fatalf("Mount 后直接操作 Share: %v", err)
	}
	share, perm, err := root.Share("work")
	if err != nil {
		t.Fatal(err)
	}
	if share.Name != "work" || perm != db.ShareWrite {
		t.Fatalf("Share(work) = (%+v, %v)", share, perm)
	}
	if _, _, err := root.Share("secret"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("未授权的 Share = %v, want ErrNotExist", err)
	}
}

func TestRootReadOnlyView(t *testing.T) {
	_, root := newRootEnv(t)
	ctx := context.Background()
	exec := os.FileMode(0755)

	// 根与挂载点根上的结构性写操作一律拒绝
	if err := root.Mkdir(ctx, "/new", 0755); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("根上 Mkdir = %v, want ErrNotSupported", err)
	}
	if err := root.Remove(ctx, "/"); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("删根 = %v, want ErrNotSupported", err)
	}
	if err := root.Remove(ctx, "/work"); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("删挂载点 = %v, want ErrNotSupported", err)
	}
	if err := root.Rename(ctx, "/work", "/w2"); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("改名挂载点 = %v, want ErrNotSupported", err)
	}
	if err := root.SetAttr(ctx, "/", Attrs{Mode: &exec}); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("改根属性 = %v, want ErrNotSupported", err)
	}
	if err := root.SetAttr(ctx, "/work", Attrs{Mode: &exec}); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("改挂载点属性 = %v, want ErrNotSupported", err)
	}
	if err := root.Symlink(ctx, "/work", "/link"); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("根下建链接 = %v, want ErrNotSupported", err)
	}
	if err := root.Symlink(ctx, "/work/a", "/work"); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("链接到挂载点根 = %v, want ErrNotSupported", err)
	}

	// Open：只读打开目录得到 ErrIsDir，带写意图则拒绝
	if _, err := root.Open(ctx, "/", OpenFlags{Read: true}, 0); !errors.Is(err, ErrIsDir) {
		t.Fatalf("Open(/) = %v, want ErrIsDir", err)
	}
	if _, err := root.Open(ctx, "/", OpenFlags{Read: true, Create: true}, 0); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("根上 Create = %v, want ErrNotSupported", err)
	}
	if _, err := root.Create(ctx, "/x.txt", 0644); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("根上 Create 便捷方法 = %v, want ErrNotSupported", err)
	}
	if _, err := root.Open(ctx, "/work", OpenFlags{Write: true}, 0); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("把挂载点当文件打开 = %v, want ErrNotSupported", err)
	}
	if _, err := root.Open(ctx, "/work", OpenFlags{Read: true}, 0); !errors.Is(err, ErrIsDir) {
		t.Fatalf("只读打开挂载点 = %v, want ErrIsDir", err)
	}
	if _, err := root.Readlink(ctx, "/"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Readlink(/) = %v, want ErrInvalid", err)
	}

	// 跨 Share 的 rename/copy/symlink → ErrCrossDevice
	if err := root.Rename(ctx, "/work/a", "/pub/a"); !errors.Is(err, ErrCrossDevice) {
		t.Fatalf("跨 Share Rename = %v, want ErrCrossDevice", err)
	}
	if err := root.Copy(ctx, "/work/a", "/pub/a", false); !errors.Is(err, ErrCrossDevice) {
		t.Fatalf("跨 Share Copy = %v, want ErrCrossDevice", err)
	}
	if err := root.Symlink(ctx, "/work/a", "/pub/link"); !errors.Is(err, ErrCrossDevice) {
		t.Fatalf("跨 Share Symlink = %v, want ErrCrossDevice", err)
	}
}

func TestRootUsePasswordOnEncryptedShare(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 2, 1, 8192)
	share := env.NewShare(p.Id, testutil.ShareOpts{
		Name: "priv", UserID: 1, Permission: db.ShareWrite, Encryption: EncryptionAESGCM,
	})
	// 用一次性会话设置口令，库里落下 salt || blake3(key)
	setter := NewShareFS(pm, share, 1, db.ShareWrite)
	if err := setter.SetPassword("pw"); err != nil {
		t.Fatal(err)
	}

	root := NewRootFS(pm, 1)
	ctx := context.Background()

	// 未提供口令：根目录照旧列出（元数据不需要密钥），文件读写报 ErrEncrypted
	if names := rootDirNames(t, root, "/"); len(names) != 1 || names[0] != "priv" {
		t.Fatalf("ReadDir(/) = %v", names)
	}
	if _, err := root.Create(ctx, "/priv/f.txt", 0644); !errors.Is(err, ErrEncrypted) {
		t.Fatalf("未提供口令写入 = %v, want ErrEncrypted", err)
	}

	if err := root.UsePassword("priv", "wrong"); !errors.Is(err, ErrPermission) {
		t.Fatalf("错误口令 = %v, want ErrPermission", err)
	}
	if err := root.UsePassword("priv", "pw"); err != nil {
		t.Fatalf("UsePassword: %v", err)
	}

	f, err := root.Create(ctx, "/priv/f.txt", 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("secret data")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// 改 Share 的配额 → 触发挂载实例重建，RootFS 缓存的密钥要沿用
	if err := env.DB.DB.Model(&db.Share{}).Where("id = ?", share.Id).
		Update("quota", 64).Error; err != nil {
		t.Fatal(err)
	}
	rf, err := root.Open(ctx, "/priv/f.txt", OpenFlags{Read: true}, 0)
	if err != nil {
		t.Fatalf("重建实例后读取: %v", err)
	}
	buf := make([]byte, len("secret data"))
	if _, err := rf.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	if err := rf.Close(); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "secret data" {
		t.Fatalf("读回 = %q", buf)
	}

	// 清除密钥后回到 ErrEncrypted
	if err := root.ClearKey("priv"); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Open(ctx, "/priv/f.txt", OpenFlags{Read: true}, 0); !errors.Is(err, ErrEncrypted) {
		t.Fatalf("清除密钥后读取 = %v, want ErrEncrypted", err)
	}
	if err := root.ClearKey("nosuch"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("给不存在的 Share 清密钥 = %v, want ErrNotExist", err)
	}
}

func TestRootMountReusesInstanceAndFollowsPermission(t *testing.T) {
	env, root := newRootEnv(t)

	first, err := root.Mount("work")
	if err != nil {
		t.Fatal(err)
	}
	second, err := root.Mount("work")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("同样的元数据下 Mount 应返回缓存实例")
	}

	// 权限降为只读 → 重建实例，写操作变成 ErrPermission
	if err := env.DB.DB.Model(&db.ShareUser{}).
		Where("user_id = ? AND share_id = ?", 1, first.Share().Id).
		Update("permission", db.ShareRead).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := root.Create(t.Context(), "/work/x.txt", 0644); !errors.Is(err, ErrPermission) {
		t.Fatalf("权限降级后写入 = %v, want ErrPermission", err)
	}
	after, err := root.Mount("work")
	if err != nil {
		t.Fatal(err)
	}
	if after == first {
		t.Fatal("权限变化后应重建挂载实例")
	}
}

func TestRootConcurrentAccess(t *testing.T) {
	_, root := newRootEnv(t)
	ctx := context.Background()

	f, err := root.Create(ctx, "/work/x.txt", 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("concurrent")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if _, err := root.ReadDir(ctx, "/"); err != nil {
					t.Error(err)
					return
				}
				if _, err := root.Stat(ctx, "/work/x.txt"); err != nil {
					t.Error(err)
					return
				}
				if _, err := root.StatFS(ctx, "/"); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
