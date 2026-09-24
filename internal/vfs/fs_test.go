package vfs

import (
	"bytes"
	"errors"
	"os"
	"sort"
	"testing"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/testutil"
)

// dirNames 取目录下的条目名并按字典序排序，方便断言。
func dirNames(t *testing.T, fs *ShareFS, path string) []string {
	t.Helper()
	entries, err := fs.ReadDir(t.Context(), path)
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

func assertNames(t *testing.T, got, want []string, what string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: 条目 = %v, want %v", what, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: 条目 = %v, want %v", what, got, want)
		}
	}
}

func TestStatRoot(t *testing.T) {
	_, _, fs, share := newWritable(t)

	fi, err := fs.Stat(t.Context(), "/")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Kind != KindDir || fi.Id != 0 || fi.Name != "/" || fi.Path != "/" {
		t.Fatalf("根条目 = %+v", fi)
	}
	// gid 取所属 Share 的 id；合成的根没有 inode 行，所以 uid 为 0
	if fi.Gid != uint32(share.Id) {
		t.Fatalf("根条目 gid = %d, want %d", fi.Gid, share.Id)
	}
	if fi.Uid != 0 {
		t.Fatalf("合成根的 uid = %d, want 0（根没有 inode 记录）", fi.Uid)
	}
	if !fi.Mode.IsDir() || fi.Mode.Perm()&0o222 == 0 {
		t.Fatalf("可写挂载的目录权限 = %v", fi.Mode)
	}
}

func TestMkdirReadDirRemove(t *testing.T) {
	_, _, fs, _ := newWritable(t)
	ctx := t.Context()

	if err := fs.Mkdir(ctx, "/a", 0755); err != nil {
		t.Fatal(err)
	}
	if err := fs.Mkdir(ctx, "/a/b", 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, fs, "/a/f.txt", []byte("hello"))

	assertNames(t, dirNames(t, fs, "/"), []string{"a"}, "读根目录")
	assertNames(t, dirNames(t, fs, "/a"), []string{"b", "f.txt"}, "读 /a")

	// 已存在、根、父目录不存在
	if err := fs.Mkdir(ctx, "/a", 0755); !errors.Is(err, ErrExist) {
		t.Fatalf("重复 Mkdir = %v, want ErrExist", err)
	}
	if err := fs.Mkdir(ctx, "/", 0755); !errors.Is(err, ErrExist) {
		t.Fatalf("Mkdir 根 = %v, want ErrExist", err)
	}
	if err := fs.Mkdir(ctx, "/nope/x", 0755); !errors.Is(err, ErrNotExist) {
		t.Fatalf("父目录不存在 = %v, want ErrNotExist", err)
	}

	// 非空目录不能删
	if err := fs.Remove(ctx, "/a"); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("删非空目录 = %v, want ErrNotEmpty", err)
	}
	if err := fs.Remove(ctx, "/"); !errors.Is(err, ErrIsDir) {
		t.Fatalf("删根 = %v, want ErrIsDir", err)
	}
	if err := fs.Remove(ctx, "/nope"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("删不存在 = %v, want ErrNotExist", err)
	}

	// 删文件后目录里不再有它；再删空目录
	if err := fs.Remove(ctx, "/a/f.txt"); err != nil {
		t.Fatal(err)
	}
	assertNames(t, dirNames(t, fs, "/a"), []string{"b"}, "删文件后")
	if err := fs.Remove(ctx, "/a/b"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Remove(ctx, "/a"); err != nil {
		t.Fatal(err)
	}
	assertNames(t, dirNames(t, fs, "/"), nil, "删空目录后")
}

func TestReadDirOnFileFails(t *testing.T) {
	_, _, fs, _ := newWritable(t)
	writeFile(t, fs, "/f.txt", []byte("x"))
	if _, err := fs.ReadDir(t.Context(), "/f.txt"); !errors.Is(err, ErrNotDir) {
		t.Fatalf("对文件 ReadDir = %v, want ErrNotDir", err)
	}
}

