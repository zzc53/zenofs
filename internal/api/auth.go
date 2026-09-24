package api

import (
	"net/http"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/otp"
)

// userView 是用户的对外表示：**绝不含** PasswordHash 与 OTPSecret。
type userView struct {
	Id         int64  `json:"id"`
	Username   string `json:"username"`
	Role       string `json:"role"`        // admin / user
	OTPEnabled bool   `json:"otp_enabled"` // 是否已配置二次验证
	CreatedAt  int64  `json:"created_at"`
}

// viewOfUser 把模型转成对外表示。
func viewOfUser(u *db.User) userView {
	role := "user"
	if u.Role == db.UserAdmin {
		role = "admin"
	}
	return userView{
		Id:         u.Id,
		Username:   u.Username,
		Role:       role,
		OTPEnabled: u.OTPSecret != "",
		CreatedAt:  u.CreatedAt,
	}
}

// credentialBody 是创建用户 / bootstrap 的请求体。
//
// otp_secret 是 base32 的 TOTP 密钥（SHA-1 / 30 秒 / 6 位），otp_code 是用它算出的
// 当前六位验证码——两者一起提交可以用来证明密钥录入正确；只给 otp_secret 不给
// otp_code 会被拒。两者都不给时服务端自动生成密钥并只在响应里返回一次。
type credentialBody struct {
	Username  string `json:"username"`
	Password  string `json:"password"`
	OTPSecret string `json:"otp_secret"`
	OTPCode   string `json:"otp_code"`
}

// createdUserView 是创建用户的响应：OTP 密钥只在这里出现一次（服务端生成时）。
type createdUserView struct {
	userView
	OTPSecret string `json:"otp_secret,omitempty"`
	OTPURI    string `json:"otp_uri,omitempty"`
}

// otpURI 给出可直接扫码/手工录入的 otpauth:// 地址；密钥为空时返回空串。
func otpURI(secret, username string) string {
	if secret == "" {
		return ""
	}
	return otp.URI(secret, username, "zenofs")
}

// ─────────────────────────────────────────────────────────────
// 免认证端点
// ─────────────────────────────────────────────────────────────

// handleBootstrap 创建系统的第一个用户，并且固定是管理员。
//
// 只有在 users 表为空时可用（这是唯一的免认证写入口）；之后创建用户必须由管理员
// 带 JWT 调用。仍然要求密码 + OTP 密钥 + 当前六位验证码，避免第一个账号是弱口令。
func (s *server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	var body credentialBody
	if !decodeBody(w, r, &body) {
		return
	}
	u, secret, err := s.auth.Bootstrap(body.Username, body.Password, body.OTPSecret, body.OTPCode)
	if err != nil {
		respondErr(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, createdUserView{
		userView:  viewOfUser(u),
		OTPSecret: secret,
		OTPURI:    otpURI(secret, u.Username),
	})
}

// loginBody 是登录请求：用户名 + 密码 + 六位验证码。
type loginBody struct {
	Username string `json:"username"`
	Password string `json:"password"`
	OTPCode  string `json:"otp_code"`
}

// handleLogin 校验密码与 TOTP，返回 JWT。
func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body loginBody
	if !decodeBody(w, r, &body) {
		return
	}
	token, u, err := s.auth.Login(body.Username, body.Password, body.OTPCode)
	if err != nil {
		respondErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"token":      token,
		"token_type": "Bearer",
		"expires_in": int64(s.auth.TTL().Seconds()),
		"user":       viewOfUser(u),
	})
}

// handleMe 返回当前登录用户。
func (s *server) handleMe(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, viewOfUser(userOf(r)))
}
