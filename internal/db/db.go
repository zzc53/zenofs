// Package db 提供 GORM 数据库连接的管理。支持 SQLite、PostgreSQL 和 MySQL。
package db

import (
	"strings"

	"github.com/zzc53/zenofs/internal/errs"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Manager 管理 GORM 数据库实例。
type DbManager struct {
	DB *gorm.DB
}

// New 根据配置创建一个 GORM 数据库连接。
func New(url string) (*DbManager, error) {
	var dial gorm.Dialector

	if strings.HasPrefix(url, "sqlite://") {
		dial = sqlite.Open(strings.Replace(url, "sqlite://", "", 1) + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	} else if strings.HasPrefix(url, "mysql://") {
		dial = mysql.Open(url)
	} else if strings.HasPrefix(url, "postgres://") {
		dial = postgres.Open(url)
	} else {
		return nil, errs.New(errs.ECODE_DB_BAD_DSN, errs.ESTR_DB_BAD_DSN, "unsupported database url", url)
	}

	db, err := gorm.Open(dial, &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, errs.FromError(err, errs.ECODE_DB_BAD_CONN, errs.ESTR_DB_BAD_CONN)
	}

	return &DbManager{DB: db}, nil
}

// Close 关闭底层数据库连接。
func (m *DbManager) Close() error {
	sqlDB, err := m.DB.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// AutoMigrate 自动迁移给定的模型。
func (m *DbManager) AutoMigrate() error {
	if err := m.DB.AutoMigrate(&Pool{}, &Disk{}, &Chunk{}, &Stripe{}, &WriteQueue{}, &ReadCache{}, &Setting{}, &Task{}); err != nil {
		return err
	}
	// 首次运行时写入默认配置
	var count int64
	m.DB.Model(&Setting{}).Count(&count)
	if count == 0 {
		defaults := []Setting{
			{Name: "HTTP_PORT", Value: "8080"},
			{Name: "ACTION_LOCK", Value: "0"},
		}
		m.DB.Create(&defaults)
	}
	return nil
}

// GetSetting 返回指定名称的配置值，不存在时返回 fallback。
func (m *DbManager) GetSetting(name, fallback string) string {
	var s Setting
	if err := m.DB.Where("name = ?", name).First(&s).Error; err != nil {
		return fallback
	}
	return s.Value
}
