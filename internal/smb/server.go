package smb

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"

	gsmb "github.com/jfjallid/go-smb/smb"
	"github.com/jfjallid/go-smb/smb/server"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
	"github.com/zzc53/zenofs/internal/pool"
	"github.com/zzc53/zenofs/internal/token"
)

// DefaultPort 是 SMB 的标准端口（445）。监听 445 需要 root；
// 调试时可以改到 1445 之类的端口，客户端用 `-p` / `port=1445` 指定。
const DefaultPort = 445

// DefaultNetBIOSName 是没有配置时的服务端名（NTLM 的 TargetInfo 里可见）。
const DefaultNetBIOSName = "ZENOFS"

// Options 是 SMB 服务的可选配置；零值表示"要求签名 + 支持加密"的安全默认。
type Options struct {
	// NetBIOSName 是客户端看到的服务端名，空则用 DefaultNetBIOSName。
	NetBIOSName string
	// DisableSigning 关闭"要求 SMB 签名"。只在排查客户端兼容性问题时用：
	// 关掉之后中间的任何人都能改 SMB 报文。
	DisableSigning bool
	// DisableEncryption 关闭 SMB 3.x 传输加密支持。
	DisableEncryption bool
	// Logger 覆盖 go-smb 的日志实现，nil 时用默认（Notice/Info/Error 写入标准 log）。
	Logger server.Logger
}

// Server 是一个运行中的 SMB2/3 服务端。
type Server struct {
	srv   *server.Server
	users *userCache

	mu        sync.Mutex
	listeners []net.Listener
	wg        sync.WaitGroup
}

// New 装配 SMB 服务：注册 shares 里的每个 Share 为一个同名共享，并挂上
// token 认证。shares 由 LoadShares 从库里读出——注册是静态快照，
// 之后新建的 Share 要重启服务才对外可见。
func New(pm *pool.PoolManager, tm *token.Manager, shares []db.Share, opts Options) *Server {
	users := newUserCache(pm)
	name := opts.NetBIOSName
	if name == "" {
		name = DefaultNetBIOSName
	}
	logger := opts.Logger
	if logger == nil {
		logger = logAdapter{}
	}
	cfg := &server.ServerConfig{
		NetBIOSName:         name,
		SigningRequired:     !opts.DisableSigning,
		EncryptionSupported: !opts.DisableEncryption,
		Authenticator:       &tokenAuthenticator{tokens: tm, users: users},
		Logger:              logger,
	}
	srv := &server.Server{Config: cfg}
	s := &Server{srv: srv, users: users}

	for _, share := range shares {
		if err := checkShareName(share.Name); err != nil {
			log.Printf("smb: 跳过 Share %d：%v", share.Id, err)
			continue
		}
		srv.RegisterShare(share.Name, server.Share{
			Type:   gsmb.ShareTypeDisk,
			Remark: "zenofs share " + share.Name,
			VFS:    &shareVFS{share: share, pm: pm, users: users},
		})
	}
	return s
}

// LoadShares 读出要暴露给 SMB 的所有 Share（按名字排序）。
func LoadShares(pm *pool.PoolManager) ([]db.Share, error) {
	var shares []db.Share
	if err := pm.DbManager.DB.Order("name").Find(&shares).Error; err != nil {
		return nil, errs.DBQuery(err)
	}
	return shares, nil
}

// Start 在 addr 上开始服务（后台运行），返回实际监听地址。
// addr 为空表示 ":445"；传 ":0" 可以拿随机端口，测试用。
func (s *Server) Start(addr string) (net.Addr, error) {
	if addr == "" {
		addr = fmt.Sprintf(":%d", DefaultPort)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("smb: listen %s: %w", addr, err)
	}
	s.mu.Lock()
	s.listeners = append(s.listeners, ln)
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, server.ErrServerClosed) {
			log.Printf("smb: serve %s: %v", ln.Addr(), err)
		}
	}()
	return ln.Addr(), nil
}

// Shutdown 停止监听并等在途连接结束（ctx 超时后强退）。
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.srv.Shutdown(ctx)
	s.wg.Wait()
	return err
}

// Shares 返回当前注册的共享名（测试与诊断用）。
func (s *Server) Shares() []string {
	if s.srv.Config == nil {
		return nil
	}
	out := make([]string, 0, len(s.srv.Config.Shares))
	for name := range s.srv.Config.Shares {
		out = append(out, name)
	}
	return out
}

// checkShareName 校验 Share 名能当 SMB 共享名用。
func checkShareName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("共享名为空")
	}
	if strings.ContainsAny(name, "\\/\x00") || len(name) > 80 {
		return fmt.Errorf("共享名 %q 含反斜杠/斜杠或过长", name)
	}
	return nil
}

// logAdapter 把 go-smb 的日志接进标准 log。
// Debug 级别的输出量太大（每个 SMB 请求一条），这里丢掉。
type logAdapter struct{}

func (logAdapter) Errorf(format string, v ...any)  { log.Printf("smb: ERROR "+format, v...) }
func (logAdapter) Errorln(v ...any)                { log.Println(append([]any{"smb: ERROR"}, v...)...) }
func (logAdapter) Noticef(format string, v ...any) { log.Printf("smb: "+format, v...) }
func (logAdapter) Noticeln(v ...any)               { log.Println(append([]any{"smb:"}, v...)...) }
func (logAdapter) Infof(format string, v ...any)   { log.Printf("smb: "+format, v...) }
func (logAdapter) Infoln(v ...any)                 { log.Println(append([]any{"smb:"}, v...)...) }
func (logAdapter) Debugf(format string, v ...any)  {}
func (logAdapter) Debugln(v ...any)                {}
