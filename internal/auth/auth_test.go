package auth

import (
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/otp"
)

// fixedNow 是本包测试用的固定时刻。
var fixedNow = time.Unix(1700000000, 0)

// newTestManager 在临时库里建好表并返回固定时钟的管理器。
func newTestManager(t *testing.T) *Manager {
	t.Helper()
	dbm, err := db.New("sqlite://" + filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { dbm.Close() })
	if err := dbm.AutoMigrate(); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	m, err := NewManager(dbm)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	m.now = func() time.Time { return fixedNow }
	return m
}

// codeNow 算当前（固定时刻）的验证码。
func codeNow(t *testing.T, secret string) string {
	t.Helper()
	code, err := otp.Code(secret, fixedNow)
	if err != nil {
		t.Fatalf("otp.Code: %v", err)
	}
	return code
}

func TestBootstrapFirstUserOnly(t *testing.T) {
	m := newTestManager(t)

	has, err := m.HasUsers()
	if err != nil || has {
		t.Fatalf("初始 HasUsers = %v, err=%v，期望 false", has, err)
	}

	secret, err := otp.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	u, generated, err := m.Bootstrap("root", "s3cret-pw", secret, codeNow(t, secret))
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if generated != "" {
		t.Fatalf("显式提供了密钥，不该再返回生成的明文: %q", generated)
	}
	if u.Role != db.UserAdmin {
		t.Fatalf("首个用户角色 = %d，期望 UserAdmin", u.Role)
	}
	if u.OTPSecret != secret || u.PasswordHash == "s3cret-pw" {
		t.Fatalf("用户字段不对: %+v", u)
	}

	// 第二个 bootstrap 必须失败
	other, err := otp.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Bootstrap("root2", "s3cret-pw", other, codeNow(t, other)); !errors.Is(err, ErrHasUsers) {
		t.Fatalf("第二次 Bootstrap err = %v，期望 ErrHasUsers", err)
	}
}

func TestCreateUserGeneratesSecretWhenMissing(t *testing.T) {
	m := newTestManager(t)
	admin, generated, err := m.Bootstrap("root", "s3cret-pw", "", "")
	if err != nil {
		t.Fatalf("Bootstrap（省略密钥）: %v", err)
	}
	if admin.OTPSecret == "" || generated != admin.OTPSecret {
		t.Fatalf("省略密钥时应自动生成并返回明文: user=%+v generated=%q", admin, generated)
	}

	// 用返回的密钥算码即可登录
	token, got, err := m.Login("root", "s3cret-pw", codeNow(t, generated))
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if got.Id != admin.Id || token == "" {
		t.Fatalf("Login 结果不对: %+v", got)
	}
}

func TestCreateUserValidation(t *testing.T) {
	m := newTestManager(t)
	secret, err := otp.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Bootstrap("root", "s3cret-pw", secret, codeNow(t, secret)); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	s2, err := otp.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	code := codeNow(t, s2)

	// 用户名重复
	if _, _, err := m.CreateUser("root", "another-pw", s2, code, db.UserNormal); !errors.Is(err, ErrUserExist) {
		t.Fatalf("重复用户名 err = %v，期望 ErrUserExist", err)
	}
	// 密码太短
	if _, _, err := m.CreateUser("bob", "short", s2, code, db.UserNormal); !errors.Is(err, ErrUserBad) {
		t.Fatalf("短密码 err = %v，期望 ErrUserBad", err)
	}
	// 用户名非法
	for _, name := range []string{"", "  ", "with space", "tab\tname"} {
		if _, _, err := m.CreateUser(name, "s3cret-pw", s2, code, db.UserNormal); !errors.Is(err, ErrUserBad) {
			t.Errorf("用户名 %q err = %v，期望 ErrUserBad", name, err)
		}
	}
	// 验证码错
	if _, _, err := m.CreateUser("bob", "s3cret-pw", s2, "000000", db.UserNormal); !errors.Is(err, ErrBadCode) {
		t.Fatalf("错误验证码 err = %v，期望 ErrBadCode", err)
	}
	// 密钥格式非法
	if _, _, err := m.CreateUser("bob", "s3cret-pw", "not-base32!!", code, db.UserNormal); !errors.Is(err, ErrBadSecret) {
		t.Fatalf("非法密钥 err = %v，期望 ErrBadSecret", err)
	}
	// 正常创建
	u, generated, err := m.CreateUser("bob", "s3cret-pw", s2, code, db.UserNormal)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if generated != "" || u.Role != db.UserNormal {
		t.Fatalf("普通用户字段不对: %+v", u)
	}
}

