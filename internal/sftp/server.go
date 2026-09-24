// Package sftp 用 golang.org/x/crypto/ssh + github.com/pkg/sftp 提供 SFTP 服务。
//
// # 认证
//
// 认证接 access_tokens 表（internal/token），两种方式都支持：
//   - SSH 公钥：匹配 Kind=TokenPublicKey 的凭证（库里明文存公钥 + 指纹）；
//   - 密码：客户端填"zenofs 用户名 + token"，服务端比 token 的单向摘要。
//
// 两种凭证都可以设过期时间，过期即拒绝登录。
//
// # 视图
//
// 登录后的根目录是该用户在 zenofs 里可见的全部 Share（vfs.RootFS）：
// 路径形如 /<share>/...，根目录就是 Share 列表；权限来自 share_users，
// 只读 Share 上的写操作会被 vfs 层拒绝。
//
// # 主机密钥
//
// 默认在 settings 表里持久化一对自动生成的 ed25519 主机密钥（首次启动生成、
// 之后复用），也可以用外部 PEM 文件覆盖（Options.HostKeyFile）。
package sftp

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"sync"

	psftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"gorm.io/gorm"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/pool"
	"github.com/zzc53/zenofs/internal/token"
	"github.com/zzc53/zenofs/internal/vfs"
)

const (
	// DefaultPort 是 SFTP 的默认监听端口。
	//
	// 用 2222 而不是标准的 22：22 是特权端口（要 root），2222 让普通用户
	// 直接 `go run ./cmd/zenofs` 就能起一个可用的 SFTP 服务。
	DefaultPort = 2222
	// hostKeySetting 是 settings 表里保存自动生成主机密钥的键名。
	hostKeySetting = "SFTP_HOST_KEY"
	// extUserID 是认证成功后放进 ssh.Permissions.Extensions 的用户 id。
	extUserID = "zenofs_user_id"
)

// Options 是 SFTP 服务的可选项；零值表示公钥与 token 密码都接受。
type Options struct {
	// HostKeyFile 是外部 SSH 主机私钥（PEM）。为空时用 settings 表里持久化的自动生成密钥。
	HostKeyFile string
	// DisablePassword 关闭密码认证（只留公钥）。
	DisablePassword bool
	// DisablePublicKey 关闭公钥认证（只留 token 密码）。
	DisablePublicKey bool
}

// Server 是一个运行中的 SFTP 服务端。
type Server struct {
	pm     *pool.PoolManager
	tokens *token.Manager
	cfg    *ssh.ServerConfig

	mu     sync.Mutex
	ln     net.Listener
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup
}

// New 装配 SFTP 服务：加载主机密钥、装好两种认证回调。
func New(pm *pool.PoolManager, tm *token.Manager, opts Options) (*Server, error) {
	if opts.DisablePassword && opts.DisablePublicKey {
		return nil, errors.New("sftp: 公钥与密码认证都被关闭，无法登录")
	}
	signer, err := loadHostKey(pm.DbManager, opts.HostKeyFile)
	if err != nil {
		return nil, err
	}

	cfg := &ssh.ServerConfig{}
	cfg.AddHostKey(signer)

	s := &Server{pm: pm, tokens: tm, cfg: cfg, conns: make(map[net.Conn]struct{})}
	if !opts.DisablePublicKey {
		cfg.PublicKeyCallback = func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			tok, err := tm.AuthenticatePublicKey(c.User(), key)
			if err != nil {
				return nil, fmt.Errorf("sftp: 公钥认证失败: %v", err)
			}
			return permissionsOf(tok.UserId), nil
		}
	}
	if !opts.DisablePassword {
		cfg.PasswordCallback = func(c ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			tok, err := tm.AuthenticateSecret(c.User(), string(password))
			if err != nil {
				return nil, fmt.Errorf("sftp: token 认证失败: %v", err)
			}
			return permissionsOf(tok.UserId), nil
		}
	}
	return s, nil
}

// Start 开始监听（后台运行），返回实际监听地址。
// addr 为空表示 ":2222"（见 DefaultPort）；传 "127.0.0.1:0" 可以拿随机端口（测试用）。
func (s *Server) Start(addr string) (net.Addr, error) {
	if addr == "" {
		addr = fmt.Sprintf(":%d", DefaultPort)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("sftp: listen %s: %w", addr, err)
	}

	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.accept(ln)
	}()
	return ln.Addr(), nil
}

// Shutdown 停止监听、断开在途连接，并等所有连接 goroutine 退出。
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	ln := s.ln
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	if ln != nil {
		ln.Close()
	}
	for _, c := range conns {
		c.Close()
	}

	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// accept 是监听循环。
func (s *Server) accept(ln net.Listener) {
	for {
		nConn, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return
			}
			// 临时错误（如 EMFILE）：等一会儿再试，别把监听器拆了。
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			log.Printf("sftp: accept: %v", err)
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			nConn.Close()
			return
		}
		s.conns[nConn] = struct{}{}
		s.mu.Unlock()

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.conns, nConn)
				s.mu.Unlock()
			}()
			s.handleConn(nConn)
		}()
	}
}

