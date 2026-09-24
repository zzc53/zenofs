// Package auth 管用户与登录态：密码用 bcrypt，二次验证用 TOTP（internal/otp），
// 登录成功签发 HS256 的 JWT（有效期默认 24 小时，可用 settings.JWT_TTL 调）。
//
// 签名密钥存在 settings 表（键 JWT_SECRET），首次使用时自动生成 32 字节随机值：
// 删掉这一行或换库，已签发的 token 全部失效。
//
// 这里只做逻辑（校验、签发、解析、用户读写），HTTP 中间件在 internal/api 里，
// 这样错误响应格式与其它端点保持一致。
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
	"github.com/zzc53/zenofs/internal/otp"
)

const (
	// DefaultTTL 是 JWT 的默认有效期（settings.JWT_TTL 以秒为单位覆盖它）。
	DefaultTTL = 24 * time.Hour
	// ttlSetting / secretSetting 是 settings 表里的键名。
	ttlSetting    = "JWT_TTL"
	secretSetting = "JWT_SECRET"
	// secretBytes 是自动生成签名密钥的随机长度。
	secretBytes = 32
	// minPasswordLen 是密码最小长度。
	minPasswordLen = 8
	// maxUsernameLen 是用户名最大长度。
	maxUsernameLen = 64
)

// 用户与登录态相关的哨兵错误（都是 *errs.ZenoError，API 层可直接映射）。
var (
	// ErrBadLogin 用户名或密码错误——两种情况共用一个错误，不暴露用户名是否存在。
	ErrBadLogin = errs.New(errs.ECODE_AUTH_BAD_LOGIN, errs.ESTR_AUTH_BAD_LOGIN, "auth: bad username or password", "")
	// ErrBadCode 二次验证码错误。
	ErrBadCode = errs.New(errs.ECODE_OTP_INVALID, errs.ESTR_OTP_INVALID, "auth: bad otp code", "")
	// ErrBadSecret OTP 密钥格式非法。
	ErrBadSecret = errs.New(errs.ECODE_OTP_BAD_SECRET, errs.ESTR_OTP_BAD_SECRET, "auth: bad otp secret", "")
	// ErrNoToken 请求缺少 Bearer token。
	ErrNoToken = errs.New(errs.ECODE_AUTH_NO_TOKEN, errs.ESTR_AUTH_NO_TOKEN, "auth: missing bearer token", "")
	// ErrBadToken token 无效或已过期。
	ErrBadToken = errs.New(errs.ECODE_AUTH_BAD_TOKEN, errs.ESTR_AUTH_BAD_TOKEN, "auth: invalid or expired token", "")
	// ErrNotAdmin 需要管理员权限。
	ErrNotAdmin = errs.New(errs.ECODE_AUTH_NOT_ADMIN, errs.ESTR_AUTH_NOT_ADMIN, "auth: admin required", "")
	// ErrHasUsers 系统里已有用户，不能再 bootstrap。
	ErrHasUsers = errs.New(errs.ECODE_AUTH_DONE, errs.ESTR_AUTH_DONE, "auth: users already exist", "")
	// ErrUserExist 用户名已存在。
	ErrUserExist = errs.New(errs.ECODE_USER_EXIST, errs.ESTR_USER_EXIST, "auth: username already taken", "")
	// ErrUserNotFound 用户不存在。
	ErrUserNotFound = errs.New(errs.ECODE_USER_NOT_FOUND, errs.ESTR_USER_NOT_FOUND, "auth: user not found", "")
	// ErrUserBad 用户参数非法（用户名/密码/角色等）。
	ErrUserBad = errs.New(errs.ECODE_USER_BAD, errs.ESTR_USER_BAD, "auth: invalid user parameters", "")
)

// claims 是 JWT 的载荷：主体是用户 id（sub），附带用户名与角色便于前端显示。
type claims struct {
	Username string `json:"username"`
	Role     int8   `json:"role"`
	jwt.RegisteredClaims
}

