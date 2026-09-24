package vfs

import (
	"testing"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/testutil"
)

// overwriteFile 从偏移 0 覆盖写一个已存在的文件（会提交一个新版本）。
func overwriteFile(t *testing.T, fs *ShareFS, path string, data []byte) {
	t.Helper()
	f, err := fs.Open(t.Context(), path, OpenFlags{Write: true}, 0)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatalf("Write(%s): %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close(%s): %v", path, err)
	}
}

// TestUsageBreakdown 三类占用的口径：当前版本、历史版本、回收站。
func TestUsageBreakdown(t *testing.T) {
	_, _, fs, _ := newWritable(t)
	ctx := t.Context()

	writeFile(t, fs, "/cur.txt", []byte("cur"))                  // 3 字节，当前版本
	writeFile(t, fs, "/hist.txt", []byte("old-old-old"))         // 11 字节，随后被覆盖成历史版本
	overwriteFile(t, fs, "/hist.txt", []byte("new-replacement")) // 15 字节，当前版本
	writeFile(t, fs, "/trash.txt", []byte("tra"))                // 3 字节
	if err := fs.Remove(ctx, "/trash.txt"); err != nil {
		t.Fatal(err)
	}

	got, err := fs.UsageBreakdown(ctx)
	if err != nil {
		t.Fatalf("UsageBreakdown: %v", err)
	}
	if got.CurrentBytes != 3+15 {
		t.Errorf("CurrentBytes = %d, want 18", got.CurrentBytes)
	}
	if got.HistoryBytes != 11 {
		t.Errorf("HistoryBytes = %d, want 11", got.HistoryBytes)
	}
	if got.RecycleBytes != 3 {
		t.Errorf("RecycleBytes = %d, want 3", got.RecycleBytes)
	}
}

// TestUsageBreakdownDedupesSharedChunks：写时复制让两个版本共享同一批分片，
// 统计必须按分片去重——按 SUM(versions.size) 累加会把同一份物理数据算两次。
func TestUsageBreakdownDedupesSharedChunks(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 2, 1, 1) // 1KB 切片，方便造多个切片
	fs, _ := newShareFS(t, env, pm, p.Id, testutil.ShareOpts{
		Name: "s", UserID: 1, Permission: db.ShareWrite,
	})
	ctx := t.Context()

	// 3 个满切片，每个 1024 字节
	big := make([]byte, 3*1024)
	for i := range big {
		big[i] = byte(i%251) + 1
	}
	writeFile(t, fs, "/m.txt", big)

	// 只改第 2 个切片里的一小段：新版继承另外两个切片，只重建被触及的那一个
	f, err := fs.Open(ctx, "/m.txt", OpenFlags{Write: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("xxxxxxxxxxxxxxxxxxxx"), 1024+10); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := fs.UsageBreakdown(ctx)
	if err != nil {
		t.Fatalf("UsageBreakdown: %v", err)
	}
	// 盘上共 4 个分片：被当前版本引用的 3 个（含 2 个与旧版本共享的）+ 只被旧版本引用的 1 个
	if got.CurrentBytes != 3*1024 {
		t.Errorf("CurrentBytes = %d, want %d", got.CurrentBytes, 3*1024)
	}
	if got.HistoryBytes != 1024 {
		t.Errorf("HistoryBytes = %d, want 1024（共享的分片不该被重复计入）", got.HistoryBytes)
	}
	if got.RecycleBytes != 0 {
		t.Errorf("RecycleBytes = %d, want 0", got.RecycleBytes)
	}
}