// handleConn 完成 SSH 握手，并把每个 session channel 交给 SFTP 处理器。
func (s *Server) handleConn(nConn net.Conn) {
	defer nConn.Close()

	conn, chans, reqs, err := ssh.NewServerConn(nConn, s.cfg)
	if err != nil {
		log.Printf("sftp: SSH 握手失败（%s）: %v", nConn.RemoteAddr(), err)
		return
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)

	userID, err := userIDOf(conn)
	if err != nil {
		log.Printf("sftp: 连接缺少用户信息: %v", err)
		return
	}
	// 每个连接一份 RootFS：它缓存了该用户的 Share 视图与授权。
	root := vfs.NewRootFS(s.pm, userID)

	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			newChannel.Reject(ssh.UnknownChannelType, "只支持 session channel")
			continue
		}
		channel, requests, err := newChannel.Accept()
		if err != nil {
			log.Printf("sftp: 接受 channel 失败: %v", err)
			continue
		}
		go s.serveSession(channel, requests, root)
	}
}

// serveSession 在一个 session channel 上等 "subsystem: sftp" 请求，
// 收到后把该 channel 交给 pkg/sftp 的请求服务器。
func (s *Server) serveSession(channel ssh.Channel, requests <-chan *ssh.Request, root *vfs.RootFS) {
	defer channel.Close()
	started := false
	for req := range requests {
		if req.Type != "subsystem" || len(req.Payload) < 4 || string(req.Payload[4:]) != "sftp" {
			if req.WantReply {
				req.Reply(false, nil)
			}
			continue
		}
		if started {
			req.Reply(false, nil)
			continue
		}
		started = true
		req.Reply(true, nil)

		rs := psftp.NewRequestServer(channel, psftp.Handlers{
			FileGet:  &handlers{root: root},
			FilePut:  &handlers{root: root},
			FileCmd:  &handlers{root: root},
			FileList: &handlers{root: root},
		})
		if err := rs.Serve(); err != nil && !errors.Is(err, io.EOF) {
			log.Printf("sftp: 会话结束: %v", err)
		}
		rs.Close()
		return
	}
}

// ─────────────────────────────────────────────────────────────
// 认证辅助
// ─────────────────────────────────────────────────────────────

// permissionsOf 把用户 id 放进 SSH 权限扩展，供连接建立后取出。
func permissionsOf(userID int64) *ssh.Permissions {
	return &ssh.Permissions{Extensions: map[string]string{extUserID: strconv.FormatInt(userID, 10)}}
}

// userIDOf 从已认证连接里取用户 id。
func userIDOf(conn *ssh.ServerConn) (int64, error) {
	if conn.Permissions == nil {
		return 0, errors.New("sftp: 没有权限信息")
	}
	raw, ok := conn.Permissions.Extensions[extUserID]
	if !ok {
		return 0, errors.New("sftp: 没有用户 id")
	}
	return strconv.ParseInt(raw, 10, 64)
}

// ─────────────────────────────────────────────────────────────
// 主机密钥
// ─────────────────────────────────────────────────────────────

// loadHostKey 取 SSH 主机私钥：优先外部文件，其次 settings 表里持久化的
// 自动生成密钥；两者都没有就生成一对并写回 settings。
func loadHostKey(dbm *db.DbManager, file string) (ssh.Signer, error) {
	if file != "" {
		pemBytes, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("sftp: 读主机密钥 %s: %w", file, err)
		}
		signer, err := ssh.ParsePrivateKey(pemBytes)
		if err != nil {
			return nil, fmt.Errorf("sftp: 解析主机密钥 %s: %w", file, err)
		}
		return signer, nil
	}

	if stored := dbm.GetSetting(hostKeySetting, ""); stored != "" {
		if signer, err := ssh.ParsePrivateKey([]byte(stored)); err == nil {
			return signer, nil
		}
		// 存量密钥坏了就只能换新的：客户端会看到 host key 变化。
		log.Printf("sftp: settings 表里的主机密钥无法解析，重新生成")
	}

	signer, pemText, err := generateHostKey()
	if err != nil {
		return nil, err
	}
	var setting db.Setting
	err = dbm.DB.Where("name = ?", hostKeySetting).First(&setting).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		if err := dbm.DB.Create(&db.Setting{Name: hostKeySetting, Value: pemText}).Error; err != nil {
			return nil, fmt.Errorf("sftp: 保存主机密钥: %w", err)
		}
	case err != nil:
		return nil, fmt.Errorf("sftp: 读主机密钥配置: %w", err)
	default:
		if err := dbm.DB.Model(&db.Setting{}).Where("id = ?", setting.Id).
			Update("value", pemText).Error; err != nil {
			return nil, fmt.Errorf("sftp: 更新主机密钥: %w", err)
		}
	}
	return signer, nil
}

// generateHostKey 生成一对 ed25519 主机密钥，返回签名器与 PKCS#8 PEM 文本。
func generateHostKey() (ssh.Signer, string, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("sftp: 生成主机密钥: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, "", fmt.Errorf("sftp: 序列化主机密钥: %w", err)
	}
	pemText := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, "", fmt.Errorf("sftp: 构造签名器: %w", err)
	}
	return signer, string(pemText), nil
}
