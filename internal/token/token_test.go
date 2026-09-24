package token

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
	"golang.org/x/crypto/ssh"
)

// newTestManager 在临时 SQLite 库里建好表并插入一个用户。
func newTestManager(t *testing.T, username string) (*Manager, int64) {
	t.Helper()
	mgr, err := db.New("sqlite://" + filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { mgr.Close() })
	if err := mgr.AutoMigrate(); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	user := db.User{Username: username, PasswordHash: "x"}
	if err := mgr.DB.Create(&user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	return NewManager(mgr), user.Id
}

// newTestKey 生成一对临时 SSH 密钥，返回 authorized_keys 行。
func newTestKey(t *testing.T) (ssh.PublicKey, string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("NewPublicKey: %v", err)
	}
	return key, string(ssh.MarshalAuthorizedKey(key))
}

func TestGenerate(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 32; i++ {
		tok, err := Generate()
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		// 32 字节 base64url 无填充 = 43 个字符
		if len(tok) != 43 {
			t.Fatalf("token 长度 = %d，期望 43", len(tok))
		}
		if seen[tok] {
			t.Fatalf("Generate 产生了重复 token")
		}
		seen[tok] = true
	}
}

func TestNTHashMatchesProtocolVector(t *testing.T) {
	// MS-NLMP 的著名测试向量：NTOWFv1("") = MD4(UTF-16LE(""))。
	const want = "31d6cfe0d16ae931b73c59d7e0c089c0"
	if got := hex.EncodeToString(NTHash("")); got != want {
		t.Fatalf("NTHash(\"\") = %s，期望 %s", got, want)
	}
	if len(NTHash("zenofs")) != 16 {
		t.Fatalf("NT hash 长度应为 16 字节")
	}
}

func TestCreateSecretStoresOnlyDigests(t *testing.T) {
	m, userID := newTestManager(t, "alice")
	plain, tok, err := m.Create(userID, "laptop", 0)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if tok.Kind != db.TokenSecret || tok.UserId != userID || tok.Name != "laptop" {
		t.Fatalf("凭证字段不符: %+v", tok)
	}
	if tok.PublicKey != "" || tok.Fingerprint != "" {
		t.Fatalf("token 凭证不应有公钥字段: %+v", tok)
	}
	if len(tok.NTHash) != 16 {
		t.Fatalf("NT hash 长度 = %d，期望 16", len(tok.NTHash))
	}

	// 库里查到的记录只有摘要：明文不出现在任何列里。
	got, err := m.Get(tok.Id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.TokenHash) != string(Sum(plain)) {
		t.Fatalf("token_hash 与 Sum(明文) 不一致")
	}
	var rows []string
	if err := m.db.DB.Raw("SELECT token_hash FROM access_tokens").Scan(&rows).Error; err != nil {
		t.Fatalf("读回 token_hash: %v", err)
	}
	if len(rows) != 1 || strings.Contains(rows[0], plain) {
		t.Fatalf("token 明文疑似落库: %v", rows)
	}
}

func TestAuthenticateSecret(t *testing.T) {
	m, userID := newTestManager(t, "alice")
	plain, _, err := m.Create(userID, "laptop", 0)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// 另一个用户，用来验证"token 不能跨用户使用"。
	other := db.User{Username: "bob", PasswordHash: "x"}
	if err := m.db.DB.Create(&other).Error; err != nil {
		t.Fatalf("create bob: %v", err)
	}

	if _, err := m.AuthenticateSecret("alice", plain); err != nil {
		t.Fatalf("正确 token 认证失败: %v", err)
	}
	if _, err := m.AuthenticateSecret("ALICE", plain); err != nil {
		t.Fatalf("用户名大小写不敏感应通过: %v", err)
	}
	for _, tc := range []struct{ name, user, secret string }{
		{"错误 token", "alice", "not-the-token"},
		{"空 token", "alice", ""},
		{"未知用户", "carol", plain},
		{"token 属于别人", "bob", plain},
	} {
		_, err := m.AuthenticateSecret(tc.user, tc.secret)
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v，期望 ErrInvalid", tc.name, err)
		}
	}
}

func TestExpiry(t *testing.T) {
	m, userID := newTestManager(t, "alice")
	plain, tok, err := m.Create(userID, "expired", time.Now().Add(-time.Minute).Unix())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !Expired(tok) {
		t.Fatalf("Expired 应为 true")
	}
	if _, err := m.AuthenticateSecret("alice", plain); !errors.Is(err, ErrExpired) {
		t.Fatalf("过期 token 认证 err = %v，期望 ErrExpired", err)
	}
	if _, hashes, err := m.SMBHashes("alice"); err != nil || len(hashes) != 0 {
		t.Fatalf("过期凭证不应出现在 SMB 候选里: hashes=%d err=%v", len(hashes), err)
	}
}

