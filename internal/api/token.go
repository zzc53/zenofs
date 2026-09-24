package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
	"github.com/zzc53/zenofs/internal/token"
)

// tokenView 是访问凭证的对外表示：只有元数据。
// 摘要字段（TokenHash / NTHash）永远不出现在响应里。
type tokenView struct {
	Id          int64  `json:"id"`
	UserId      int64  `json:"user_id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"` // secret（SMB/SFTP/WebDAV 通用 token）| public_key（SFTP 公钥）
	Fingerprint string `json:"fingerprint,omitempty"`
	PublicKey   string `json:"public_key,omitempty"`
	ExpiresAt   int64  `json:"expires_at"` // Unix 秒，0 = 永不过期
	CreatedAt   int64  `json:"created_at"`
	LastUsedAt  int64  `json:"last_used_at"`
}

// createdTokenView 是创建 token 的响应：多一个明文 token。
// 明文只在这里出现一次，之后任何接口都查不回来。
type createdTokenView struct {
	tokenView
	Token string `json:"token"`
}

// viewOf 把库里的凭证转成对外表示。
func viewOf(t *db.AccessToken) tokenView {
	kind := "secret"
	if t.Kind == db.TokenPublicKey {
		kind = "public_key"
	}
	return tokenView{
		Id:          t.Id,
		UserId:      t.UserId,
		Name:        t.Name,
		Kind:        kind,
		Fingerprint: t.Fingerprint,
		PublicKey:   t.PublicKey,
		ExpiresAt:   t.ExpiresAt,
		CreatedAt:   t.CreatedAt,
		LastUsedAt:  t.LastUsedAt,
	}
}

// respondTokenErr 把凭证错误映射成响应：不存在 → 404，其余交给统一的错误处理（400）。
func respondTokenErr(w http.ResponseWriter, err error) {
	var ze *errs.ZenoError
	if errors.As(err, &ze) && errors.Is(err, token.ErrNotFound) {
		respondJSON(w, http.StatusNotFound, apiError{
			Code: ze.Code, StrCode: ze.StrCode, Message: ze.Error(),
		})
		return
	}
	respondErr(w, err)
}

// registerTokenRoutes 注册访问凭证相关端点。
//
// 凭证绑定 zenofs 用户（users.id），可带过期时间；一个 token 可以登所有协议
// （SMB / SFTP / WebDAV），公钥只用于 SFTP。
func registerTokenRoutes(r chi.Router, tokens *token.Manager) {
	// POST /api/users/{id}/tokens —— 生成一条随机 token
	// 请求：{"name": "laptop", "expires_at": 0}
	// 响应：{"id":..., "token":"<明文，仅此一次>", ...}
	r.Post("/users/{id}/tokens", func(w http.ResponseWriter, r *http.Request) {
		userId, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		var body struct {
			Name      string `json:"name"`
			ExpiresAt int64  `json:"expires_at"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		if body.ExpiresAt < 0 {
			badRequest(w, "expires_at 不能为负数")
			return
		}
		plain, tok, err := tokens.Create(userId, body.Name, body.ExpiresAt)
		if err != nil {
			respondTokenErr(w, err)
			return
		}
		respondJSON(w, http.StatusCreated, createdTokenView{tokenView: viewOf(tok), Token: plain})
	})

	// POST /api/users/{id}/pubkeys —— 注册一条 SFTP 公钥
	// 请求：{"name": "mac", "public_key": "ssh-ed25519 AAAA...", "expires_at": 0}
	r.Post("/users/{id}/pubkeys", func(w http.ResponseWriter, r *http.Request) {
		userId, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		var body struct {
			Name      string `json:"name"`
			PublicKey string `json:"public_key"`
			ExpiresAt int64  `json:"expires_at"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		if strings.TrimSpace(body.PublicKey) == "" {
			badRequest(w, "public_key 不能为空")
			return
		}
		if body.ExpiresAt < 0 {
			badRequest(w, "expires_at 不能为负数")
			return
		}
		tok, err := tokens.CreatePublicKey(userId, body.Name, body.PublicKey, body.ExpiresAt)
		if err != nil {
			respondTokenErr(w, err)
			return
		}
		respondJSON(w, http.StatusCreated, viewOf(tok))
	})

	// GET /api/users/{id}/tokens?kind=secret|public_key —— 列出凭证（不含摘要）
	r.Get("/users/{id}/tokens", func(w http.ResponseWriter, r *http.Request) {
		userId, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		var kind *db.AccessTokenKind
		switch r.URL.Query().Get("kind") {
		case "":
		case "secret":
			k := db.TokenSecret
			kind = &k
		case "public_key":
			k := db.TokenPublicKey
			kind = &k
		default:
			badRequest(w, "kind 只支持 secret / public_key")
			return
		}
		list, err := tokens.List(userId, kind)
		if err != nil {
			respondTokenErr(w, err)
			return
		}
		views := make([]tokenView, 0, len(list))
		for i := range list {
			views = append(views, viewOf(&list[i]))
		}
		respondJSON(w, http.StatusOK, views)
	})

	// DELETE /api/tokens/{id} —— 吊销凭证（硬删除，立即失效）
	r.Delete("/tokens/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		if err := tokens.Revoke(id); err != nil {
			respondTokenErr(w, err)
			return
		}
		respondJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
	})
}
