package vfs

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/testutil"
)

// newSliced 建一个切片 512KB 的可写 Share，方便构造多切片场景。
func newSliced(t *testing.T) (*testutil.Env, *ShareFS, db.Share) {
	t.Helper()
	env, pm := newEnv(t)
	// 池的 chunk size 就是这个共享的切片大小（见 vfs.sliceSize），这里用 512KB
	// 是为了让下面的断言能精确落在切片边界上。
	p := env.NewPool("p", 2, 1, 512)
	fs, share := newShareFS(t, env, pm, p.Id, testutil.ShareOpts{
		Name: "s", UserID: 1, Permission: db.ShareWrite,
	})
	return env, fs, share
}

// inodeOf 读出一个文件的 inode 行。
func inodeOf(t *testing.T, env *testutil.Env, share db.Share, name string) db.Inode {
	t.Helper()
	var in db.Inode
	if err := env.DB.DB.Where("share_id = ? AND name = ? AND deleted = 0", share.Id, name).
		First(&in).Error; err != nil {
		t.Fatalf("查 inode %s: %v", name, err)
	}
	return in
}

// chunkIdsOfVersion 返回某个版本各 idx 的 chunk id。
func chunkIdsOfVersion(t *testing.T, env *testutil.Env, versionId int64) map[int64]int64 {
	t.Helper()
	var rows []db.VersionChunk
	if err := env.DB.DB.Where("version_id = ?", versionId).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	out := make(map[int64]int64, len(rows))
	for _, r := range rows {
		out[r.Idx] = r.ChunkId
	}
	return out
}

