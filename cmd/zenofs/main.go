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
	cfg := config.LoadConfig(os.Args)
	log.Printf("config loaded: %+v", cfg)

	dbManager, err := db.New(cfg.DatabaseUrl)
	if err != nil {
		log.Fatalf("failed to init database: %+v", err)
	}
	if err := dbManager.AutoMigrate(); err != nil {
		log.Fatalf("failed to migrate database: %+v", err)
	}
	defer dbManager.Close()

	// 配置连接池
	sqlDB, err := dbManager.DB.DB()
	if err != nil {
		log.Fatalf("failed to get underlying sql.DB: %+v", err)
	}
	sqlDB.SetMaxOpenConns(25)
	sqlDB.SetMaxIdleConns(10)
	sqlDB.SetConnMaxLifetime(5 * time.Minute)

	pm := pool.New(dbManager, []pool.ChunkHandler{pool.NewLocalChunkHandler()})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pm.StartParityWorker(ctx)

	r := api.NewRouter(pm)

	port := dbManager.GetSetting("HTTP_PORT", "8080")
	addr := ":" + port

	srv := &http.Server{
		Addr:         addr,
		Handler:      r,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// 优雅关闭：捕获 SIGINT/SIGTERM
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
		cancel() // 通知 parity worker 退出
	}()

	log.Printf("zenofs API listening on %s", addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}

	// 等待 parity worker 完成当前批次
	<-ctx.Done()
	log.Println("server stopped gracefully")
}
