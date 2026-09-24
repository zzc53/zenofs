package db

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzc53/zenofs/internal/errs"
)

// openTemp 在临时目录里开一个 SQLite 库。
func openTemp(t *testing.T) *DbManager {
	t.Helper()
	m, err := New("sqlite://" + filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

func TestNewSQLiteAndAutoMigrate(t *testing.T) {
	m := openTemp(t)
	if err := m.AutoMigrate(); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}

	// 全部模型都必须登记到 AutoMigrate，否则运行时报 no such table
	wantTables := []string{
		"pools", "disks", "chunks", "stripes", "write_queues", "stripe_queues", "read_caches",
		"settings", "tasks", "users", "shares", "share_users",
		"inodes", "versions", "version_chunks", "inode_histories",
	}
	var names []string
	if err := m.DB.Raw("SELECT name FROM sqlite_master WHERE type = 'table'").Scan(&names).Error; err != nil {
		t.Fatalf("列 sqlite_master: %v", err)
	}
	have := make(map[string]bool, len(names))
	for _, n := range names {
		have[n] = true
	}
	for _, table := range wantTables {
		if !have[table] {
			t.Errorf("建表后缺少表 %q", table)
		}
	}

	// 首次运行写入默认配置
	if got := m.GetSetting("HTTP_PORT", ""); got != "8080" {
		t.Errorf("HTTP_PORT = %q, want 8080", got)
	}
	if got := m.GetSetting("ACTION_LOCK", ""); got != "0" {
		t.Errorf("ACTION_LOCK = %q, want 0", got)
	}

	// 幂等：再迁移一次不报错，默认配置也不会重复插入
	if err := m.AutoMigrate(); err != nil {
		t.Fatalf("第二次 AutoMigrate: %v", err)
	}
	var count int64
	if err := m.DB.Model(&Setting{}).Where("name = ?", "HTTP_PORT").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("HTTP_PORT 记录数 = %d, want 1", count)
	}

	// 同名 inode 的唯一索引（软删除后可以重名）必须建出来
	var idxNames []string
	if err := m.DB.Raw("SELECT name FROM sqlite_master WHERE type = 'index'").Scan(&idxNames).Error; err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range idxNames {
		if n == "idx_inode_active_parent_name" {
			found = true
		}
	}
	if !found {
		t.Error("缺少 idx_inode_active_parent_name 索引")
	}
}

func TestNewBadDSN(t *testing.T) {
	_, err := New("redis://localhost:6379")
	if err == nil {
		t.Fatal("不支持的 DSN 应该报错")
	}
	var ze *errs.ZenoError
	if !errors.As(err, &ze) {
		t.Fatalf("错误类型 = %T, want *errs.ZenoError", err)
	}
	if ze.Code != errs.ECODE_DB_BAD_DSN {
		t.Fatalf("Code = %d, want %d", ze.Code, errs.ECODE_DB_BAD_DSN)
	}
	if !strings.Contains(ze.Error(), "redis://") {
		t.Fatalf("错误信息里应该带上 DSN: %q", ze.Error())
	}
}

// TestNewNetworkBackends 只验证 mysql/postgres 两种 DSN 会被识别成对应后端：
// 本机没有服务器时，gorm 可能直接报连接错误，也可能延迟到首次查询才失败，
// 两种都算正常；但不该被当成"不支持的 DSN"。
func TestNewNetworkBackends(t *testing.T) {
	urls := []string{
		"mysql://user:pass@tcp(127.0.0.1:1)/zenofs",
		"postgres://user:pass@127.0.0.1:1/zenofs",
	}
	for _, url := range urls {
		m, err := New(url)
		if err != nil {
			var ze *errs.ZenoError
			if errors.As(err, &ze) && ze.Code == errs.ECODE_DB_BAD_DSN {
				t.Errorf("%s: 被当成不支持的 DSN: %v", url, err)
			}
			continue
		}
		m.Close()
	}
}

func TestGetSetting(t *testing.T) {
	m := openTemp(t)
	if err := m.AutoMigrate(); err != nil {
		t.Fatal(err)
	}

	if got := m.GetSetting("NOT_EXIST", "fallback"); got != "fallback" {
		t.Fatalf("不存在的配置 = %q, want fallback", got)
	}

	if err := m.DB.Create(&Setting{Name: "CUSTOM", Value: "value-1"}).Error; err != nil {
		t.Fatal(err)
	}
	if got := m.GetSetting("CUSTOM", "fallback"); got != "value-1" {
		t.Fatalf("已有配置 = %q, want value-1", got)
	}
	if err := m.DB.Model(&Setting{}).Where("name = ?", "CUSTOM").
		Update("value", "value-2").Error; err != nil {
		t.Fatal(err)
	}
	if got := m.GetSetting("CUSTOM", "fallback"); got != "value-2" {
		t.Fatalf("更新后 = %q, want value-2", got)
	}
}

func TestClose(t *testing.T) {
	m := openTemp(t)
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