func TestWriteReadAcrossSlices(t *testing.T) {
	_, fs, _ := newSliced(t)
	ctx := t.Context()

	// 1.5MB / 512KB 切片 = 3 个切片（末片半满）
	payload := testutil.RandBytes(21, 1536*1024)
	writeFile(t, fs, "/big.bin", payload)

	fi, err := fs.Stat(ctx, "/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size != int64(len(payload)) {
		t.Fatalf("size = %d, want %d", fi.Size, len(payload))
	}

	f, err := fs.Open(ctx, "/big.bin", OpenFlags{Read: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// 跨切片读：整片、跨边界、末片
	for _, tc := range []struct{ off, n int64 }{
		{0, 4096},
		{512*1024 - 10, 20}, // 跨第 1、2 片边界
		{512 * 1024, 100},   // 第 2 片开头
		{1024*1024 - 1, 2},  // 跨第 2、3 片边界
		{1300 * 1024, 4096}, // 末片中间
		{int64(len(payload)) - 1, 1},
	} {
		buf := make([]byte, tc.n)
		n, err := f.ReadAt(buf, tc.off)
		if err != nil {
			t.Fatalf("ReadAt(off=%d, n=%d): %v", tc.off, tc.n, err)
		}
		if int64(n) != tc.n {
			t.Fatalf("ReadAt(off=%d) 读到 %d 字节, want %d", tc.off, n, tc.n)
		}
		if !bytes.Equal(buf, payload[tc.off:tc.off+tc.n]) {
			t.Fatalf("ReadAt(off=%d, n=%d) 内容不一致", tc.off, tc.n)
		}
	}

	// Seek + Read
	if _, err := f.Seek(1024*1024+7, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	if _, err := io.ReadFull(f, buf); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, payload[1024*1024+7:1024*1024+7+64]) {
		t.Fatal("Seek + Read 内容不一致")
	}
	// 当前偏移前移了
	off, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(1024*1024 + 7 + 64); off != want {
		t.Fatalf("当前偏移 = %d, want %d", off, want)
	}
	// SeekEnd 到文件末尾再回退
	if off, err = f.Seek(-10, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	if off != int64(len(payload))-10 {
		t.Fatalf("SeekEnd(-10) = %d, want %d", off, int64(len(payload))-10)
	}
	if _, err := f.Seek(-1, io.SeekStart); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Seek 到负偏移 = %v, want ErrInvalid", err)
	}
}

func TestReadAtEdgeCases(t *testing.T) {
	_, fs, _ := newSliced(t)
	ctx := t.Context()

	payload := testutil.RandBytes(22, 1000)
	writeFile(t, fs, "/f.bin", payload)

	f, err := fs.Open(ctx, "/f.bin", OpenFlags{Read: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// 从文件中段读超过剩余长度：读到末尾为止，不补零
	buf := make([]byte, 4096)
	n, err := f.ReadAt(buf, 900)
	if err != nil {
		t.Fatalf("读到末尾不该报错: %v", err)
	}
	if n != 100 {
		t.Fatalf("读到 %d 字节, want 100", n)
	}
	if !bytes.Equal(buf[:n], payload[900:]) {
		t.Fatal("末尾数据不一致")
	}

	// 起点就在末尾之外：EOF
	if _, err := f.ReadAt(buf, 1000); !errors.Is(err, io.EOF) {
		t.Fatalf("越界读 = %v, want io.EOF", err)
	}
	if _, err := f.ReadAt(buf, -1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("负偏移 = %v, want ErrInvalid", err)
	}
	// 空读
	if n, err := f.ReadAt(nil, 0); n != 0 || err != nil {
		t.Fatalf("空读 = (%d, %v)", n, err)
	}
}

func TestPartialWriteKeepsOtherSliceChunks(t *testing.T) {
	env, fs, share := newSliced(t)
	ctx := t.Context()

	payload := testutil.RandBytes(23, 1536*1024)
	writeFile(t, fs, "/big.bin", payload)

	before := chunkIdsOfVersion(t, env, inodeOf(t, env, share, "big.bin").VersionId.Int64)
	if len(before) != 3 {
		t.Fatalf("切片数 = %d, want 3", len(before))
	}

	// 只改第 2 个切片里的一个字节
	f, err := fs.Open(ctx, "/big.bin", OpenFlags{Read: true, Write: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	patched := []byte("PATCHED!")
	if _, err := f.WriteAt(patched, 512*1024+100); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	copy(payload[512*1024+100:], patched)

	after := chunkIdsOfVersion(t, env, inodeOf(t, env, share, "big.bin").VersionId.Int64)
	if len(after) != 3 {
		t.Fatalf("新版本切片数 = %d, want 3", len(after))
	}
	for _, idx := range []int64{0, 2} {
		if before[idx] != after[idx] {
			t.Fatalf("idx %d 的 chunk 变了（%d → %d），写时复制应只重建被写到的切片",
				idx, before[idx], after[idx])
		}
	}
	if before[1] == after[1] {
		t.Fatal("被写到的切片没有生成新 chunk")
	}

	// 读回：只有目标位置变了，其它内容不变
	got := readFile(t, fs, "/big.bin", int64(len(payload)))
	if !bytes.Equal(got, payload) {
		t.Fatal("部分写之后文件内容不一致")
	}
}

func TestOpenSnapshotIsolation(t *testing.T) {
	_, fs, _ := newSliced(t)
	ctx := t.Context()

	writeFile(t, fs, "/f.txt", []byte("v1-content"))

	// 读句柄持有打开时的版本
	reader, err := fs.Open(ctx, "/f.txt", OpenFlags{Read: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	// 另一个句柄覆盖写
	writer, err := fs.Open(ctx, "/f.txt", OpenFlags{Read: true, Write: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteAt([]byte("v2"), 0); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, len("v1-content"))
	if _, err := reader.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, []byte("v1-content")) {
		t.Fatalf("旧句柄看到 %q，快照语义应保持 v1-content", buf)
	}

	// 新开的句柄看到新内容
	if got := readFile(t, fs, "/f.txt", int64(len("v2-content"))); !bytes.Equal(got, []byte("v2-content")) {
		t.Fatalf("新句柄看到 %q", got)
	}
}

func TestAppendMode(t *testing.T) {
	_, fs, _ := newSliced(t)
	ctx := t.Context()

	if err := fs.Mkdir(ctx, "/d", 0755); err != nil {
		t.Fatal(err)
	}
	f, err := fs.Open(ctx, "/d/a.txt", OpenFlags{Read: true, Write: true, Create: true, Append: true}, 0644)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"AA", "BB", "CC"} {
		if _, err := f.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, fs, "/d/a.txt", 6); !bytes.Equal(got, []byte("AABBCC")) {
		t.Fatalf("追加结果 = %q", got)
	}
}

func TestTruncateThroughHandle(t *testing.T) {
	_, fs, _ := newSliced(t)
	ctx := t.Context()

	payload := testutil.RandBytes(24, 2048)
	writeFile(t, fs, "/f.bin", payload)

	// 缩小：尾部数据丢弃，size 随之变小
	f, err := fs.Open(ctx, "/f.bin", OpenFlags{Read: true, Write: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1000); err != nil {
		t.Fatal(err)
	}
	if fi, err := f.Stat(); err != nil || fi.Size != 1000 {
		t.Fatalf("Truncate 后 Stat = (%+v, %v)", fi, err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, fs, "/f.bin", 1000); !bytes.Equal(got, payload[:1000]) {
		t.Fatal("截断后保留下来的前缀不对")
	}

	// 扩大：补出新区域，读作零（此前从未写过这段）
	writeFile(t, fs, "/g.bin", payload[:1000])
	g, err := fs.Open(ctx, "/g.bin", OpenFlags{Read: true, Write: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Truncate(1500); err != nil {
		t.Fatal(err)
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}

	got := readFile(t, fs, "/g.bin", 1500)
	if !bytes.Equal(got[:1000], payload[:1000]) {
		t.Fatal("扩大后原有的前缀不对")
	}
	for i := 1000; i < 1500; i++ {
		if got[i] != 0 {
			t.Fatalf("第 %d 字节 = %d, 扩大部分应补零", i, got[i])
		}
	}
}

func TestCloseDiscardsEmptyVersion(t *testing.T) {
	env, fs, share := newSliced(t)
	ctx := t.Context()

	writeFile(t, fs, "/f.txt", []byte("data"))
	in := inodeOf(t, env, share, "f.txt")
	firstVersion := in.VersionId.Int64

	// 打开后一个字节都不写：Close 时丢弃这个空版本
	f, err := fs.Open(ctx, "/f.txt", OpenFlags{Read: true, Write: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	var count int64
	if err := env.DB.DB.Model(&db.Version{}).Where("inode_id = ?", in.Id).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("版本数 = %d, want 1（空版本应被丢弃）", count)
	}
	if got := inodeOf(t, env, share, "f.txt").VersionId.Int64; got != firstVersion {
		t.Fatalf("inode 指向的版本 = %d, want %d（应保持原版本）", got, firstVersion)
	}
}

func TestCloseCommitsNewVersion(t *testing.T) {
	env, fs, share := newSliced(t)

	writeFile(t, fs, "/f.txt", []byte("first"))
	v1 := inodeOf(t, env, share, "f.txt").VersionId.Int64

	// 第二次写入 → 新版本，版本链指向前一个
	writeFile(t, fs, "/f.txt", []byte("second-version"))
	in := inodeOf(t, env, share, "f.txt")
	if in.VersionId.Int64 == v1 {
		t.Fatal("写入新内容后 inode 仍指向旧版本")
	}

	var versions []db.Version
	if err := env.DB.DB.Where("inode_id = ?", in.Id).Order("id").Find(&versions).Error; err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 {
		t.Fatalf("版本数 = %d, want 2", len(versions))
	}
	if versions[1].ParentId.Int64 != v1 || !versions[1].ParentId.Valid {
		t.Fatalf("新版本的父版本 = %+v, want %d", versions[1].ParentId, v1)
	}
	if versions[1].Size != int64(len("second-version")) {
		t.Fatalf("新版本 size = %d", versions[1].Size)
	}
	if versions[0].Size != int64(len("first")) {
		t.Fatalf("旧版本 size = %d, want %d", versions[0].Size, len("first"))
	}
	// 新版本继承的切片指向与内容都对
	if got := readFile(t, fs, "/f.txt", int64(len("second-version"))); !bytes.Equal(got, []byte("second-version")) {
		t.Fatalf("读回 = %q", got)
	}
}

func TestUnsavedWriteVisibleToOwnHandle(t *testing.T) {
	_, fs, _ := newSliced(t)
	ctx := t.Context()

	// 两个切片：先把第 2 个切片写满，让第 1 个切片成为"活动缓冲"
	payload := testutil.RandBytes(25, 1024*1024)
	writeFile(t, fs, "/big.bin", payload)

	f, err := fs.Open(ctx, "/big.bin", OpenFlags{Read: true, Write: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// 只写第 1 个切片，不 Close/Sync
	patched := []byte("UNSAVED")
	if _, err := f.WriteAt(patched, 512*1024+10); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(patched))
	if _, err := f.ReadAt(buf, 512*1024+10); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, patched) {
		t.Fatalf("同一句柄读到 %q, want %q", buf, patched)
	}

	// 别的句柄还看不到未提交的改动
	other, err := fs.Open(ctx, "/big.bin", OpenFlags{Read: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	obuf := make([]byte, len(patched))
	if _, err := other.ReadAt(obuf, 512*1024+10); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(obuf, patched) {
		t.Fatal("未提交的写入不该被其它句柄看到")
	}
}

func TestReadOnlyHandleRejectsWrite(t *testing.T) {
	_, fs, _ := newSliced(t)
	ctx := t.Context()

	writeFile(t, fs, "/f.txt", []byte("data"))
	f, err := fs.Open(ctx, "/f.txt", OpenFlags{Read: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if _, err := f.Write([]byte("x")); !errors.Is(err, ErrPermission) {
		t.Fatalf("只读句柄写入 = %v, want ErrPermission", err)
	}
	if _, err := f.WriteAt([]byte("x"), 0); !errors.Is(err, ErrPermission) {
		t.Fatalf("只读句柄 WriteAt = %v, want ErrPermission", err)
	}
	if err := f.Truncate(0); !errors.Is(err, ErrPermission) {
		t.Fatalf("只读句柄 Truncate = %v, want ErrPermission", err)
	}
	// 只读句柄的 Sync 是空操作
	if err := f.Sync(); err != nil {
		t.Fatalf("只读句柄 Sync = %v", err)
	}
}

// truncate 到 1000 字节再关掉重开。当前切片 512KB，所以截断点落在第 1 个切片内部。
func shrinkTo1000(t *testing.T, fs *ShareFS, path string) {
	t.Helper()
	f, err := fs.Open(t.Context(), path, OpenFlags{Read: true, Write: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1000); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTruncateShrinkDropsCutData(t *testing.T) {
	_, fs, _ := newSliced(t)
	ctx := t.Context()

	payload := testutil.RandBytes(31, 2048)
	writeFile(t, fs, "/f.bin", payload)
	shrinkTo1000(t, fs, "/f.bin")

	// 再扩大：截断过的那段必须读作零，不能把旧数据读回来
	g, err := fs.Open(ctx, "/f.bin", OpenFlags{Read: true, Write: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Truncate(1500); err != nil {
		t.Fatal(err)
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}

	got := readFile(t, fs, "/f.bin", 1500)
	if !bytes.Equal(got[:1000], payload[:1000]) {
		t.Fatal("保留的部分不对")
	}
	for i := 1000; i < 1500; i++ {
		if got[i] != 0 {
			t.Fatalf("第 %d 字节 = %d, 截断后扩大出的区域应读作零", i, got[i])
		}
	}

	// 缩小到 0：所有切片丢弃
	f, err := fs.Open(ctx, "/f.bin", OpenFlags{Read: true, Write: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(0); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	fi, err := fs.Stat(ctx, "/f.bin")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size != 0 {
		t.Fatalf("截断到 0 后 size = %d", fi.Size)
	}
	h, err := fs.Open(ctx, "/f.bin", OpenFlags{Read: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.ReadAt(make([]byte, 1), 0); !errors.Is(err, io.EOF) {
		t.Fatalf("截断到 0 后读取 = %v, want io.EOF", err)
	}
}

func TestTruncateAcrossSlicesDropsCutData(t *testing.T) {
	_, fs, _ := newSliced(t)
	ctx := t.Context()

	// 3 个切片，截断点落在第 3 个切片内部
	payload := testutil.RandBytes(32, 1536*1024)
	writeFile(t, fs, "/big.bin", payload)

	const cut = 1100 * 1024
	f, err := fs.Open(ctx, "/big.bin", OpenFlags{Read: true, Write: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(cut); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, fs, "/big.bin", cut); !bytes.Equal(got, payload[:cut]) {
		t.Fatal("截断后保留的部分不对")
	}

	// 再扩大：只有被截掉的那一段是零
	const grown = 1400 * 1024
	g, err := fs.Open(ctx, "/big.bin", OpenFlags{Read: true, Write: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Truncate(grown); err != nil {
		t.Fatal(err)
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, fs, "/big.bin", grown)
	if !bytes.Equal(got[:cut], payload[:cut]) {
		t.Fatal("扩大后保留的部分不对")
	}
	for i := int64(cut); i < grown; i++ {
		if got[i] != 0 {
			t.Fatalf("第 %d 字节 = %d（第 3 个切片内），截断后扩大出的区域应读作零", i, got[i])
		}
	}
}

func TestTruncateGrowKeepsActivityBuffer(t *testing.T) {
	_, fs, _ := newSliced(t)
	ctx := t.Context()

	// 只写 1000 字节：此时第 1 个切片还在"活动缓冲"里没落盘
	payload := testutil.RandBytes(33, 1000)
	f, err := fs.Open(ctx, "/f.bin", OpenFlags{Read: true, Write: true, Create: true}, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(payload); err != nil {
		t.Fatal(err)
	}
	// 扩大到切片边界之外：已写的内容要保留，其余读作零
	if err := f.Truncate(512 * 1024); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got := readFile(t, fs, "/f.bin", 512*1024)
	if !bytes.Equal(got[:len(payload)], payload) {
		t.Fatal("扩大后已写内容被清掉了")
	}
	for i := len(payload); i < len(got); i++ {
		if got[i] != 0 {
			t.Fatalf("第 %d 字节 = %d, 文件末尾之后应读作零", i, got[i])
		}
	}
}

func TestTruncateOnSliceBoundaryKeepsWholeSlice(t *testing.T) {
	_, fs, _ := newSliced(t)
	ctx := t.Context()
	const slice = int64(512 * 1024)

	// 先写第 2 个切片、再回写第 1 个：回写时会把第 2 个切片刷到盘上，
	// 于是活动缓冲停在 idx 0（这一片完整地落在后面的截断点之内）
	inFirst := testutil.RandBytes(34, 1000)
	inSecond := testutil.RandBytes(35, 1000)
	f, err := fs.Open(ctx, "/f.bin", OpenFlags{Read: true, Write: true, Create: true}, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(inSecond, slice+10); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(inFirst, 0); err != nil {
		t.Fatal(err)
	}
	// 截断点正好落在切片边界：idx 0 整片都要保留
	if err := f.Truncate(slice); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got := readFile(t, fs, "/f.bin", slice)
	if !bytes.Equal(got[:len(inFirst)], inFirst) {
		t.Fatal("对齐到切片边界的截断把完整保留的切片清掉了")
	}
	for i := len(inFirst); i < len(got); i++ {
		if got[i] != 0 {
			t.Fatalf("第 %d 字节 = %d, 截断后应读作零", i, got[i])
		}
	}
}
