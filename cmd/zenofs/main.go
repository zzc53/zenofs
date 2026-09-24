// ZenoFS — 分布式纠删码存储系统
//
// 启动流程：
//  1. 加载配置（命令行参数 / ZENOFS_DSN 环境变量 / 默认 SQLite）
//  2. 初始化数据库连接并自动迁移表结构
//  3. 创建 PoolManager + LocalChunkHandler
//  4. 启动后台 parity worker（异步计算 RS 校验）和缓存清理 worker
//  5. 创建访问凭证管理器（access token：SMB / SFTP 共用）
//  6. 按 settings 表里的端口启动 SMB / SFTP 服务（SMB 默认不启用，SFTP 默认 2222）
//  7. 启动 HTTP API 服务器（WebDAV 挂在它上面的 /dav）
//  8. 监听 SIGINT/SIGTERM，优雅关闭（先关协议服务 → HTTP → 等 worker 完成 → 退出）
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zzc53/zenofs/internal/api"
	"github.com/zzc53/zenofs/internal/auth"
	"github.com/zzc53/zenofs/internal/config"
	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/pool"
	"github.com/zzc53/zenofs/internal/sftp"
	"github.com/zzc53/zenofs/internal/smb"
	"github.com/zzc53/zenofs/internal/token"
	"github.com/zzc53/zenofs/internal/webdav"
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

	// 收拾上一次运行留下的 write queue：写完的搬进 stripe queue（该算的校验块不漏），
	// 半途中断的孤片清掉。放在 worker 启动之前，这样清出来的任务能被立刻消费。
	if removed, err := pm.CleanupWriteQueueOnStartup(); err != nil {
		log.Printf("startup: cleanup write queue failed: %v", err)
	} else if removed > 0 {
		log.Printf("startup: dropped %d stale write queue record(s)", removed)
	}

	// 启动后台 worker（parity 计算 + 缓存清理）
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pm.StartParityWorker(ctx)
	pm.StartCacheCleaner(ctx)

	// 访问凭证：SMB / SFTP / WebDAV 都用它认证（见 internal/token）
	tokens := token.NewManager(dbManager)

	// 用户与登录态：密码 + TOTP 二次验证，签发 JWT（见 internal/auth）
	authManager, err := auth.NewManager(dbManager)
	if err != nil {
		log.Fatalf("failed to init auth manager: %+v", err)
	}
	if has, err := authManager.HasUsers(); err == nil && !has {
		log.Printf("auth: 还没有任何用户，请先调 POST /api/auth/bootstrap 创建第一个管理员")
	}

	// 文件协议服务：SMB 与 SFTP 的端口写在 settings 表里（SFTP 默认 2222，SMB 默认不启用）
	smbServer := startSMB(pm, tokens, dbManager)
	sftpServer := startSFTP(pm, tokens, dbManager)

	// 创建 HTTP 路由：WebDAV 与前端都和 REST API 共用这个 HTTP 服务（同一端口）
	webdavPrefix := dbManager.GetSetting("WEBDAV_PREFIX", webdav.DefaultPrefix)
	switch webdavPrefix {
	case "off", "-":
		log.Printf("webdav: 已关闭（WEBDAV_PREFIX=%q）", webdavPrefix)
	default:
		log.Printf("webdav: 挂载在 %s/（Basic 认证：zenofs 用户名 + access token）", webdavPrefix)
	}
	webuiEnabled := dbManager.GetSetting("WEBUI_ENABLED", "1") != "0"
	if webuiEnabled {
		log.Printf("webui: 挂在 /（首次运行访问 http://<host>:<port>/ 走引导流程）")
	}
	r := api.NewRouter(api.Options{
		PoolManager:  pm,
		Tokens:       tokens,
		Auth:         authManager,
		WebDAVPrefix: webdavPrefix,
		DisableWebUI: !webuiEnabled,
	})

	// 读取端口配置，启动 HTTP 服务
	port := dbManager.GetSetting("HTTP_PORT", "8080")
	addr := ":" + port

	srv := newHTTPServer(addr, r)

	// 优雅关闭：收到 SIGINT/SIGTERM 时逐步关闭
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		log.Printf("received signal %v, shutting down...", sig)

		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()

		// 先停文件协议服务（等会话收尾），再停 HTTP
		if sftpServer != nil {
			if err := sftpServer.Shutdown(shutdownCtx); err != nil {
				log.Printf("SFTP server shutdown error: %v", err)
			}
		}
		if smbServer != nil {
			if err := smbServer.Shutdown(shutdownCtx); err != nil {
				log.Printf("SMB server shutdown error: %v", err)
			}
		}
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

