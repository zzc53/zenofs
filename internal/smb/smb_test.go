package smb

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	gsmb "github.com/jfjallid/go-smb/smb"
	"github.com/jfjallid/go-smb/spnego"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/pool"
	"github.com/zzc53/zenofs/internal/testutil"
	"github.com/zzc53/zenofs/internal/token"
	"github.com/zzc53/zenofs/internal/vfs"
)

// ─────────────────────────────────────────────────────────────
// 纯函数
// ─────────────────────────────────────────────────────────────

func TestVFSPath(t *testing.T) {
	cases := []struct {
		in    string
		want  string
		valid bool
	}{
		{"", "/", true},
		{"\\", "/", true},
		{"\\docs\\a.txt", "/docs/a.txt", true},
		{"docs\\a.txt", "/docs/a.txt", true},
		{"\\docs\\.\\a.txt", "/docs/a.txt", true},
		{"\\..\\etc\\passwd", "", false},
		{"\\docs\\..\\..\\x", "", false},
		{"\\a\\\\b", "", false},
		{"\\a/b", "", false},
		{"\\a\x00b", "", false},
	}
	for _, c := range cases {
		got, ok := vfsPath(c.in)
		if ok != c.valid || (ok && got != c.want) {
			t.Errorf("vfsPath(%q) = %q,%v；期望 %q,%v", c.in, got, ok, c.want, c.valid)
		}
	}
}