// Manager 读写 users 表并签发/校验 JWT。
type Manager struct {
	db     *db.DbManager
	secret []byte
	ttl    time.Duration
	now    func() time.Time // 便于测试
}

// NewManager 创建认证管理器：从 settings 里取（或生成）JWT 密钥与有效期。
func NewManager(dbManager *db.DbManager) (*Manager, error) {
	secret, err := loadSecret(dbManager)
	if err != nil {
		return nil, err
	}
	ttl := DefaultTTL
	if v := dbManager.GetSetting(ttlSetting, ""); v != "" {
		secs, err := strconv.ParseInt(v, 10, 64)
		if err != nil || secs <= 0 {
			return nil, errs.New(errs.ECODE_AUTH_BAD_TOKEN, errs.ESTR_AUTH_BAD_TOKEN,
				"auth: bad JWT_TTL setting", v)
		}
		ttl = time.Duration(secs) * time.Second
	}
	return &Manager{db: dbManager, secret: secret, ttl: ttl, now: time.Now}, nil
}

// TTL 返回当前配置的 token 有效期。
func (m *Manager) TTL() time.Duration { return m.ttl }

// loadSecret 取 JWT 签名密钥：settings 里没有就生成并存进去。
func loadSecret(dbm *db.DbManager) ([]byte, error) {
	if v := dbm.GetSetting(secretSetting, ""); v != "" {
		return []byte(v), nil
	}
	buf := make([]byte, secretBytes)
	if _, err := rand.Read(buf); err != nil {
		return nil, errs.FromError(err, errs.ECODE_CRYPTO_ERROR, errs.ESTR_CRYPTO_ERROR)
	}
	secret := hex.EncodeToString(buf)
	if err := dbm.DB.Create(&db.Setting{Name: secretSetting, Value: secret}).Error; err != nil {
		return nil, errs.DBQuery(err)
	}
	return []byte(secret), nil
}

// ─────────────────────────────────────────────────────────────
// 用户
// ─────────────────────────────────────────────────────────────

// HasUsers 报告系统里是否已有用户（没有则允许 bootstrap 第一个管理员）。
func (m *Manager) HasUsers() (bool, error) {
	var n int64
	if err := m.db.DB.Model(&db.User{}).Count(&n).Error; err != nil {
		return false, errs.DBQuery(err)
	}
	return n > 0, nil
}

// HashPassword 用 bcrypt 生成密码哈希。
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", errs.FromError(err, errs.ECODE_CRYPTO_ERROR, errs.ESTR_CRYPTO_ERROR)
	}
	return string(hash), nil
}

// CreateUser 创建用户。
//
// otpSecret 为空时服务端自动生成一个（第二个返回值就是明文，只此一次），此时
// 不校验 otpCode——管理员还没机会把密钥录进 App；otpSecret 非空时必须带上当前
// 的六位 otpCode，用来证明密钥录入正确。role 决定权限，首个用户固定为管理员。
func (m *Manager) CreateUser(username, password, otpSecret, otpCode string, role db.UserRole) (*db.User, string, error) {
	username = strings.TrimSpace(username)
	if err := checkUsername(username); err != nil {
		return nil, "", err
	}
	if len(password) < minPasswordLen {
		return nil, "", ErrUserBad
	}

	secret := strings.TrimSpace(otpSecret)
	generated := ""
	if secret == "" {
		// 管理员没提供密钥：服务端生成一个并把明文返回一次。此时没法校验验证码
		//（管理员还没来得及把它录进 App），所以跳过校验；用户下一次登录起必须带码。
		var err error
		secret, err = otp.GenerateSecret()
		if err != nil {
			return nil, "", err
		}
		generated = secret
	} else {
		if _, err := otp.Code(secret, m.now()); err != nil {
			return nil, "", ErrBadSecret
		}
		if !otp.Verify(secret, otpCode, m.now()) {
			return nil, "", ErrBadCode
		}
	}

	exists, err := m.usernameTaken(username)
	if err != nil {
		return nil, "", err
	}
	if exists {
		return nil, "", ErrUserExist
	}

	hash, err := HashPassword(password)
	if err != nil {
		return nil, "", err
	}
	u := &db.User{Username: username, PasswordHash: hash, OTPSecret: secret, Role: role}
	if err := m.db.DB.Create(u).Error; err != nil {
		return nil, "", errs.DBQuery(err)
	}
	return u, generated, nil
}

