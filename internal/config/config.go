// Package config 提供系统配置结构体与加载功能。
package config

import (
	"os"
)

// DBType 支持的数据库类型。
type DBType string

const (
	DBTypeSQLite   DBType = "sqlite"
	DBTypePostgres DBType = "postgres"
	DBTypeMySQL    DBType = "mysql"
)

// Config 是系统全局配置。
type Config struct {
	DatabaseUrl string
}

// Default 返回默认配置。
func Default() *Config {
	return &Config{
		DatabaseUrl: "sqlite://zenofs.db",
	}
}

// LoadFromEnv 从环境变量加载配置，覆盖默认值。
func LoadConfig(args []string) *Config {
	cfg := Default()
	if len(args) > 1 && args[1] != "" {
		cfg.DatabaseUrl = args[1]
		return cfg
	}
	if v := os.Getenv("ZENOFS_DSN"); v != "" {
		cfg.DatabaseUrl = v
		return cfg
	}
	return cfg
}