func TestBaseName(t *testing.T) {
	for in, want := range map[string]string{"": "/", "\\": "/", "\\a.txt": "a.txt", "\\d\\e.txt": "e.txt"} {
		if got := baseName(in); got != want {
			t.Errorf("baseName(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestDesiredWrite(t *testing.T) {
	if desiredWrite(0x00120089) { // FILE_READ_DATA|FILE_READ_ATTRIBUTES 之类的只读组合
		t.Errorf("只读访问掩码被判成写")
	}
	for _, access := range []uint32{accessWriteData, accessAppendData, accessDelete, accessGenericWrite} {
		if !desiredWrite(access) {
			t.Errorf("access=0x%08x 应判成写", access)
		}
	}
}

func TestMatchPattern(t *testing.T) {
	cases := []struct {
		name, pattern string
		want          bool
	}{
		{"a.txt", "*", true},
		{"a.txt", "*.txt", true},
		{"a.txt", "*.TXT", true},
		{"a.txt", "b.txt", false},
		{"abc", "a?c", true},
		{"abcd", "a?c", false},
		{"a.txt", "[", false}, // 非法 pattern：退化成精确比较（大小写不敏感）
	}
	for _, c := range cases {
		if got := matchPattern(c.name, c.pattern); got != c.want {
			t.Errorf("matchPattern(%q,%q) = %v，期望 %v", c.name, c.pattern, got, c.want)
		}
	}
}

func TestStatusOf(t *testing.T) {
	cases := []struct {
		err  error
		want uint32
	}{
		{nil, gsmb.StatusOk},
		{vfs.ErrNotExist, gsmb.StatusObjectNameNotFound},
		{vfs.ErrExist, gsmb.StatusObjectNameCollision},
		{vfs.ErrNotEmpty, gsmb.StatusDirectoryNotEmpty},
		{vfs.ErrIsDir, gsmb.StatusFileIsADirectory},
		{vfs.ErrNotDir, gsmb.StatusNotADirectory},
		{vfs.ErrReadOnly, statusMediaWriteProtected},
		{vfs.ErrNoSpace, statusDiskFull},
		{vfs.ErrNotSupported, gsmb.StatusNotSupported},
		{vfs.ErrBusy, statusSharingViolation},
		{vfs.ErrNameTooLong, statusNameTooLong},
		{vfs.ErrCrossDevice, statusNotSameDevice},
		{vfs.ErrPermission, gsmb.StatusAccessDenied},
		{vfs.ErrEncrypted, gsmb.StatusAccessDenied},
		{vfs.ErrInvalid, gsmb.StatusInvalidParameter},
	}
	for _, c := range cases {
		if got := statusOf(c.err); got != c.want {
			t.Errorf("statusOf(%v) = 0x%08x，期望 0x%08x", c.err, got, c.want)
		}
	}
	// vfs 的错误被包了一层 ZenoError 时也要认得。
	if got := statusOf(&wrappedErr{inner: vfs.ErrNotExist}); got != gsmb.StatusObjectNameNotFound {
		t.Errorf("包装后的 ErrNotExist 映射 = 0x%08x", got)
	}
}

type wrappedErr struct{ inner error }

func (e *wrappedErr) Error() string { return "wrapped: " + e.inner.Error() }
func (e *wrappedErr) Unwrap() error { return e.inner }

func TestFileTimeRoundTrip(t *testing.T) {
	// 2010-01-01T00:00:00Z 的 FILETIME：(Unix 秒 1262304000 + 11644473600) ×10^7。
	const ft = 129067776000000000
	got := fileTimeToTime(ft)
	if got.UTC().Format(time.RFC3339) != "2010-01-01T00:00:00Z" {
		t.Fatalf("fileTimeToTime = %v", got)
	}
	if !fileTimeToTime(0).IsZero() {
		t.Fatalf("FILETIME 0 应转成零值时间")
	}
}

// ─────────────────────────────────────────────────────────────
// 端到端：客户端 → SMB 服务端 → zenofs vfs
// ─────────────────────────────────────────────────────────────

// smbTestEnv 是一套跑起来的 SMB 服务端 + 客户端连接参数。
type smbTestEnv struct {
	env    *testutil.Env
	pm     *pool.PoolManager
	tokens *token.Manager
	server *Server
	addr   net.Addr
	userID int64
	share  db.Share
	plain  string
}

func newSMBTestEnv(t *testing.T) *smbTestEnv {
	t.Helper()
	env := testutil.New(t)
	pm := pool.New(env.DB, []pool.ChunkHandler{pool.NewLocalChunkHandler()})

	user := db.User{Username: "alice", PasswordHash: "x"}
	if err := env.DB.DB.Create(&user).Error; err != nil {
		t.Fatalf("建用户: %v", err)
	}
	p := env.NewPool("p", 2, 1, 8192)
	share := env.NewShare(p.Id, testutil.ShareOpts{
		Name: "docs", UserID: user.Id, Permission: db.ShareWrite,
	})
	// 另一个没有任何授权的 Share，用来验证挂载鉴权。
	env.NewShare(p.Id, testutil.ShareOpts{Name: "hidden"})

	tokens := token.NewManager(env.DB)
	plain, _, err := tokens.Create(user.Id, "smb-test", 0)
	if err != nil {
		t.Fatalf("建 token: %v", err)
	}

	shares, err := LoadShares(pm)
	if err != nil {
		t.Fatalf("LoadShares: %v", err)
	}
	srv := New(pm, tokens, shares, Options{})
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
	return &smbTestEnv{env: env, pm: pm, tokens: tokens, server: srv, addr: addr, userID: user.Id, share: share, plain: plain}
}

// dial 用"用户名 + 密码（token 明文）"连服务端；返回 nil error 表示登录成功。
func (e *smbTestEnv) dial(user, password string) (*gsmb.Connection, error) {
	tcp, ok := e.addr.(*net.TCPAddr)
	if !ok {
		return nil, io.ErrUnexpectedEOF
	}
	return gsmb.NewConnection(gsmb.Options{
		Host:        "127.0.0.1",
		Port:        tcp.Port,
		Initiator:   &spnego.NTLMInitiator{User: user, Password: password, Domain: "WORKGROUP"},
		Dialects:    gsmb.DialectsSMB2Only,
		DialTimeout: 5 * time.Second,
	})
}

func TestSMBTokenLoginFileRoundTrip(t *testing.T) {
	e := newSMBTestEnv(t)

	c, err := e.dial("alice", e.plain)
	if err != nil {
		t.Fatalf("用 token 登录失败: %v", err)
	}
	defer c.Close()

	payload := []byte("hello from zenofs over smb\n")
	src := bytes.NewReader(payload)
	if err := c.PutFile(e.share.Name, "hello.txt", 0, func(buf []byte) (int, error) {
		n, err := src.Read(buf)
		if err == io.EOF && n == 0 {
			return 0, io.EOF
		}
		return n, err
	}); err != nil {
		t.Fatalf("PutFile: %v", err)
	}

	var got bytes.Buffer
	if err := c.RetrieveFile(e.share.Name, "hello.txt", 0, func(b []byte) (int, error) {
		got.Write(b)
		return len(b), nil
	}); err != nil {
		t.Fatalf("RetrieveFile: %v", err)
	}
	if !bytes.Equal(got.Bytes(), payload) {
		t.Fatalf("读回的字节不一致: %q", got.String())
	}

	// 内容应该真的进了 zenofs 的存储层（vfs 再读一遍）。
	fs := vfs.NewShareFS(e.pm, e.share, e.userID, db.ShareWrite)
	f, err := fs.Open(t.Context(), "/hello.txt", vfs.OpenFlags{Read: true}, 0)
	if err != nil {
		t.Fatalf("vfs Open: %v", err)
	}
	defer f.Close()
	buf := make([]byte, len(payload))
	if n, err := f.ReadAt(buf, 0); n != len(payload) || err != nil && err != io.EOF {
		t.Fatalf("vfs ReadAt: n=%d err=%v", n, err)
	}
	if !bytes.Equal(buf, payload) {
		t.Fatalf("vfs 里的内容不一致: %q", buf)
	}
}

func TestSMBLoginRejectsBadAndExpiredTokens(t *testing.T) {
	e := newSMBTestEnv(t)

	// 错误 token
	if c, err := e.dial("alice", "not-the-token"); err == nil {
		c.Close()
		t.Fatalf("错误 token 竟然登录成功")
	}
	// 未知用户
	if c, err := e.dial("nobody", e.plain); err == nil {
		c.Close()
		t.Fatalf("未知用户竟然登录成功")
	}
	// 过期 token：把唯一凭证改成已过期
	if err := e.env.DB.DB.Model(&db.AccessToken{}).Where("user_id = ?", e.userID).
		Update("expires_at", time.Now().Add(-time.Minute).Unix()).Error; err != nil {
		t.Fatalf("改过期时间: %v", err)
	}
	if c, err := e.dial("alice", e.plain); err == nil {
		c.Close()
		t.Fatalf("过期 token 竟然登录成功")
	}
}

func TestSMBUnauthorizedShareDenied(t *testing.T) {
	e := newSMBTestEnv(t)

	c, err := e.dial("alice", e.plain)
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	defer c.Close()

	// "hidden" 没有 share_users 记录：连得上共享，但一操作就该被拒。
	src := strings.NewReader("x")
	err = c.PutFile("hidden", "a.txt", 0, func(buf []byte) (int, error) {
		n, rerr := src.Read(buf)
		if rerr == io.EOF && n == 0 {
			return 0, io.EOF
		}
		return n, rerr
	})
	if err == nil {
		t.Fatalf("对未授权 Share 的写入竟然成功")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "denied") {
		t.Logf("（提示）被拒的错误文本：%v", err)
	}
}
