// Package db 提供 GORM 数据库连接的管理。支持 SQLite、PostgreSQL 和 MySQL。
package db

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/zzc53/zenofs/internal/errs"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	// defaultTxTimeout 是单个事务允许的最长耗时。
	defaultTxTimeout = 15 * time.Second

	// slowTxThreshold 超过它的（还没超时的）事务也会记一条日志。
	slowTxThreshold = 2 * time.Second

	// slowQueryThreshold 超过它的单条 SQL 由 GORM 自己记日志。
	slowQueryThreshold = time.Second
)

// DbManager 管理 GORM 数据库实例。
type DbManager struct {
	DB *gorm.DB

	// TxTimeout 是单个事务允许的最长耗时，超时后事务连同底层 context 一起被取消。
	// 零值表示用 defaultTxTimeout。
	//
	// 为什么需要它：数据库被别的事务锁住时，事务会一直等下去。等下去既不会成功，
	// 还会把后面的请求一起拖住——连接被占着不放，新请求排队，表现出来就是"卡死"。
	// 有超时至少能保证请求 fail fast、服务不倒，并在日志里留下证据。
	TxTimeout time.Duration
}

// New 根据 URL 前缀自动选择 SQLite/MySQL/PostgreSQL 驱动建立连接。
// 支持三种 URL 格式：
//
//	sqlite:///path/to/db   — SQLite（自动启用 WAL 模式）
//	mysql://user:pass@tcp(host:port)/dbname
//	postgres://user:pass@host:port/dbname
func New(url string) (*DbManager, error) {
	var dial gorm.Dialector
	isSQLite := false

	if strings.HasPrefix(url, "sqlite://") {
		isSQLite = true
		// 几个 pragma 参数：
		//   journal_mode(WAL)  —— 读不挡写、写不挡读
		//   busy_timeout(5000) —— 写锁被占时等一会儿，而不是立刻报错
		//   _txlock=immediate  —— 事务用 BEGIN IMMEDIATE 开始
		//
		// 最后这条本来很关键：默认的 BEGIN DEFERRED 是"读的时候拿共享锁、要写了才
		// 升级成排他锁"，两个并发写事务就会各自持着共享锁再抢升级，SQLite 把这种
		// 情况判为死锁、**立即**返回 SQLITE_BUSY，busy_timeout 也救不了（等下去也
		// 不会有人放手）。不过现在连接池被限制成单连接（见下面 SetMaxOpenConns），
		// 事务天然串行，这条其实已经冗余；留着是为了万一以后调大连接数、或者有
		// 外部 sqlite3 客户端同时打开这个库。
		path := strings.Replace(url, "sqlite://", "", 1)
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&" // 调用方自己带了参数
		}
		dial = sqlite.Open(path + sep +
			"_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate")
	} else if strings.HasPrefix(url, "mysql://") {
		dial = mysql.Open(url)
	} else if strings.HasPrefix(url, "postgres://") {
		dial = postgres.Open(url)
	} else {
		return nil, errs.New(errs.ECODE_DB_BAD_DSN, errs.ESTR_DB_BAD_DSN, "unsupported database url", url)
	}

	gdb, err := gorm.Open(dial, &gorm.Config{
		Logger: logger.New(log.New(os.Stderr, "", log.LstdFlags), logger.Config{
			// 慢 SQL 留一条痕。默认 200ms 对本地 SQLite 太吵，1 秒才算"不对劲"。
			SlowThreshold:             slowQueryThreshold,
			LogLevel:                  logger.Warn,
			IgnoreRecordNotFoundError: true,
			Colorful:                  false,
		}),
	})
	if err != nil {
		return nil, errs.FromError(err, errs.ECODE_DB_BAD_CONN, errs.ESTR_DB_BAD_CONN)
	}

	m := &DbManager{DB: gdb, TxTimeout: defaultTxTimeout}

	if isSQLite {
		sqlDB, err := gdb.DB()
		if err != nil {
			return nil, errs.FromError(err, errs.ECODE_DB_BAD_CONN, errs.ESTR_DB_BAD_CONN)
		}
		// SQLite 是单写者数据库：整个库同一时刻只能有一个写事务。
		// 与其放多个连接去抢那把锁（抢不到的就耗 busy_timeout，然后
		// database is locked；请求一多就看起来像卡死），不如只留一个连接，
		// 让事务在应用层老老实实排队——排队至少是能被超时打断的。
		// WAL 模式下读本来就不挡写，单连接牺牲的是并发读，换来的是不会锁死。
		sqlDB.SetMaxOpenConns(1)
		sqlDB.SetMaxIdleConns(1)
		sqlDB.SetConnMaxLifetime(0) // 本地文件，不需要定期换连接
	}

	return m, nil
}

// Tx 在一个带超时的上下文里执行事务。
//
// 所有事务都该走这里而不是直接 DB.Transaction：超时控制和诊断日志集中在这一个点上。
// 超时会取消底层 context，database/sql 据此中断等待/执行，所以卡住的事务最坏也只是
// 失败，不会把所有请求一起拖死。
func (m *DbManager) Tx(fn func(tx *gorm.DB) error) error {
	timeout := m.TxTimeout
	if timeout <= 0 {
		timeout = defaultTxTimeout
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	err := m.DB.WithContext(ctx).Transaction(fn)

	// 事务跑慢了、或者直接被超时掐断，都要在日志里留下痕迹。
	// 这类问题平时是"完全看不见"的——服务端一声不响，只是前端一直转圈。
	if elapsed := time.Since(start); elapsed >= slowTxThreshold || errors.Is(err, context.DeadlineExceeded) {
		log.Printf("db: transaction took %s (timeout %s, err=%v)%s",
			elapsed.Round(time.Millisecond), timeout, err, m.poolState())
	}
	return err
}

// poolState 把连接池状态拼进慢事务日志。
// wait 持续增长通常就意味着有事务占着连接不放——"卡死"的典型形态。
func (m *DbManager) poolState() string {
	sqlDB, err := m.DB.DB()
	if err != nil {
		return ""
	}
	st := sqlDB.Stats()
	return fmt.Sprintf(" [pool open=%d in_use=%d idle=%d wait=%d]",
		st.OpenConnections, st.InUse, st.Idle, st.WaitCount)
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
	if err := m.DB.AutoMigrate(&Pool{}, &Disk{}, &Chunk{}, &Stripe{}, &WriteQueue{}, &StripeQueue{}, &ReadCache{},
		&Setting{}, &Task{}, &User{}, &Share{}, &ShareUser{}, &Inode{}, &Version{}, &VersionChunk{}, &InodeHistory{},
		&AccessToken{}); err != nil {
		return err
	}
	// inodes: 未删除的记录同一目录下不允许重名
	m.DB.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_inode_active_parent_name ON inodes(parent_id, name) WHERE deleted = 0")
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
