package pool

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
	"github.com/zzc53/zenofs/internal/testutil"
)

// newEnv 建一个测试环境 + 绑定本地盘 handler 的 PoolManager。
func newEnv(t *testing.T) (*testutil.Env, *PoolManager) {
	t.Helper()
	env := testutil.New(t)
	return env, New(env.DB, []ChunkHandler{NewLocalChunkHandler()})
}

// assertCode 断言 err 是带指定错误码的 ZenoError。
func assertCode(t *testing.T, err error, code int, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: 期望报错，实际成功", what)
	}
	var ze *errs.ZenoError
	if !errors.As(err, &ze) {
		t.Fatalf("%s: 错误类型 = %T (%v), want *errs.ZenoError", what, err, err)
	}
	if ze.Code != code {
		t.Fatalf("%s: Code = %d (%s), want %d (%s)", what, ze.Code, ze.StrCode, code, codeStr(code))
	}
}

// assertZeno 断言 err 是 ZenoError（不关心具体码）。
func assertZeno(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: 期望报错，实际成功", what)
	}
	var ze *errs.ZenoError
	if !errors.As(err, &ze) {
		t.Fatalf("%s: 错误类型 = %T (%v), want *errs.ZenoError", what, err, err)
	}
}

// codeStr 把错误码翻成字符串码，失败信息更好读。
func codeStr(code int) string {
	switch code {
	case errs.ECODE_POOL_BAD:
		return errs.ESTR_POOL_BAD
	case errs.ECODE_POOL_BAD_NAME:
		return errs.ESTR_POOL_BAD_NAME
	case errs.ECODE_POOL_OFFLINE:
		return errs.ESTR_POOL_OFFLINE
	case errs.ECODE_DISK_BAD_BACKEND:
		return errs.ESTR_DISK_BAD_BACKEND
	case errs.ECODE_DISK_BAD_TYPE:
		return errs.ESTR_DISK_BAD_TYPE
	case errs.ECODE_DISK_OFFLINE:
		return errs.ESTR_DISK_OFFLINE
	case errs.ECODE_CHUNK_EMPTY:
		return errs.ESTR_CHUNK_EMPTY
	case errs.ECODE_CHUNK_SIZE_EXCEED:
		return errs.ESTR_CHUNK_SIZE_EXCEED
	case errs.ECODE_CHUNK_NOT_FOUND:
		return errs.ESTR_CHUNK_NOT_FOUND
	case errs.ECODE_FILE_WRITE:
		return errs.ESTR_FILE_WRITE
	default:
		return "?"
	}
}