// Bootstrap 创建第一个用户，并且必须是管理员。
// 系统里已经有用户时返回 ErrHasUsers。
func (m *Manager) Bootstrap(username, password, otpSecret, otpCode string) (*db.User, string, error) {
	has, err := m.HasUsers()
	if err != nil {
		return nil, "", err
	}
	if has {
		return nil, "", ErrHasUsers
	}
	return m.CreateUser(username, password, otpSecret, otpCode, db.UserAdmin)
}

// ListUsers 列出所有用户（按 id）。
func (m *Manager) ListUsers() ([]db.User, error) {
	var users []db.User
	if err := m.db.DB.Order("id").Find(&users).Error; err != nil {
		return nil, errs.DBQuery(err)
	}
	return users, nil
}

// GetUser 按 id 取用户。
func (m *Manager) GetUser(id int64) (*db.User, error) {
	var u db.User
	err := m.db.DB.First(&u, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, errs.DBQuery(err)
	}
	return &u, nil
}

// DeleteUser 删除用户（同时清掉它的 Share 授权与访问凭证）。
func (m *Manager) DeleteUser(id int64) error {
	return m.db.DB.Transaction(func(tx *gorm.DB) error {
		res := tx.Delete(&db.User{}, id)
		if res.Error != nil {
			return errs.DBQuery(res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrUserNotFound
		}
		if err := tx.Where("user_id = ?", id).Delete(&db.ShareUser{}).Error; err != nil {
			return errs.DBQuery(err)
		}
		if err := tx.Where("user_id = ?", id).Delete(&db.AccessToken{}).Error; err != nil {
			return errs.DBQuery(err)
		}
		return nil
	})
}

// SetPassword 改密码。
func (m *Manager) SetPassword(id int64, password string) error {
	if len(password) < minPasswordLen {
		return ErrUserBad
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	return m.updateUser(id, map[string]any{"password_hash": hash})
}

// SetRole 改角色。
func (m *Manager) SetRole(id int64, role db.UserRole) error {
	if role != db.UserNormal && role != db.UserAdmin {
		return ErrUserBad
	}
	return m.updateUser(id, map[string]any{"role": role})
}

// ResetOTP 重置二次验证密钥（同样需要当前验证码，密钥为空则自动生成）。
func (m *Manager) ResetOTP(id int64, otpSecret, otpCode string) (*db.User, string, error) {
	secret := strings.TrimSpace(otpSecret)
	generated := ""
	if secret == "" {
		// 与 CreateUser 一致：没提供密钥就生成一个，这时不校验验证码。
		var err error
		secret, err = otp.GenerateSecret()
		if err != nil {
			return nil, "", err
		}
		generated = secret
	} else {
		if _, err := otp.Code(secret, m.now()); err != nil {
			return nil, "", ErrBadSecret
		}
		if !otp.Verify(secret, otpCode, m.now()) {
			return nil, "", ErrBadCode
		}
	}
	if err := m.updateUser(id, map[string]any{"otp_secret": secret}); err != nil {
		return nil, "", err
	}
	u, err := m.GetUser(id)
	if err != nil {
		return nil, "", err
	}
	return u, generated, nil
}

// ─────────────────────────────────────────────────────────────
// 登录态
// ─────────────────────────────────────────────────────────────

// Login 校验密码与二次验证码，成功则签发 JWT。
//
// 用户不存在与密码错误返回同一个 ErrBadLogin；验证码错误单独返回 ErrBadCode
// （此时密码已经对了，把原因告诉客户端不会泄露额外信息）。
func (m *Manager) Login(username, password, otpCode string) (string, *db.User, error) {
	var u db.User
	err := m.db.DB.Where("username = ?", strings.TrimSpace(username)).First(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil, ErrBadLogin
	}
	if err != nil {
		return "", nil, errs.DBQuery(err)
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		return "", nil, ErrBadLogin
	}
	if u.OTPSecret == "" || !otp.Verify(u.OTPSecret, otpCode, m.now()) {
		return "", nil, ErrBadCode
	}
	token, err := m.Issue(&u)
	if err != nil {
		return "", nil, err
	}
	return token, &u, nil
}

// Issue 给一个已确认的用户签发 JWT。
func (m *Manager) Issue(u *db.User) (string, error) {
	now := m.now()
	c := claims{
		Username: u.Username,
		Role:     int8(u.Role),
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   strconv.FormatInt(u.Id, 10),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(m.ttl)),
		},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(m.secret)
	if err != nil {
		return "", errs.FromError(err, errs.ECODE_CRYPTO_ERROR, errs.ESTR_CRYPTO_ERROR)
	}
	return token, nil
}

// Authenticate 从请求头里取 Bearer token 并解析出用户。
func (m *Manager) Authenticate(r *http.Request) (*db.User, error) {
	hdr := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "Bearer "
	if hdr == "" {
		return nil, ErrNoToken
	}
	if !strings.HasPrefix(hdr, prefix) {
		return nil, ErrNoToken
	}
	return m.Parse(strings.TrimSpace(hdr[len(prefix):]))
}

// Parse 校验 JWT 并取回用户；用户已被删除时同样返回 ErrBadToken。
func (m *Manager) Parse(token string) (*db.User, error) {
	if token == "" {
		return nil, ErrNoToken
	}
	var c claims
	parsed, err := jwt.ParseWithClaims(token, &c, func(*jwt.Token) (any, error) {
		return m.secret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithTimeFunc(m.now))
	if err != nil || !parsed.Valid {
		return nil, ErrBadToken
	}
	id, err := strconv.ParseInt(c.Subject, 10, 64)
	if err != nil || id <= 0 {
		return nil, ErrBadToken
	}
	u, err := m.GetUser(id)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return nil, ErrBadToken
		}
		return nil, err
	}
	return u, nil
}