func TestPublicKeyLifecycle(t *testing.T) {
	m, userID := newTestManager(t, "alice")
	key, line := newTestKey(t)

	tok, err := m.CreatePublicKey(userID, "mac", line, 0)
	if err != nil {
		t.Fatalf("CreatePublicKey: %v", err)
	}
	if tok.Kind != db.TokenPublicKey || !strings.HasPrefix(tok.Fingerprint, "SHA256:") {
		t.Fatalf("公钥凭证字段不符: %+v", tok)
	}
	if !strings.Contains(tok.PublicKey, "ssh-ed25519") {
		t.Fatalf("公钥未明文落库: %q", tok.PublicKey)
	}
	if len(tok.TokenHash) != 0 || len(tok.NTHash) != 0 {
		t.Fatalf("公钥凭证不应有 token 摘要")
	}

	if _, err := m.AuthenticatePublicKey("alice", key); err != nil {
		t.Fatalf("公钥认证失败: %v", err)
	}
	// 另一把密钥必须被拒。
	otherKey, otherLine := newTestKey(t)
	if _, err := m.AuthenticatePublicKey("alice", otherKey); !errors.Is(err, ErrInvalid) {
		t.Fatalf("未注册公钥 err = %v，期望 ErrInvalid", err)
	}
	// 注册到 bob 名下后，alice 不能再用它登录。
	bob := db.User{Username: "bob", PasswordHash: "x"}
	if err := m.db.DB.Create(&bob).Error; err != nil {
		t.Fatalf("create bob: %v", err)
	}
	if _, err := m.CreatePublicKey(bob.Id, "bob-mac", otherLine, 0); err != nil {
		t.Fatalf("CreatePublicKey(bob): %v", err)
	}
	if _, err := m.AuthenticatePublicKey("alice", otherKey); !errors.Is(err, ErrInvalid) {
		t.Fatalf("跨用户公钥 err = %v，期望 ErrInvalid", err)
	}

	// 非法公钥
	if _, err := m.CreatePublicKey(userID, "bad", "not-a-key", 0); !errors.Is(err, ErrBadKey) {
		t.Fatalf("非法公钥 err = %v，期望 ErrBadKey", err)
	}
	// 未知用户
	if _, _, err := m.Create(userID+999, "ghost", 0); !errors.Is(err, ErrBadUser) {
		t.Fatalf("未知用户 err = %v，期望 ErrBadUser", err)
	}
}

func TestListAndRevoke(t *testing.T) {
	m, userID := newTestManager(t, "alice")
	if _, _, err := m.Create(userID, "one", 0); err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, keyID := newTestKey(t)
	tok, err := m.CreatePublicKey(userID, "two", keyID, 0)
	if err != nil {
		t.Fatalf("CreatePublicKey: %v", err)
	}

	all, err := m.List(userID, nil)
	if err != nil || len(all) != 2 {
		t.Fatalf("List = %d 条, err=%v，期望 2 条", len(all), err)
	}
	secrets := db.TokenSecret
	only, err := m.List(userID, &secrets)
	if err != nil || len(only) != 1 || only[0].Name != "one" {
		t.Fatalf("按 kind 过滤失败: %+v err=%v", only, err)
	}

	if err := m.Revoke(tok.Id); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := m.Get(tok.Id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("吊销后 Get err = %v，期望 ErrNotFound", err)
	}
	if err := m.Revoke(tok.Id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复吊销 err = %v，期望 ErrNotFound", err)
	}
}

func TestSMBHashes(t *testing.T) {
	m, userID := newTestManager(t, "alice")
	plain, tok, err := m.Create(userID, "smb", 0)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, line := newTestKey(t)
	if _, err := m.CreatePublicKey(userID, "mac", line, 0); err != nil {
		t.Fatalf("CreatePublicKey: %v", err)
	}

	gotUser, hashes, err := m.SMBHashes("ALICE")
	if err != nil {
		t.Fatalf("SMBHashes: %v", err)
	}
	if gotUser != userID {
		t.Fatalf("SMBHashes userID = %d，期望 %d", gotUser, userID)
	}
	if len(hashes) != 1 || string(hashes[0]) != string(NTHash(plain)) {
		t.Fatalf("SMB 候选摘要不符: %d 条", len(hashes))
	}

	// 未知用户的 SMB 登录按 ErrInvalid 处理（不暴露用户是否存在）。
	if _, _, err := m.SMBHashes("nobody"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("未知用户 err = %v，期望 ErrInvalid", err)
	}

	// Touch 只更新 LastUsedAt，不改变摘要。
	if _, err := m.AuthenticateSecret("alice", plain); err != nil {
		t.Fatalf("AuthenticateSecret: %v", err)
	}
	got, err := m.Get(tok.Id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LastUsedAt == 0 {
		t.Fatalf("LastUsedAt 未更新")
	}
	if string(got.NTHash) != string(NTHash(plain)) {
		t.Fatalf("Touch 改动了摘要")
	}
}

// 确认哨兵错误带上了系统错误码，协议层可以据此映射。
func TestSentinelCodes(t *testing.T) {
	var ze *errs.ZenoError
	if !errors.As(error(ErrExpired), &ze) || ze.StrCode != errs.ESTR_TOKEN_EXPIRED {
		t.Fatalf("ErrExpired 未携带错误码: %v", ErrExpired)
	}
}