func TestAddPoolValidation(t *testing.T) {
	env, pm := newEnv(t)

	// chunk size 必须在 1 ~ 65536 KB 之间
	for _, kb := range []int64{0, -1, 64*1024 + 1} {
		_, err := pm.AddPool("bad", kb)
		assertCode(t, err, errs.ECODE_POOL_BAD, "非法 chunk size")
	}

	// 边界值可用
	for _, tc := range []struct {
		name string
		kb   int64
	}{{"min", 1}, {"max", 64 * 1024}} {
		p, err := pm.AddPool(tc.name, tc.kb)
		if err != nil {
			t.Fatalf("AddPool(%s): %v", tc.name, err)
		}
		if p.Id == 0 || p.ChunkSize != tc.kb || p.Status != db.Online {
			t.Fatalf("AddPool(%s) 返回的行不对: %+v", tc.name, p)
		}
	}

	// 重名
	_, err := pm.AddPool("min", 16)
	assertCode(t, err, errs.ECODE_POOL_BAD_NAME, "重名 pool")

	var count int64
	if err := env.DB.DB.Model(&db.Pool{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("pool 行数 = %d, want 2", count)
	}
}

func TestGetPool(t *testing.T) {
	_, pm := newEnv(t)
	p, err := pm.AddPool("p", 16)
	if err != nil {
		t.Fatal(err)
	}

	got, err := pm.GetPool(p.Id)
	if err != nil {
		t.Fatalf("GetPool: %v", err)
	}
	if got.Name != "p" {
		t.Fatalf("Name = %q", got.Name)
	}

	_, err = pm.GetPool(p.Id + 1000)
	assertCode(t, err, errs.ECODE_POOL_BAD, "不存在的 pool")
}

func TestAddDisk(t *testing.T) {
	env, pm := newEnv(t)
	p, err := pm.AddPool("p", 16)
	if err != nil {
		t.Fatal(err)
	}

	// 数据盘：DataShards +1
	d1, err := pm.AddDisk(p.Id, env.Path("d1"), int8(db.LocalBackend), int8(db.DataDisk), false)
	if err != nil {
		t.Fatal(err)
	}
	if d1.Type != db.DataDisk || d1.Status != db.Online || d1.Backend != db.LocalBackend {
		t.Fatalf("数据盘字段不对: %+v", d1)
	}
	// parity 盘：ParityShards +1
	if _, err := pm.AddDisk(p.Id, env.Path("d2"), int8(db.LocalBackend), int8(db.DataDisk), true); err != nil {
		t.Fatal(err)
	}
	// 缓存盘：不影响分片数
	d3, err := pm.AddDisk(p.Id, env.Path("cache"), int8(db.LocalBackend), int8(db.CacheDisk), false)
	if err != nil {
		t.Fatal(err)
	}
	if d3.Type != db.CacheDisk {
		t.Fatalf("缓存盘 Type = %d", d3.Type)
	}

	got, err := pm.GetPool(p.Id)
	if err != nil {
		t.Fatal(err)
	}
	if got.DataShards != 1 || got.ParityShards != 1 {
		t.Fatalf("分片数 = %d+%d, want 1+1", got.DataShards, got.ParityShards)
	}

	// 非法参数
	_, err = pm.AddDisk(p.Id, env.Path("x"), int8(9), int8(db.DataDisk), false)
	assertCode(t, err, errs.ECODE_DISK_BAD_BACKEND, "非法 backend")
	_, err = pm.AddDisk(p.Id, env.Path("x"), int8(db.LocalBackend), int8(9), false)
	assertCode(t, err, errs.ECODE_DISK_BAD_TYPE, "非法 disk type")
	_, err = pm.AddDisk(p.Id+1000, env.Path("x"), int8(db.LocalBackend), int8(db.DataDisk), false)
	assertZeno(t, err, "不存在的 pool")
}

func TestOfflinePoolAndSwapDisk(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 1, 0, 16)
	d, err := pm.AddDisk(p.Id, env.Path("extra"), int8(db.LocalBackend), int8(db.DataDisk), false)
	if err != nil {
		t.Fatal(err)
	}

	if err := pm.OfflinePool(p.Id); err != nil {
		t.Fatal(err)
	}
	got, err := pm.GetPool(p.Id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != db.Offline {
		t.Fatalf("Status = %d, want Offline", got.Status)
	}

	// 换盘：盘转 Repair + 路径更新，所属池下线（重建完成前不对外服务）
	newPath := env.Path("replacement")
	if err := pm.SwapDisk(d.Id, newPath); err != nil {
		t.Fatal(err)
	}
	var disk db.Disk
	if err := env.DB.DB.First(&disk, d.Id).Error; err != nil {
		t.Fatal(err)
	}
	if disk.Status != db.Repair || disk.Path != newPath {
		t.Fatalf("换盘后 = %+v", disk)
	}
	got, err = pm.GetPool(p.Id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != db.Offline {
		t.Fatalf("换盘后 pool status = %d, want Offline", got.Status)
	}
}

func TestPoolIdOfChunk(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 1, 0, 4096)

	chunk, err := pm.AddChunk(p.Id, testutil.RandBytes(1, 128))
	if err != nil {
		t.Fatal(err)
	}
	got, err := pm.PoolIdOfChunk(chunk.Id)
	if err != nil {
		t.Fatal(err)
	}
	if got != p.Id {
		t.Fatalf("PoolIdOfChunk = %d, want %d", got, p.Id)
	}

	_, err = pm.PoolIdOfChunk(chunk.Id + 1000)
	assertCode(t, err, errs.ECODE_CHUNK_NOT_FOUND, "不存在的 chunk")
}

func TestUniqueIds(t *testing.T) {
	tests := []struct {
		in   []int64
		want []int64
	}{
		{nil, []int64{}},
		{[]int64{1, 1, 2, 3, 3, 1}, []int64{1, 2, 3}},
		{[]int64{5}, []int64{5}},
	}
	for _, tt := range tests {
		got := uniqueIds(tt.in)
		if len(got) != len(tt.want) {
			t.Fatalf("uniqueIds(%v) = %v, want %v", tt.in, got, tt.want)
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Fatalf("uniqueIds(%v) = %v, want %v", tt.in, got, tt.want)
			}
		}
	}
}

