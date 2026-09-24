package pool

import (
	"errors"
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