func TestRenameSemantics(t *testing.T) {
	_, _, fs, _ := newWritable(t)
	ctx := t.Context()

	writeFile(t, fs, "/a.txt", []byte("aaa"))
	if err := fs.Rename(ctx, "/a.txt", "/b.txt"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, fs, "/b.txt", 3); !bytes.Equal(got, []byte("aaa")) {
		t.Fatalf("改名后内容 = %q", got)
	}

	// 目标已存在的文件按 POSIX 覆盖
	writeFile(t, fs, "/c.txt", []byte("ccc"))
	if err := fs.Rename(ctx, "/c.txt", "/b.txt"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, fs, "/b.txt", 3); !bytes.Equal(got, []byte("ccc")) {
		t.Fatalf("覆盖后内容 = %q", got)
	}
	assertNames(t, dirNames(t, fs, "/"), []string{"b.txt"}, "覆盖后")

	// 自反改名是空操作
	if err := fs.Rename(ctx, "/b.txt", "/b.txt"); err != nil {
		t.Fatal(err)
	}

	// 目录不能移进自己的子树
	if err := fs.Mkdir(ctx, "/dir", 0755); err != nil {
		t.Fatal(err)
	}
	if err := fs.Mkdir(ctx, "/dir/sub", 0755); err != nil {
		t.Fatal(err)
	}
	if err := fs.Rename(ctx, "/dir", "/dir/sub/dir"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("移进子树 = %v, want ErrInvalid", err)
	}

	// 根不能被改名，源不存在要报 ErrNotExist
	if err := fs.Rename(ctx, "/", "/x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("改名根 = %v, want ErrInvalid", err)
	}
	if err := fs.Rename(ctx, "/nope", "/other"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("源不存在 = %v, want ErrNotExist", err)
	}
}

func TestSymlinkAndReadlink(t *testing.T) {
	_, _, fs, _ := newWritable(t)
	ctx := t.Context()

	if err := fs.Mkdir(ctx, "/dir", 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, fs, "/dir/target.txt", []byte("data"))
	if err := fs.Symlink(ctx, "/dir/target.txt", "/link"); err != nil {
		t.Fatal(err)
	}

	// 链接在 Share 内的绝对路径就是 Readlink 的返回值
	got, err := fs.Readlink(ctx, "/link")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/dir/target.txt" {
		t.Fatalf("Readlink = %q, want /dir/target.txt", got)
	}

	// Lstat 不跟随，Stat 跟随
	lfi, err := fs.Lstat(ctx, "/link")
	if err != nil {
		t.Fatal(err)
	}
	if lfi.Kind != KindSymlink || lfi.Target != "/dir/target.txt" {
		t.Fatalf("Lstat = %+v", lfi)
	}
	sfi, err := fs.Stat(ctx, "/link")
	if err != nil {
		t.Fatal(err)
	}
	if sfi.Kind != KindFile || sfi.Size != 4 {
		t.Fatalf("Stat 跟随 = %+v", sfi)
	}
	// 通过链接读内容
	if got := readFile(t, fs, "/link", 4); !bytes.Equal(got, []byte("data")) {
		t.Fatalf("经链接读到 %q", got)
	}

	// 错误分支
	if err := fs.Symlink(ctx, "/dir/nope", "/link2"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("链接到不存在的目标 = %v, want ErrNotExist", err)
	}
	if err := fs.Symlink(ctx, "/", "/link3"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("链接到根 = %v, want ErrInvalid", err)
	}
	if err := fs.Symlink(ctx, "/dir/target.txt", "/link"); !errors.Is(err, ErrExist) {
		t.Fatalf("链接名已存在 = %v, want ErrExist", err)
	}
	if _, err := fs.Readlink(ctx, "/dir/target.txt"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("对普通文件 Readlink = %v, want ErrInvalid", err)
	}
	// ReadDir 里链接条目带 Target
	entries, err := fs.ReadDir(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Name == "link" {
			found = true
			if e.Kind != KindSymlink || e.Target != "/dir/target.txt" {
				t.Fatalf("ReadDir 里的链接条目 = %+v", e)
			}
		}
	}
	if !found {
		t.Fatal("ReadDir 没有列出符号链接")
	}
}