func TestHandlerFor(t *testing.T) {
	_, pm := newEnv(t)
	if h := pm.handlerFor(db.LocalBackend); h == nil || h.Type() != db.LocalBackend {
		t.Fatalf("本地 handler 没注册: %v", h)
	}
	if h := pm.handlerFor(db.S3Backend); h != nil {
		t.Fatalf("未注册的后端应返回 nil，得到 %v", h)
	}
}

// 空路径的盘（早期版本允许加出来，属于历史脏数据）不该被选中存数据：
// 它写到哪儿取决于进程的工作目录，会悄悄把 chunk 写错地方。
func TestEmptyPathDiskIsExcluded(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 1, 0, 4096) // 这个 helper 已经建好 1 块数据盘

	var disk db.Disk
	if err := env.DB.DB.Where("pool_id = ? AND type = ?", p.Id, db.DataDisk).First(&disk).Error; err != nil {
		t.Fatal(err)
	}

	// 清空路径，模拟历史脏数据（池的分片计数里还算着这块盘）。
	// 用裸 SQL：GORM 的 Update 会把这里当零值处理，可能不落库。
	if err := env.DB.DB.Exec("UPDATE disks SET path = '' WHERE id = ?", disk.Id).Error; err != nil {
		t.Fatal(err)
	}
	var after db.Disk
	if err := env.DB.DB.First(&after, disk.Id).Error; err != nil {
		t.Fatal(err)
	}
	if after.Path != "" {
		t.Fatalf("清空路径没生效，得到 %q", after.Path)
	}

	if _, err := pm.AddChunks(p.Id, [][]byte{testutil.RandBytes(31, 4096)}); err == nil {
		t.Fatalf("池里只有空路径盘时，写入必须失败而不是把 chunk 写到工作目录")
	}
}

// 并发写入不能撞上 SQLite 的写锁。
//
// getNewChunks 的事务是"先读（查预留槽位）后写（建 stripe）"：如果事务用默认的
// BEGIN DEFERRED 开始，两个并发事务会各自拿着共享锁再抢升级，SQLite 对这种死锁
// 是**立即**返回 SQLITE_BUSY 的，busy_timeout 也救不了（等下去也不会有人放手）。
// 解决办法是让事务用 BEGIN IMMEDIATE 开始（DSN 里的 _txlock=immediate）。
//
// 这是 database is locked 的回归测试：去掉 _txlock=immediate 后它必然失败。
func TestConcurrentAddChunksNoLockError(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 2, 1, 4096)

	const goroutines = 8
	const perGoroutine = 4
	var wg sync.WaitGroup
	failures := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				data := testutil.RandBytes(int64(g*1000+i), 1024)
				if _, err := pm.AddChunks(p.Id, [][]byte{data}); err != nil {
					failures <- err
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if strings.Contains(err.Error(), "database is locked") {
			t.Fatalf("并发写入撞上 SQLite 写锁: %v", err)
		}
		t.Fatalf("并发写入失败: %v", err)
	}
}