// newHTTPServer 构造 HTTP 服务（REST API + WebDAV + 前端都挂在它上面）。
//
// 这里**故意不设** ReadTimeout / WriteTimeout：文件上传下载、WebDAV 传大文件可能持续
// 几分钟甚至更久，按固定时间掐断会把传输弄坏（客户端只会看到一个 400 或连接中断，
// 比如 10 秒整失败的那种）。抗慢速攻击靠 ReadHeaderTimeout（只限制请求头）与
// IdleTimeout（连接空闲）；生产环境若要限制单次传输的时长或带宽，建议在反向代理层做。
func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// startSMB 按 settings 里的 SMB_PORT 启动 SMB 服务。
// 端口为空或 "0" 表示不启用——445 是特权端口，默认开着会让普通用户启动失败。
// 返回 nil 表示没有启动。
func startSMB(pm *pool.PoolManager, tokens *token.Manager, dbManager *db.DbManager) *smb.Server {
	port := dbManager.GetSetting("SMB_PORT", "")
	if port == "" || port == "0" {
		log.Printf("smb: 未启用（在 settings 表设置 SMB_PORT 后重启；标准端口 %d 需要 root）", smb.DefaultPort)
		return nil
	}
	shares, err := smb.LoadShares(pm)
	if err != nil {
		log.Printf("smb: 读取 Share 失败，未启动：%v", err)
		return nil
	}
	srv := smb.New(pm, tokens, shares, smb.Options{
		NetBIOSName: dbManager.GetSetting("SMB_NETBIOS_NAME", smb.DefaultNetBIOSName),
	})
	addr, err := srv.Start(":" + port)
	if err != nil {
		log.Printf("smb: 启动失败：%v", err)
		return nil
	}
	log.Printf("smb: 监听 %s（%d 个共享，客户端用 zenofs 用户名 + access token 登录）", addr, len(shares))
	return srv
}

// startSFTP 按 settings 里的 SFTP_PORT 启动 SFTP 服务。
// 默认端口 2222（见 sftp.DefaultPort）；设为 off / - / 0 可以关掉。
func startSFTP(pm *pool.PoolManager, tokens *token.Manager, dbManager *db.DbManager) *sftp.Server {
	setting := dbManager.GetSetting("SFTP_PORT", "")
	addr := sftpAddr(setting)
	if addr == "" {
		log.Printf("sftp: 已关闭（settings.SFTP_PORT=%q；去掉它即用默认端口 %d）", setting, sftp.DefaultPort)
		return nil
	}
	srv, err := sftp.New(pm, tokens, sftp.Options{
		HostKeyFile: dbManager.GetSetting("SFTP_HOST_KEY_FILE", ""),
	})
	if err != nil {
		log.Printf("sftp: 初始化失败，未启动：%v", err)
		return nil
	}
	listened, err := srv.Start(addr)
	if err != nil {
		log.Printf("sftp: 启动失败：%v", err)
		return nil
	}
	log.Printf("sftp: 监听 %s（公钥或 username + access token 登录）", listened)
	return srv
}

// sftpAddr 把 settings 里的 SFTP_PORT 解析成监听地址。
//
// 默认是 2222（sftp.DefaultPort）——不是标准的 22：22 要 root，而 2222 让
// 普通用户也能直接起服务；设为 off / - / 0 表示关闭（返回空串）。
func sftpAddr(setting string) string {
	port := strings.TrimSpace(setting)
	switch port {
	case "off", "-", "0":
		return ""
	case "":
		return ":" + strconv.Itoa(sftp.DefaultPort)
	}
	return ":" + port
}