func TestCopy(t *testing.T) {
	_, _, fs, _ := newWritable(t)
	ctx := t.Context()

	payload := testutil.RandBytes(5, 4096)
	writeFile(t, fs, "/src.bin", payload)
	if err := fs.Copy(ctx, "/src.bin", "/copy.bin", false); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, fs, "/copy.bin", int64(len(payload))); !bytes.Equal(got, payload) {
		t.Fatal("复制出来的文件内容不一致")
	}
	// 复制是独立的两份：改一份不影响另一份
	f, err := fs.Open(ctx, "/copy.bin", OpenFlags{Read: true, Write: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("XXXX"), 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, fs, "/src.bin", int64(len(payload))); !bytes.Equal(got, payload) {
		t.Fatal("改副本影响了源文件")
	}

	// 目标已存在 → ErrExist
	if err := fs.Copy(ctx, "/src.bin", "/copy.bin", false); !errors.Is(err, ErrExist) {
		t.Fatalf("目标已存在 = %v, want ErrExist", err)
	}
	// 源是挂载根 → ErrInvalid
	if err := fs.Copy(ctx, "/", "/rootcopy", false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("复制根 = %v, want ErrInvalid", err)
	}

	// 目录：非递归拒绝，递归复制整棵树
	if err := fs.Mkdir(ctx, "/tree", 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, fs, "/tree/a.txt", []byte("a"))
	if err := fs.Mkdir(ctx, "/tree/sub", 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, fs, "/tree/sub/b.txt", []byte("bb"))
	if err := fs.Copy(ctx, "/tree", "/tree2", false); !errors.Is(err, ErrIsDir) {
		t.Fatalf("非递归复制目录 = %v, want ErrIsDir", err)
	}
	if err := fs.Copy(ctx, "/tree", "/tree2", true); err != nil {
		t.Fatal(err)
	}
	assertNames(t, dirNames(t, fs, "/tree2"), []string{"a.txt", "sub"}, "复制的目录")
	if got := readFile(t, fs, "/tree2/sub/b.txt", 2); !bytes.Equal(got, []byte("bb")) {
		t.Fatalf("递归复制的内容 = %q", got)
	}
}

func TestSetAttr(t *testing.T) {
	_, _, fs, _ := newWritable(t)
	ctx := t.Context()

	writeFile(t, fs, "/f.txt", []byte("0123456789"))

	// Mode 只体现"可执行"这一位
	exec := os.FileMode(0755)
	if err := fs.SetAttr(ctx, "/f.txt", Attrs{Mode: &exec}); err != nil {
		t.Fatal(err)
	}
	fi, err := fs.Stat(ctx, "/f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode.Perm()&0o111 == 0 {
		t.Fatalf("设置可执行后 Mode = %v", fi.Mode)
	}
	plain := os.FileMode(0644)
	if err := fs.SetAttr(ctx, "/f.txt", Attrs{Mode: &plain}); err != nil {
		t.Fatal(err)
	}
	if fi, err = fs.Stat(ctx, "/f.txt"); err != nil {
		t.Fatal(err)
	}
	if fi.Mode.Perm()&0o111 != 0 {
		t.Fatalf("取消可执行后 Mode = %v", fi.Mode)
	}

	// Uid/Gid 被忽略（权限由 share_users 控制），文件属主不变
	other := uint32(999)
	if err := fs.SetAttr(ctx, "/f.txt", Attrs{Uid: &other, Gid: &other}); err != nil {
		t.Fatal(err)
	}
	if fi, err = fs.Stat(ctx, "/f.txt"); err != nil {
		t.Fatal(err)
	}
	if fi.Uid == other || fi.Gid == other {
		t.Fatalf("Uid/Gid 不该被 SetAttr 改动: %+v", fi)
	}

	// Size 走 Truncate
	short := int64(4)
	if err := fs.SetAttr(ctx, "/f.txt", Attrs{Size: &short}); err != nil {
		t.Fatal(err)
	}
	if fi, err = fs.Stat(ctx, "/f.txt"); err != nil {
		t.Fatal(err)
	}
	if fi.Size != 4 {
		t.Fatalf("截断后 size = %d, want 4", fi.Size)
	}
	if got := readFile(t, fs, "/f.txt", 4); !bytes.Equal(got, []byte("0123")) {
		t.Fatalf("截断后内容 = %q", got)
	}

	// 空 Attrs 是空操作
	if err := fs.SetAttr(ctx, "/f.txt", Attrs{}); err != nil {
		t.Fatal(err)
	}
}