func TestLoginAndParseToken(t *testing.T) {
	m := newTestManager(t)
	secret, err := otp.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	u, _, err := m.Bootstrap("root", "s3cret-pw", secret, codeNow(t, secret))
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	code := codeNow(t, secret)

	// 密码错（与用户不存在返回同一个错误）
	if _, _, err := m.Login("root", "wrong-pw", code); !errors.Is(err, ErrBadLogin) {
		t.Fatalf("错误密码 err = %v，期望 ErrBadLogin", err)
	}
	if _, _, err := m.Login("nobody", "s3cret-pw", code); !errors.Is(err, ErrBadLogin) {
		t.Fatalf("未知用户 err = %v，期望 ErrBadLogin", err)
	}
	// 验证码错
	if _, _, err := m.Login("root", "s3cret-pw", "000000"); !errors.Is(err, ErrBadCode) {
		t.Fatalf("错误验证码 err = %v，期望 ErrBadCode", err)
	}

	token, got, err := m.Login("root", "s3cret-pw", code)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if got.Id != u.Id || got.Username != "root" {
		t.Fatalf("Login 返回的用户不对: %+v", got)
	}
	parsed, err := m.Parse(token)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.Id != u.Id || parsed.Username != "root" {
		t.Fatalf("Parse 结果不对: %+v", parsed)
	}

	// 篡改的 token
	if _, err := m.Parse(token + "x"); !errors.Is(err, ErrBadToken) {
		t.Fatalf("篡改 token err = %v，期望 ErrBadToken", err)
	}
	// 过期（把时钟往后拨过 TTL）
	m.now = func() time.Time { return fixedNow.Add(m.TTL() + time.Minute) }
	if _, err := m.Parse(token); !errors.Is(err, ErrBadToken) {
		t.Fatalf("过期 token err = %v，期望 ErrBadToken", err)
	}
	// 用户被删后，旧 token 也不该再通过
	m.now = func() time.Time { return fixedNow }
	if err := m.DeleteUser(u.Id); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if _, err := m.Parse(token); !errors.Is(err, ErrBadToken) {
		t.Fatalf("用户已删 err = %v，期望 ErrBadToken", err)
	}
}

func TestAuthenticateHeader(t *testing.T) {
	m := newTestManager(t)
	secret, err := otp.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Bootstrap("root", "s3cret-pw", secret, codeNow(t, secret)); err != nil {
		t.Fatal(err)
	}
	token, _, err := m.Login("root", "s3cret-pw", codeNow(t, secret))
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, header string
		wantErr      error
	}{
		{"没有头", "", ErrNoToken},
		{"不是 Bearer", "Basic abc", ErrNoToken},
		{"空 token", "Bearer ", ErrNoToken},
		{"坏 token", "Bearer garbage", ErrBadToken},
		{"正常", "Bearer " + token, nil},
	}
	for _, c := range cases {
		req, err := http.NewRequest(http.MethodGet, "/", nil)
		if err != nil {
			t.Fatal(err)
		}
		if c.header != "" {
			req.Header.Set("Authorization", c.header)
		}
		u, err := m.Authenticate(req)
		if c.wantErr == nil {
			if err != nil || u == nil || u.Username != "root" {
				t.Errorf("%s: err = %v, user = %+v", c.name, err, u)
			}
			continue
		}
		if !errors.Is(err, c.wantErr) {
			t.Errorf("%s: err = %v，期望 %v", c.name, err, c.wantErr)
		}
	}
}

