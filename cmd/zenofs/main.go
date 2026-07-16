// ZenoFS — 分布式纠删码存储系统
//
// 启动流程：
//  1. 加载配置（命令行参数 / ZENOFS_DSN 环境变量 / 默认 SQLite）
//  2. 初始化数据库连接并自动迁移表结构
//  3. 创建 PoolManager + LocalChunkHandler
//  4. 启动后台 parity worker（异步计算 RS 校验）和缓存清理 worker
//  5. 启动 HTTP API 服务器
//  6. 监听 SIGINT/SIGTERM，优雅关闭（先关 HTTP → 等 worker 完成 → 退出）
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zzc53/zenofs/internal/api"
	"github.com/zzc53/zenofs/internal/config"
	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/pool"
)

func main() {
	// 加载配置
	cfg := config.LoadConfig(os.Args)
	log.Printf("config loaded: %+v", cfg)

	// 初始化数据库（SQLite/MySQL/PostgreSQL）
	dbManager, err := db.New(cfg.DatabaseUrl)
	if err != nil {
		log.Fatalf("failed to init database: %+v", err)
	}
	// 自动建表
	if err := dbManager.AutoMigrate(); err != nil {
		log.Fatalf("failed to migrate database: %+v", err)
	}
	defer dbManager.Close()

	// 配置 DB 连接池
	sqlDB, err := dbManager.DB.DB()
	if err != nil {
		log.Fatalf("failed to get underlying sql.DB: %+v", err)
	}
	sqlDB.SetMaxOpenConns(25)
	sqlDB.SetMaxIdleConns(10)
	sqlDB.SetConnMaxLifetime(5 * time.Minute)

	// 创建 PoolManager（本地文件系统后端）
	pm := pool.New(dbManager, []pool.ChunkHandler{pool.NewLocalChunkHandler()})

	// 启动后台 worker（parity 计算 + 缓存清理）
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pm.StartParityWorker(ctx)
	pm.StartCacheCleaner(ctx)

	// 创建 HTTP 路由
	r := api.NewRouter(pm)

	// 读取端口配置，启动 HTTP 服务
	port := dbManager.GetSetting("HTTP_PORT", "8080")
	addr := ":" + port

	srv := &http.Server{
		Addr:         addr,
		Handler:      r,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// 优雅关闭：收到 SIGINT/SIGTERM 时逐步关闭
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		log.Printf("received signal %v, shutting down...", sig)

		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP server shutdown error: %v", err)
		}
		cancel() // 通知后台 worker（parity + 缓存清理）退出
	}()

	log.Printf("zenofs API listening on %s", addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}

	// 等待所有后台 worker 完成当前批次
	<-ctx.Done()
	log.Println("server stopped gracefully")
}