func TestStatFSQuota(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 1, 0, 4096)
	fs, _ := newShareFS(t, env, pm, p.Id, testutil.ShareOpts{
		Name: "q", UserID: 1, Permission: db.ShareWrite, QuotaMB: 1,
	})

	info, err := fs.StatFS(t.Context(), "/")
	if err != nil {
		t.Fatal(err)
	}
	if info.TotalBytes != 1<<20 || info.UsedBytes != 0 {
		t.Fatalf("空 Share 的容量 = %+v", info)
	}

	writeFile(t, fs, "/a.bin", make([]byte, 1000))
	info, err = fs.StatFS(t.Context(), "/")
	if err != nil {
		t.Fatal(err)
	}
	if info.UsedBytes != 1000 {
		t.Fatalf("UsedBytes = %d, want 1000", info.UsedBytes)
	}
	if info.FreeBytes != 1<<20-1000 {
		t.Fatalf("FreeBytes = %d, want %d", info.FreeBytes, 1<<20-1000)
	}
}

func TestStatFSWithoutQuota(t *testing.T) {
	_, _, fs, _ := newWritable(t)
	writeFile(t, fs, "/a.bin", make([]byte, 512))

	info, err := fs.StatFS(t.Context(), "/")
	if err != nil {
		t.Fatal(err)
	}
	// 没配额时只报已用量，总量交给底层存储池
	if info.TotalBytes != 0 || info.UsedBytes != 512 {
		t.Fatalf("无配额容量 = %+v", info)
	}
}

func TestQuotaExceeded(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 2, 1, 8192)
	fs, _ := newShareFS(t, env, pm, p.Id, testutil.ShareOpts{
		Name: "q", UserID: 1, Permission: db.ShareWrite, QuotaMB: 1,
	})

	f, err := fs.Create(t.Context(), "/big.bin", 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(make([]byte, 2<<20)); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("写入超配额 = %v, want ErrNoSpace", err)
	}
}

func TestReadOnlyShareRejectsWrites(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 1, 0, 4096)

	// 先用可写会话准备一点数据
	writer, _ := newShareFS(t, env, pm, p.Id, testutil.ShareOpts{
		Name: "ro", UserID: 1, Permission: db.ShareWrite,
	})
	writeFile(t, writer, "/f.txt", []byte("read only"))
	if err := writer.Mkdir(t.Context(), "/d", 0755); err != nil {
		t.Fatal(err)
	}
	ro := NewShareFS(pm, writer.Share(), 1, db.ShareRead)
	ctx := t.Context()

	// 读操作正常
	if _, err := ro.Stat(ctx, "/f.txt"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, ro, "/f.txt", 9); !bytes.Equal(got, []byte("read only")) {
		t.Fatalf("读内容 = %q", got)
	}
	if _, err := ro.ReadDir(ctx, "/"); err != nil {
		t.Fatal(err)
	}

	// 写操作一律 ErrPermission
	if _, err := ro.Create(ctx, "/new.txt", 0644); !errors.Is(err, ErrPermission) {
		t.Fatalf("Create = %v, want ErrPermission", err)
	}
	if _, err := ro.Open(ctx, "/f.txt", OpenFlags{Read: true, Write: true}, 0); !errors.Is(err, ErrPermission) {
		t.Fatalf("写打开 = %v, want ErrPermission", err)
	}
	if err := ro.Mkdir(ctx, "/d2", 0755); !errors.Is(err, ErrPermission) {
		t.Fatalf("Mkdir = %v, want ErrPermission", err)
	}
	if err := ro.Remove(ctx, "/f.txt"); !errors.Is(err, ErrPermission) {
		t.Fatalf("Remove = %v, want ErrPermission", err)
	}
	if err := ro.Rename(ctx, "/f.txt", "/g.txt"); !errors.Is(err, ErrPermission) {
		t.Fatalf("Rename = %v, want ErrPermission", err)
	}
	if err := ro.Symlink(ctx, "/f.txt", "/l"); !errors.Is(err, ErrPermission) {
		t.Fatalf("Symlink = %v, want ErrPermission", err)
	}
	if err := ro.Copy(ctx, "/f.txt", "/c.txt", false); !errors.Is(err, ErrPermission) {
		t.Fatalf("Copy = %v, want ErrPermission", err)
	}
	mode := os.FileMode(0755)
	if err := ro.SetAttr(ctx, "/f.txt", Attrs{Mode: &mode}); !errors.Is(err, ErrPermission) {
		t.Fatalf("SetAttr = %v, want ErrPermission", err)
	}
	zero := int64(0)
	if err := ro.SetAttr(ctx, "/f.txt", Attrs{Size: &zero}); !errors.Is(err, ErrPermission) {
		t.Fatalf("SetAttr Size = %v, want ErrPermission", err)
	}
}