func TestUserMaintenance(t *testing.T) {
	m := newTestManager(t)
	s1, err := otp.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	admin, _, err := m.Bootstrap("root", "s3cret-pw", s1, codeNow(t, s1))
	if err != nil {
		t.Fatal(err)
	}
	s2, err := otp.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	bob, _, err := m.CreateUser("bob", "bob-pw-123", s2, codeNow(t, s2), db.UserNormal)
	if err != nil {
		t.Fatal(err)
	}
	// 给 bob 一条 Share 授权与一个 access token，删除用户时应当一并清掉
	if err := m.db.DB.Create(&db.ShareUser{ShareId: 1, UserId: bob.Id, Permission: db.ShareWrite}).Error; err != nil {
		t.Fatal(err)
	}
	if err := m.db.DB.Create(&db.AccessToken{UserId: bob.Id, Name: "t", Kind: db.TokenSecret, TokenHash: []byte("x")}).Error; err != nil {
		t.Fatal(err)
	}

	users, err := m.ListUsers()
	if err != nil || len(users) != 2 {
		t.Fatalf("ListUsers = %d 条, err=%v", len(users), err)
	}
	if _, err := m.GetUser(9999); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("GetUser(9999) err = %v，期望 ErrUserNotFound", err)
	}

	// 改密码：旧密码失效、新密码可用
	if err := m.SetPassword(bob.Id, "new-pw-4567"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if _, _, err := m.Login("bob", "bob-pw-123", codeNow(t, s2)); !errors.Is(err, ErrBadLogin) {
		t.Fatalf("旧密码 err = %v，期望 ErrBadLogin", err)
	}
	if _, _, err := m.Login("bob", "new-pw-4567", codeNow(t, s2)); err != nil {
		t.Fatalf("新密码登录失败: %v", err)
	}
	if err := m.SetPassword(bob.Id, "short"); !errors.Is(err, ErrUserBad) {
		t.Fatalf("短密码 err = %v，期望 ErrUserBad", err)
	}

	// 改角色
	if err := m.SetRole(bob.Id, db.UserAdmin); err != nil {
		t.Fatalf("SetRole: %v", err)
	}
	if u, err := m.GetUser(bob.Id); err != nil || u.Role != db.UserAdmin {
		t.Fatalf("改角色后 = %+v, err=%v", u, err)
	}
	if err := m.SetRole(bob.Id, db.UserRole(9)); !errors.Is(err, ErrUserBad) {
		t.Fatalf("非法角色 err = %v，期望 ErrUserBad", err)
	}

	// 重置 OTP：不提供密钥时服务端生成并返回明文，老密钥立刻失效
	_, generated, err := m.ResetOTP(bob.Id, "", "")
	if err != nil {
		t.Fatalf("ResetOTP（自动生成）: %v", err)
	}
	if generated == "" {
		t.Fatalf("ResetOTP 应返回新密钥明文")
	}
	if _, _, err := m.Login("bob", "new-pw-4567", codeNow(t, s2)); !errors.Is(err, ErrBadCode) {
		t.Fatalf("重置后用旧密钥登录 err = %v，期望 ErrBadCode", err)
	}
	if _, _, err := m.Login("bob", "new-pw-4567", codeNow(t, generated)); err != nil {
		t.Fatalf("重置后用新密钥登录失败: %v", err)
	}
	// 显式提供密钥时要校验验证码
	newSecret, err := otp.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.ResetOTP(bob.Id, newSecret, "000000"); !errors.Is(err, ErrBadCode) {
		t.Fatalf("错误验证码重置 err = %v，期望 ErrBadCode", err)
	}
	if _, _, err := m.ResetOTP(bob.Id, newSecret, codeNow(t, newSecret)); err != nil {
		t.Fatalf("ResetOTP: %v", err)
	}

	// 删除用户：授权与凭证一并清掉
	if err := m.DeleteUser(bob.Id); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	var grants, tokens int64
	m.db.DB.Model(&db.ShareUser{}).Where("user_id = ?", bob.Id).Count(&grants)
	m.db.DB.Model(&db.AccessToken{}).Where("user_id = ?", bob.Id).Count(&tokens)
	if grants != 0 || tokens != 0 {
		t.Fatalf("删除用户后残留 grants=%d tokens=%d", grants, tokens)
	}
	if err := m.DeleteUser(admin.Id + 999); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("删除不存在的用户 err = %v，期望 ErrUserNotFound", err)
	}
}

func TestManagerReusesStoredSecret(t *testing.T) {
	dbm, err := db.New("sqlite://" + filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbm.Close() })
	if err := dbm.AutoMigrate(); err != nil {
		t.Fatal(err)
	}
	first, err := NewManager(dbm)
	if err != nil {
		t.Fatal(err)
	}
	stored := dbm.GetSetting(secretSetting, "")
	if stored == "" {
		t.Fatalf("JWT 密钥未写入 settings")
	}
	second, err := NewManager(dbm)
	if err != nil {
		t.Fatal(err)
	}
	if string(first.secret) != string(second.secret) {
		t.Fatalf("两次 NewManager 的密钥不一致")
	}
	// 自定义 TTL
	if err := dbm.DB.Create(&db.Setting{Name: ttlSetting, Value: "60"}).Error; err != nil {
		t.Fatal(err)
	}
	third, err := NewManager(dbm)
	if err != nil {
		t.Fatal(err)
	}
	if third.TTL() != time.Minute {
		t.Fatalf("JWT_TTL=60 时 TTL = %v，期望 1m", third.TTL())
	}
	// 非法 TTL 要报错而不是静默取默认值
	if err := dbm.DB.Model(&db.Setting{}).Where("name = ?", ttlSetting).Update("value", "abc").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := NewManager(dbm); err == nil {
		t.Fatalf("非法 JWT_TTL 应当报错")
	}
}
