package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
	"github.com/zzc53/zenofs/internal/token"
)

// decodeArray 把响应体解析成数组；同时关闭 body。
func decodeArray(t *testing.T, resp *http.Response) []map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var out []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("解析响应 JSON: %v", err)
	}
	return out
}

func TestTokenLifecycleEndpoints(t *testing.T) {
	env, srv := newTestServer(t)
	user := db.User{Username: "alice", PasswordHash: "x"}
	if err := env.DB.DB.Create(&user).Error; err != nil {
		t.Fatalf("建用户: %v", err)
	}
	tokens := token.NewManager(env.DB)

	// ── 创建 token ──
	resp := do(t, http.MethodPost, srv.URL+fmt.Sprintf("/api/users/%d/tokens", user.Id),
		[]byte(`{"name":"laptop","expires_at":0}`))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("创建 token 状态码 = %d, want 201", resp.StatusCode)
	}
	created := decode(t, resp)
	plain, _ := created["token"].(string)
	if plain == "" {
		t.Fatalf("创建响应没有返回明文 token: %v", created)
	}
	if created["kind"] != "secret" || created["name"] != "laptop" {
		t.Fatalf("创建响应 = %v", created)
	}
	tokenID := int64(created["id"].(float64))

	// 库里只有摘要，且能被 token 包验证
	var row db.AccessToken
	if err := env.DB.DB.First(&row, tokenID).Error; err != nil {
		t.Fatalf("读库: %v", err)
	}
	if string(row.TokenHash) != string(token.Sum(plain)) || len(row.NTHash) != 16 {
		t.Fatalf("库里没有正确的单向摘要: %+v", row)
	}
	if _, err := tokens.AuthenticateSecret("alice", plain); err != nil {
		t.Fatalf("刚创建的 token 无法认证: %v", err)
	}

	// ── 注册公钥 ──
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥: %v", err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("NewPublicKey: %v", err)
	}
	body, err := json.Marshal(map[string]any{
		"name": "mac", "public_key": string(ssh.MarshalAuthorizedKey(key)),
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	resp = do(t, http.MethodPost, srv.URL+fmt.Sprintf("/api/users/%d/pubkeys", user.Id), body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("注册公钥状态码 = %d, want 201", resp.StatusCode)
	}
	pubView := decode(t, resp)
	if pubView["kind"] != "public_key" || pubView["fingerprint"] != ssh.FingerprintSHA256(key) {
		t.Fatalf("注册公钥响应 = %v", pubView)
	}
	if _, ok := pubView["token"]; ok {
		t.Fatalf("公钥注册响应不该有 token 字段")
	}
	if pubView["public_key"] == nil {
		t.Fatalf("公钥响应应包含公钥本体: %v", pubView)
	}

	// ── 列表 ──
	resp = do(t, http.MethodGet, srv.URL+fmt.Sprintf("/api/users/%d/tokens", user.Id), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("列表状态码 = %d, want 200", resp.StatusCode)
	}
	list := decodeArray(t, resp)
	if len(list) != 2 {
		t.Fatalf("列表 = %v，期望 2 条", list)
	}
	for _, item := range list {
		if _, ok := item["token"]; ok {
			t.Fatalf("列表里泄露了明文 token: %v", item)
		}
		if _, ok := item["token_hash"]; ok {
			t.Fatalf("列表里泄露了摘要: %v", item)
		}
	}

	// 按 kind 过滤
	resp = do(t, http.MethodGet, srv.URL+fmt.Sprintf("/api/users/%d/tokens?kind=public_key", user.Id), nil)
	if got := decodeArray(t, resp); len(got) != 1 || got[0]["kind"] != "public_key" {
		t.Fatalf("kind 过滤 = %v", got)
	}
	if resp = do(t, http.MethodGet, srv.URL+fmt.Sprintf("/api/users/%d/tokens?kind=bogus", user.Id), nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法 kind 状态码 = %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// ── 吊销 ──
	resp = do(t, http.MethodDelete, srv.URL+fmt.Sprintf("/api/tokens/%d", tokenID), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("吊销状态码 = %d, want 200", resp.StatusCode)
	}
	if body := decode(t, resp); body["status"] != "revoked" {
		t.Fatalf("吊销响应 = %v", body)
	}
	if _, err := tokens.AuthenticateSecret("alice", plain); err == nil {
		t.Fatalf("吊销后仍能认证")
	}
	if resp = do(t, http.MethodDelete, srv.URL+fmt.Sprintf("/api/tokens/%d", tokenID), nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("重复吊销状态码 = %d, want 404", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

func TestTokenEndpointValidation(t *testing.T) {
	env, srv := newTestServer(t)
	user := db.User{Username: "alice", PasswordHash: "x"}
	if err := env.DB.DB.Create(&user).Error; err != nil {
		t.Fatalf("建用户: %v", err)
	}
	url := srv.URL + fmt.Sprintf("/api/users/%d/tokens", user.Id)

	// 归到不存在的用户 → 400 + TOKEN_BAD_USER
	if resp := do(t, http.MethodPost, srv.URL+"/api/users/9999/tokens", []byte(`{"name":"x"}`)); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("未知用户状态码 = %d, want 400", resp.StatusCode)
	} else if body := decode(t, resp); body["str_code"] != errs.ESTR_TOKEN_BAD_USER {
		t.Fatalf("未知用户响应 = %v", body)
	}
	// 非法 JSON
	if resp := do(t, http.MethodPost, url, []byte("{not json")); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法 JSON 状态码 = %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// 负数过期时间
	if resp := do(t, http.MethodPost, url, []byte(`{"expires_at":-1}`)); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("负数过期时间状态码 = %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// 非法公钥
	if resp := do(t, http.MethodPost, srv.URL+fmt.Sprintf("/api/users/%d/pubkeys", user.Id),
		[]byte(`{"public_key":"not-a-key"}`)); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法公钥状态码 = %d, want 400", resp.StatusCode)
	} else if body := decode(t, resp); body["str_code"] != errs.ESTR_TOKEN_BAD_KEY {
		t.Fatalf("非法公钥响应 = %v", body)
	}
	// 空公钥
	if resp := do(t, http.MethodPost, srv.URL+fmt.Sprintf("/api/users/%d/pubkeys", user.Id),
		[]byte(`{}`)); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("空公钥状态码 = %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// 非法 id
	if resp := do(t, http.MethodGet, srv.URL+"/api/users/abc/tokens", nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法 id 状态码 = %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

// 过期时间应当原样落库并影响认证。
func TestTokenExpiryThroughAPI(t *testing.T) {
	env, srv := newTestServer(t)
	user := db.User{Username: "alice", PasswordHash: "x"}
	if err := env.DB.DB.Create(&user).Error; err != nil {
		t.Fatalf("建用户: %v", err)
	}

	resp := do(t, http.MethodPost, srv.URL+fmt.Sprintf("/api/users/%d/tokens", user.Id),
		[]byte(`{"name":"temp","expires_at":1}`))
	created := decode(t, resp)
	plain := created["token"].(string)

	tokens := token.NewManager(env.DB)
	if _, err := tokens.AuthenticateSecret("alice", plain); err == nil {
		t.Fatalf("已过期的 token 仍能认证")
	}
}
