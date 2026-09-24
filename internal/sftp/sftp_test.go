package sftp

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	psftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/pool"
	"github.com/zzc53/zenofs/internal/testutil"
	"github.com/zzc53/zenofs/internal/token"
)

// sftpTestEnv 是一套跑起来的 SFTP 服务端。
type sftpTestEnv struct {
	env       *testutil.Env
	tokens    *token.Manager
	server    *Server
	addr      string
	userID    int64
	secret    string
	signer    ssh.Signer
	keyLine   string
	shareName string
}

func newSFTPTestEnv(t *testing.T) *sftpTestEnv {
	t.Helper()
	env := testutil.New(t)
	pm := pool.New(env.DB, []pool.ChunkHandler{pool.NewLocalChunkHandler()})

	user := db.User{Username: "alice", PasswordHash: "x"}
	if err := env.DB.DB.Create(&user).Error; err != nil {
		t.Fatalf("建用户: %v", err)
	}
	p := env.NewPool("p", 2, 1, 8192)
	env.NewShare(p.Id, testutil.ShareOpts{
		Name: "docs", UserID: user.Id, Permission: db.ShareWrite,
	})
	// 没有授权记录的 Share：不该出现在该用户的视图里。
	env.NewShare(p.Id, testutil.ShareOpts{Name: "hidden"})

	tokens := token.NewManager(env.DB)
	secret, _, err := tokens.Create(user.Id, "sftp-secret", 0)
	if err != nil {
		t.Fatalf("建 token: %v", err)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("NewSignerFromKey: %v", err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("NewPublicKey: %v", err)
	}
	line := string(ssh.MarshalAuthorizedKey(key))
	if _, err := tokens.CreatePublicKey(user.Id, "mac", line, 0); err != nil {
		t.Fatalf("注册公钥: %v", err)
	}

	srv, err := New(pm, tokens, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	addr, err := srv.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	return &sftpTestEnv{
		env: env, tokens: tokens, server: srv, addr: addr.String(),
		userID: user.Id, secret: secret, signer: signer, keyLine: line, shareName: "docs",
	}
}

// sshConfig 返回只带一种认证方式的客户端配置。
func (e *sftpTestEnv) sshConfig(user string, auth ...ssh.AuthMethod) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User:            user,
		Auth:            auth,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}
}

