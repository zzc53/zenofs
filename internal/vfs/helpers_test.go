package vfs

import (
	"testing"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/pool"
	"github.com/zzc53/zenofs/internal/testutil"
)

// newEnv 建一个测试环境与绑定本地盘 handler 的 PoolManager。
func newEnv(t *testing.T) (*testutil.Env, *pool.PoolManager) {
	t.Helper()
	env := testutil.New(t)
	return env, pool.New(env.DB, []pool.ChunkHandler{pool.NewLocalChunkHandler()})
}

// newShareFS 在给定池上建一个 Share 并挂载成 ShareFS。
func newShareFS(t *testing.T, env *testutil.Env, pm *pool.PoolManager, poolId int64, o testutil.ShareOpts) (*ShareFS, db.Share) {
	t.Helper()
	share := env.NewShare(poolId, o)
	return NewShareFS(pm, share, o.UserID, o.Permission), share
}

// newWritable 建"一个池 + 一个可写 Share"，覆盖大多数用例的开场。
func newWritable(t *testing.T) (*testutil.Env, *pool.PoolManager, *ShareFS, db.Share) {
	t.Helper()
	env, pm := newEnv(t)
	p := env.NewPool("p", 2, 1, 8192)
	fs, share := newShareFS(t, env, pm, p.Id, testutil.ShareOpts{
		Name: "s", UserID: 1, Permission: db.ShareWrite,
	})
	return env, pm, fs, share
}

// writeFile 建文件并写入内容，返回提交后的 FileInfo。
func writeFile(t *testing.T, fs *ShareFS, path string, data []byte) {
	t.Helper()
	f, err := fs.Create(t.Context(), path, 0644)
	if err != nil {
		t.Fatalf("Create(%s): %v", path, err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatalf("Write(%s): %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close(%s): %v", path, err)
	}
}

// readFile 完整读回一个文件。
func readFile(t *testing.T, fs *ShareFS, path string, size int64) []byte {
	t.Helper()
	f, err := fs.Open(t.Context(), path, OpenFlags{Read: true}, 0)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	defer f.Close()
	buf := make([]byte, size)
	if _, err := f.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt(%s): %v", path, err)
	}
	return buf
}