// ─────────────────────────────────────────────────────────────
// 内部
// ─────────────────────────────────────────────────────────────

// updateUser 更新一条 user 记录，不存在时返回 ErrUserNotFound。
func (m *Manager) updateUser(id int64, fields map[string]any) error {
	res := m.db.DB.Model(&db.User{}).Where("id = ?", id).Updates(fields)
	if res.Error != nil {
		return errs.DBQuery(res.Error)
	}
	if res.RowsAffected == 0 {
		// 字段值没变化时 RowsAffected 也是 0，这里再确认一次用户是否存在
		if _, err := m.GetUser(id); err != nil {
			return err
		}
	}
	return nil
}

// usernameTaken 判断用户名是否已被占用。
func (m *Manager) usernameTaken(username string) (bool, error) {
	var n int64
	if err := m.db.DB.Model(&db.User{}).Where("username = ?", username).Count(&n).Error; err != nil {
		return false, errs.DBQuery(err)
	}
	return n > 0, nil
}

// checkUsername 校验用户名：非空、不太长、不含空白或控制字符。
func checkUsername(name string) error {
	if name == "" || len(name) > maxUsernameLen {
		return ErrUserBad
	}
	for _, r := range name {
		if r <= ' ' || r == 0x7f {
			return ErrUserBad
		}
	}
	return nil
}