// dial 建立 SSH + SFTP 客户端。
func (e *sftpTestEnv) dial(cfg *ssh.ClientConfig) (*psftp.Client, error) {
	conn, err := ssh.Dial("tcp", e.addr, cfg)
	if err != nil {
		return nil, err
	}
	c, err := psftp.NewClient(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

func (e *sftpTestEnv) dialPublicKey(user string) (*psftp.Client, error) {
	return e.dial(e.sshConfig(user, ssh.PublicKeys(e.signer)))
}

func (e *sftpTestEnv) dialPassword(user, password string) (*psftp.Client, error) {
	return e.dial(e.sshConfig(user, ssh.Password(password)))
}

// names 取出目录下的条目名（排序后）。
func names(t *testing.T, c *psftp.Client, dir string) []string {
	t.Helper()
	entries, err := c.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func TestSFTPPublicKeyRoundTrip(t *testing.T) {
	e := newSFTPTestEnv(t)
	c, err := e.dialPublicKey("alice")
	if err != nil {
		t.Fatalf("公钥登录失败: %v", err)
	}
	defer c.Close()

	// 根目录 = 该用户可见的 Share 列表：只有 docs，没有 hidden。
	got := names(t, c, "/")
	if len(got) != 1 || got[0] != e.shareName {
		t.Fatalf("根目录 = %v，期望只有 %q", got, e.shareName)
	}

	payload := []byte("hello from zenofs over sftp\n")
	f, err := c.Create("/" + e.shareName + "/hello.txt")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 读回
	r, err := c.Open("/" + e.shareName + "/hello.txt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()
	gotBytes, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(gotBytes, payload) {
		t.Fatalf("读回内容不一致: %q", gotBytes)
	}

	// Stat / ReadDir / Mkdir / Rename / Remove / StatVFS
	st, err := c.Stat("/" + e.shareName + "/hello.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Size() != int64(len(payload)) || st.IsDir() {
		t.Fatalf("Stat 结果异常: size=%d dir=%v", st.Size(), st.IsDir())
	}
	if got := names(t, c, "/"+e.shareName); len(got) != 1 || got[0] != "hello.txt" {
		t.Fatalf("ReadDir = %v", got)
	}
	if err := c.Mkdir("/" + e.shareName + "/sub"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := c.Rename("/"+e.shareName+"/hello.txt", "/"+e.shareName+"/sub/moved.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if got := names(t, c, "/"+e.shareName+"/sub"); len(got) != 1 || got[0] != "moved.txt" {
		t.Fatalf("改名后目录 = %v", got)
	}
	if err := c.Remove("/" + e.shareName + "/sub/moved.txt"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := c.Remove("/" + e.shareName + "/sub"); err != nil {
		t.Fatalf("Rmdir: %v", err)
	}
	fs, err := c.StatVFS("/" + e.shareName)
	if err != nil {
		t.Fatalf("StatVFS: %v", err)
	}
	if fs.Bsize == 0 {
		t.Fatalf("StatVFS 结果异常: %+v", fs)
	}
}

func TestSFTPSecretLoginAndFailures(t *testing.T) {
	e := newSFTPTestEnv(t)

	// token 当密码登录
	c, err := e.dialPassword("alice", e.secret)
	if err != nil {
		t.Fatalf("token 密码登录失败: %v", err)
	}
	if _, err := c.ReadDir("/"); err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	c.Close()

	// 错误 token
	if c, err := e.dialPassword("alice", "not-the-token"); err == nil {
		c.Close()
		t.Fatalf("错误 token 竟然登录成功")
	}
	// 未知用户
	if c, err := e.dialPassword("nobody", e.secret); err == nil {
		c.Close()
		t.Fatalf("未知用户竟然登录成功")
	}

	// 过期公钥凭证
	if err := e.env.DB.DB.Model(&db.AccessToken{}).
		Where("kind = ?", db.TokenPublicKey).
		Update("expires_at", time.Now().Add(-time.Minute).Unix()).Error; err != nil {
		t.Fatalf("改过期时间: %v", err)
	}
	if c, err := e.dialPublicKey("alice"); err == nil {
		c.Close()
		t.Fatalf("过期公钥竟然登录成功")
	}
	// 过期 password 凭证
	if err := e.env.DB.DB.Model(&db.AccessToken{}).
		Where("kind = ?", db.TokenSecret).
		Update("expires_at", time.Now().Add(-time.Minute).Unix()).Error; err != nil {
		t.Fatalf("改过期时间: %v", err)
	}
	if c, err := e.dialPassword("alice", e.secret); err == nil {
		c.Close()
		t.Fatalf("过期 token 竟然登录成功")
	}

	// 未注册的密钥
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥: %v", err)
	}
	otherSigner, err := ssh.NewSignerFromKey(otherPriv)
	if err != nil {
		t.Fatalf("NewSignerFromKey: %v", err)
	}
	if c, err := e.dial(e.sshConfig("alice", ssh.PublicKeys(otherSigner))); err == nil {
		c.Close()
		t.Fatalf("未注册密钥竟然登录成功")
	}
}

func TestSFTPReadOnlyShareRejectsWrite(t *testing.T) {
	e := newSFTPTestEnv(t)
	// 把 docs 改成只读
	if err := e.env.DB.DB.Model(&db.ShareUser{}).
		Where("user_id = ?", e.userID).
		Update("permission", db.ShareRead).Error; err != nil {
		t.Fatalf("改权限: %v", err)
	}

	c, err := e.dialPublicKey("alice")
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	defer c.Close()

	f, err := c.Create("/" + e.shareName + "/nope.txt")
	if err != nil {
		// 有些客户端在 Create 就会被拒；这也算通过。
		return
	}
	if _, err := f.Write([]byte("x")); err == nil {
		t.Fatalf("只读 Share 上写入竟然成功")
	}
	f.Close()
}

// 主机密钥应当持久化在 settings 表里，重启后复用（客户端不会看到指纹变化）。
func TestHostKeyPersists(t *testing.T) {
	env := testutil.New(t)
	pm := pool.New(env.DB, []pool.ChunkHandler{pool.NewLocalChunkHandler()})
	tokens := token.NewManager(env.DB)

	first, err := loadHostKey(env.DB, "")
	if err != nil {
		t.Fatalf("loadHostKey: %v", err)
	}
	stored := env.DB.GetSetting(hostKeySetting, "")
	if stored == "" {
		t.Fatalf("主机密钥未写入 settings")
	}
	second, err := loadHostKey(env.DB, "")
	if err != nil {
		t.Fatalf("第二次 loadHostKey: %v", err)
	}
	if !bytes.Equal(first.PublicKey().Marshal(), second.PublicKey().Marshal()) {
		t.Fatalf("两次加载的主机密钥不一致")
	}

	// 外部 PEM 文件优先
	path := filepath.Join(t.TempDir(), "host_key")
	if err := os.WriteFile(path, []byte(stored), 0o600); err != nil {
		t.Fatalf("写主机密钥文件: %v", err)
	}
	third, err := loadHostKey(env.DB, path)
	if err != nil {
		t.Fatalf("loadHostKey(file): %v", err)
	}
	if !bytes.Equal(first.PublicKey().Marshal(), third.PublicKey().Marshal()) {
		t.Fatalf("外部文件加载的主机密钥与库里不一致")
	}
	// 坏文件要报错，而不是静默换新键
	if _, err := loadHostKey(env.DB, filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatalf("缺失的主机密钥文件竟然没报错")
	}

	if _, err := New(pm, tokens, Options{}); err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := New(pm, tokens, Options{DisablePassword: true, DisablePublicKey: true}); err == nil {
		t.Fatalf("两种认证都关掉时应当报错")
	}
}

// 端口为 0 时 Start 应返回真实监听地址，并接受连接。
func TestStartReportsRealAddress(t *testing.T) {
	e := newSFTPTestEnv(t)
	if _, _, err := net.SplitHostPort(e.addr); err != nil {
		t.Fatalf("监听地址不可解析: %q", e.addr)
	}
}
