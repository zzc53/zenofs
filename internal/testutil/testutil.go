// Package testutil 提供测试用的临时 SQLite 库与存储池 / Share 搭建工具。
//
// 只被 _test.go 引用，生产代码不依赖它。之所以独立成包（而不是每个包各写一份），
// 是因为 pool、vfs、api 的测试都要"临时库 + 本地盘 + Share 行"这套脚手架。
// 它只依赖 internal/db，不依赖 pool —— 否则 pool 包的内部测试会与它形成导入环。
package testutil

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzc53/zenofs/internal/db"
)

// Env 是一个测试用的临时环境：独立的临时目录 + 已迁移的 SQLite 库。
type Env struct {
	T   testing.TB
	Dir string
	DB  *db.DbManager
}

// New 创建临时环境；测试结束时自动关闭数据库（目录由 t.TempDir 清理）。
func New(t testing.TB) *Env {
	t.Helper()
	dir := t.TempDir()
	dbm, err := db.New("sqlite://" + filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("testutil: 打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { dbm.Close() })
	if err := dbm.AutoMigrate(); err != nil {
		t.Fatalf("testutil: AutoMigrate 失败: %v", err)
	}
	return &Env{T: t, Dir: dir, DB: dbm}
}

// Path 返回临时目录下的路径（不创建文件）。
func (e *Env) Path(parts ...string) string {
	e.T.Helper()
	return filepath.Join(append([]string{e.Dir}, parts...)...)
}

// NewPool 创建一个存储池，并为它建好 dataShards+parityShards 块本地数据盘
// （目录已创建）。条带预分配要求"在线数据盘数 == data + parity"，
// 所以这里一次建齐；需要造"盘数不足"等异常时用 NewDisk 自己加。
func (e *Env) NewPool(name string, dataShards, parityShards int, chunkSizeKB int64) db.Pool {
	e.T.Helper()
	p := db.Pool{
		Name:         name,
		DataShards:   int64(dataShards),
		ParityShards: int64(parityShards),
		ChunkSize:    chunkSizeKB,
		Status:       db.Online,
	}
	if err := e.DB.DB.Create(&p).Error; err != nil {
		e.T.Fatalf("testutil: 建 Pool 失败: %v", err)
	}
	for i := 0; i < dataShards+parityShards; i++ {
		e.NewDisk(p.Id, fmt.Sprintf("%s-disk%d", name, i), db.DataDisk)
	}
	return p
}

// NewDisk 给池建一块本地盘并创建对应目录。
func (e *Env) NewDisk(poolId int64, name string, typ db.DiskType) db.Disk {
	e.T.Helper()
	d := db.Disk{
		Path:    e.Path(name),
		PoolId:  poolId,
		Backend: db.LocalBackend,
		Type:    typ,
		Status:  db.Online,
	}
	if err := os.MkdirAll(d.Path, 0o755); err != nil {
		e.T.Fatalf("testutil: 建盘目录失败: %v", err)
	}
	if err := e.DB.DB.Create(&d).Error; err != nil {
		e.T.Fatalf("testutil: 建 Disk 失败: %v", err)
	}
	return d
}

// ShareOpts 描述一个测试用 Share；零值表示"不压缩、不加密、1MB 切片、不限额"。
type ShareOpts struct {
	Name        string
	UserID      int64 // 授权给哪个用户；0 表示不写 share_users（用来造"别人的 Share"）
	Permission  db.SharePermission
	QuotaMB     int64
	Compression int8
	Encryption  int8
	// 保留策略：0 表示不限（与 Share 模型的默认值一致）
	RecycleTtlHours int64
	VersionKeep     int64
}

// NewShare 建一条 Share 行；UserID 非 0 时同时写入 share_users 授权。
func (e *Env) NewShare(poolId int64, o ShareOpts) db.Share {
	e.T.Helper()
	s := db.Share{
		Name:            o.Name,
		PoolId:          poolId,
		Quota:           o.QuotaMB,
		Compression:     o.Compression,
		Encryption:      o.Encryption,
		RecycleTtlHours: o.RecycleTtlHours,
		VersionKeep:     o.VersionKeep,
		CreatedBy:       1,
	}
	if err := e.DB.DB.Create(&s).Error; err != nil {
		e.T.Fatalf("testutil: 建 Share 失败: %v", err)
	}
	if o.UserID != 0 {
		if err := e.DB.DB.Create(&db.ShareUser{
			ShareId: s.Id, UserId: o.UserID, Permission: o.Permission,
		}).Error; err != nil {
			e.T.Fatalf("testutil: 建 ShareUser 失败: %v", err)
		}
	}
	return s
}

// ReloadShare 重新读一遍 Share 行（SetPassword 之类的操作会改库里的字段）。
func (e *Env) ReloadShare(s db.Share) db.Share {
	e.T.Helper()
	if err := e.DB.DB.First(&s, s.Id).Error; err != nil {
		e.T.Fatalf("testutil: 重读 Share 失败: %v", err)
	}
	return s
}

// RandBytes 生成确定性随机字节：同一个 seed 每次结果一致，便于复现。
func RandBytes(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}
