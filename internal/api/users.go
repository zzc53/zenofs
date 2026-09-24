package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/zzc53/zenofs/internal/auth"
	"github.com/zzc53/zenofs/internal/db"
)

// registerUserRoutes 注册用户管理端点。
//
// 权限约定：列出/创建/删除用户仅管理员；查询与修改自己谁都可以（改角色、重置
// 别人的 OTP 仍限管理员）。
func (s *server) registerUserRoutes(r chi.Router) {
	// GET /api/users —— 列出用户（管理员）
	r.Get("/users", func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r) {
			return
		}
		users, err := s.auth.ListUsers()
		if err != nil {
			respondErr(w, err)
			return
		}
		views := make([]userView, 0, len(users))
		for i := range users {
			views = append(views, viewOfUser(&users[i]))
		}
		respondJSON(w, http.StatusOK, views)
	})

	// POST /api/users —— 创建用户（管理员）
	// 请求：{"username":"bob","password":"...","otp_secret":"...","otp_code":"123456","role":"user"}
	r.Post("/users", func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r) {
			return
		}
		var body struct {
			credentialBody
			Role string `json:"role"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		role, ok := parseRole(w, body.Role)
		if !ok {
			return
		}
		u, secret, err := s.auth.CreateUser(body.Username, body.Password, body.OTPSecret, body.OTPCode, role)
		if err != nil {
			respondErr(w, err)
			return
		}
		respondJSON(w, http.StatusCreated, createdUserView{
			userView:  viewOfUser(u),
			OTPSecret: secret,
			OTPURI:    otpURI(secret, u.Username),
		})
	})

	// GET /api/users/{id} —— 查询用户（管理员或本人）
	r.Get("/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		if !s.selfOrAdmin(w, r, id) {
			return
		}
		u, err := s.auth.GetUser(id)
		if err != nil {
			respondErr(w, err)
			return
		}
		respondJSON(w, http.StatusOK, viewOfUser(u))
	})

	// PUT /api/users/{id} —— 改密码 / 角色 / 重置 OTP
	// 请求：{"password":"...","role":"admin","reset_otp":true,"otp_secret":"...","otp_code":"123456"}
	r.Put("/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		if !s.selfOrAdmin(w, r, id) {
			return
		}
		var body struct {
			Password  string `json:"password"`
			Role      string `json:"role"`
			ResetOTP  bool   `json:"reset_otp"`
			OTPSecret string `json:"otp_secret"`
			OTPCode   string `json:"otp_code"`
		}
		if !decodeBody(w, r, &body) {
			return
		}

		current := userOf(r)
		isAdmin := current.Role == db.UserAdmin
		var secret string

		if body.Password != "" {
			if err := s.auth.SetPassword(id, body.Password); err != nil {
				respondErr(w, err)
				return
			}
		}
		if body.Role != "" {
			if !isAdmin {
				respondErr(w, auth.ErrNotAdmin)
				return
			}
			role, ok := parseRole(w, body.Role)
			if !ok {
				return
			}
			if err := s.auth.SetRole(id, role); err != nil {
				respondErr(w, err)
				return
			}
		}
		if body.ResetOTP {
			_, generated, err := s.auth.ResetOTP(id, body.OTPSecret, body.OTPCode)
			if err != nil {
				respondErr(w, err)
				return
			}
			secret = generated
		}

		u, err := s.auth.GetUser(id)
		if err != nil {
			respondErr(w, err)
			return
		}
		respondJSON(w, http.StatusOK, createdUserView{
			userView:  viewOfUser(u),
			OTPSecret: secret,
			OTPURI:    otpURI(secret, u.Username),
		})
	})

	// DELETE /api/users/{id} —— 删除用户（管理员，且不能删自己）
	r.Delete("/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r) {
			return
		}
		id, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		if u := userOf(r); u != nil && u.Id == id {
			badRequest(w, "不能删除当前登录的管理员自己")
			return
		}
		if err := s.auth.DeleteUser(id); err != nil {
			respondErr(w, err)
			return
		}
		respondJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	})
}

// selfOrAdmin 检查"是本人或管理员"，否则写 403 并返回 false。
func (s *server) selfOrAdmin(w http.ResponseWriter, r *http.Request, targetId int64) bool {
	u := userOf(r)
	if u == nil {
		respondErr(w, auth.ErrNoToken)
		return false
	}
	if u.Id == targetId || u.Role == db.UserAdmin {
		return true
	}
	respondErr(w, auth.ErrNotAdmin)
	return false
}

// parseRole 解析角色名；空表示普通用户。
func parseRole(w http.ResponseWriter, name string) (db.UserRole, bool) {
	switch name {
	case "", "user":
		return db.UserNormal, true
	case "admin":
		return db.UserAdmin, true
	}
	badRequest(w, "role 只支持 user / admin")
	return db.UserNormal, false
}