func TestOpenFlagsBehaviour(t *testing.T) {
	_, _, fs, _ := newWritable(t)
	ctx := t.Context()

	// 不存在且没带 Create
	if _, err := fs.Open(ctx, "/nope.txt", OpenFlags{Read: true}, 0); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Open 不存在 = %v, want ErrNotExist", err)
	}

	// Create + Exclusive
	f, err := fs.Open(ctx, "/x.txt", OpenFlags{Read: true, Write: true, Create: true, Exclusive: true}, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Open(ctx, "/x.txt", OpenFlags{Write: true, Create: true, Exclusive: true}, 0644); !errors.Is(err, ErrExist) {
		t.Fatalf("Create|Exclusive 已存在 = %v, want ErrExist", err)
	}

	// 打开目录
	if _, err := fs.Open(ctx, "/", OpenFlags{Read: true}, 0); !errors.Is(err, ErrIsDir) {
		t.Fatalf("Open 根 = %v, want ErrIsDir", err)
	}
	if err := fs.Mkdir(ctx, "/d", 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Open(ctx, "/d", OpenFlags{Read: true}, 0); !errors.Is(err, ErrIsDir) {
		t.Fatalf("Open 目录 = %v, want ErrIsDir", err)
	}

	// Truncate 打开把内容清空
	writeFile(t, fs, "/t.txt", []byte("content"))
	f, err = fs.Open(ctx, "/t.txt", OpenFlags{Read: true, Write: true, Truncate: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	fi, err := fs.Stat(ctx, "/t.txt")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size != 0 {
		t.Fatalf("Truncate 打开后 size = %d, want 0", fi.Size)
	}

	// 非法路径
	if _, err := fs.Open(ctx, "relative", OpenFlags{Read: true}, 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("相对路径 = %v, want ErrInvalid", err)
	}
}

func TestSparseFileReadsZeros(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 2, 1, 512) // chunk size = 切片大小
	fs, share := newShareFS(t, env, pm, p.Id, testutil.ShareOpts{
		Name: "sparse", UserID: 1, Permission: db.ShareWrite,
	})
	ctx := t.Context()

	writeFile(t, fs, "/s.bin", []byte("head"))
	// 扩大到 1MB：中间是空洞，不占存储
	big := int64(1 << 20)
	if err := fs.SetAttr(ctx, "/s.bin", Attrs{Size: &big}); err != nil {
		t.Fatal(err)
	}
	fi, err := fs.Stat(ctx, "/s.bin")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size != 1<<20 {
		t.Fatalf("size = %d, want %d", fi.Size, 1<<20)
	}

	got := readFile(t, fs, "/s.bin", 1<<20)
	if !bytes.Equal(got[:4], []byte("head")) {
		t.Fatalf("头部 = %q", got[:4])
	}
	for i := 4; i < len(got); i++ {
		if got[i] != 0 {
			t.Fatalf("第 %d 字节 = %d, 空洞应读作 0", i, got[i])
		}
	}

	// 空洞不产生切片记录：当前版本里只有存了"head"的那一个切片
	var inode db.Inode
	if err := env.DB.DB.Where("share_id = ? AND name = ?", share.Id, "s.bin").
		First(&inode).Error; err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := env.DB.DB.Model(&db.VersionChunk{}).
		Where("version_id = ?", inode.VersionId.Int64).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("version_chunk 数 = %d, want 1（只有头部那个切片）", count)
	}
}
