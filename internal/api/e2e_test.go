package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zzc53/zenofs/internal/auth"
	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/otp"
	"github.com/zzc53/zenofs/internal/pool"
	"github.com/zzc53/zenofs/internal/testutil"
	"github.com/zzc53/zenofs/internal/token"
)

// doAs 用指定的 JWT 发请求（token 为空表示不带认证头）。
func doAs(t *testing.T, jwt, method, url string, body []byte) *http.Response {
	t.Helper()
	req := newRequest(t, method, url, body)
	if jwt != "" {
		req.Header.Set("Authorization", "Bearer "+jwt)
	}
	return send(t, req)
}

// newUserAndLogin 造一个普通用户并登录，返回 id 与 JWT。
func newUserAndLogin(t *testing.T, env *testutil.Env, authMgr *auth.Manager, username string) (int64, string) {
	t.Helper()
	secret, err := otp.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	code, err := otp.Code(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	u, _, err := authMgr.CreateUser(username, "pw-123456", secret, code, db.UserNormal)
	if err != nil {
		t.Fatalf("CreateUser(%s): %v", username, err)
	}
	jwt, _, err := authMgr.Login(username, "pw-123456", code)
	if err != nil {
		t.Fatalf("Login(%s): %v", username, err)
	}
	return u.Id, jwt
}

// newBareServer 起一个"还没有任何用户"的服务，用来测 bootstrap 首次创建。
func newBareServer(t *testing.T) (*testutil.Env, *httptest.Server, *auth.Manager) {
	t.Helper()
	env := testutil.New(t)
	pm := pool.New(env.DB, []pool.ChunkHandler{pool.NewLocalChunkHandler()})
	authMgr, err := auth.NewManager(env.DB)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewRouter(Options{
		PoolManager: pm,
		Tokens:      token.NewManager(env.DB),
		Auth:        authMgr,
	}))
	t.Cleanup(srv.Close)
	return env, srv, authMgr
}

// otpFields 造一组 base32 密钥 + 当前验证码。
func otpFields(t *testing.T) (string, string) {
	t.Helper()
	secret, err := otp.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	code, err := otp.Code(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return secret, code
}

func TestAuthBootstrapAndLogin(t *testing.T) {
	_, srv, _ := newBareServer(t)
	secret, code := otpFields(t)

	// 没有任何用户时，管理端点也要先登录——用已注册的端点测未认证的返回
	resp := doNoAuth(t, http.MethodPost, srv.URL+"/api/pools", []byte(`{"name":"p","chunk_size_kb":16}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未登录访问 /api/pools = %d, want 401", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("401 响应缺少 WWW-Authenticate")
	}

	// bootstrap：免认证创建第一个管理员
	body, err := json.Marshal(map[string]any{
		"username": "root", "password": "root-pw-123", "otp_secret": secret, "otp_code": code,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp = doNoAuth(t, http.MethodPost, srv.URL+"/api/auth/bootstrap", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("bootstrap = %d, want 201", resp.StatusCode)
	}
	created := decode(t, resp)
	if created["role"] != "admin" || created["username"] != "root" {
		t.Fatalf("bootstrap 响应 = %v", created)
	}

	// 第二次 bootstrap 必须失败（系统里已经有用户了）
	if resp := doNoAuth(t, http.MethodPost, srv.URL+"/api/auth/bootstrap", body); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("重复 bootstrap = %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// 登录：密码错 / 验证码错 / 正确
	login := func(pw, otpCode string) *http.Response {
		b, err := json.Marshal(map[string]any{"username": "root", "password": pw, "otp_code": otpCode})
		if err != nil {
			t.Fatal(err)
		}
		return doNoAuth(t, http.MethodPost, srv.URL+"/api/auth/login", b)
	}
	if resp := login("wrong-pw", code); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误密码登录 = %d, want 401", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := login("root-pw-123", "000000"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误验证码登录 = %d, want 401", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	resp = login("root-pw-123", code)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("登录 = %d, want 200", resp.StatusCode)
	}
	loginBody := decode(t, resp)
	jwt, _ := loginBody["token"].(string)
	if jwt == "" || loginBody["token_type"] != "Bearer" {
		t.Fatalf("登录响应 = %v", loginBody)
	}

	// 用 JWT 访问受保护端点
	resp = doAs(t, jwt, http.MethodGet, srv.URL+"/api/auth/me", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/auth/me = %d, want 200", resp.StatusCode)
	}
	if me := decode(t, resp); me["username"] != "root" || me["role"] != "admin" {
		t.Fatalf("/api/auth/me = %v", me)
	}
	// 伪造/无效 JWT
	if resp := doAs(t, jwt+"x", http.MethodGet, srv.URL+"/api/auth/me", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无效 JWT = %d, want 401", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

func TestUserManagementAndAdminOnly(t *testing.T) {
	env, srv := newTestServer(t)
	authMgr, err := auth.NewManager(env.DB)
	if err != nil {
		t.Fatal(err)
	}

	// 管理员创建普通用户：不给 otp_secret，服务端生成并只返回一次
	body, err := json.Marshal(map[string]any{"username": "bob", "password": "bob-pw-123"})
	if err != nil {
		t.Fatal(err)
	}
	resp := do(t, http.MethodPost, srv.URL+"/api/users", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("创建用户 = %d, want 201", resp.StatusCode)
	}
	created := decode(t, resp)
	bobSecret, _ := created["otp_secret"].(string)
	bobID := int64(created["id"].(float64))
	if bobSecret == "" || created["otp_uri"] == "" {
		t.Fatalf("服务端生成密钥时应返回明文与 URI: %v", created)
	}
	if _, ok := created["password_hash"]; ok {
		t.Fatalf("响应泄露了密码哈希: %v", created)
	}

	// 用返回的密钥登录
	code, err := otp.Code(bobSecret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	loginBody, err := json.Marshal(map[string]any{"username": "bob", "password": "bob-pw-123", "otp_code": code})
	if err != nil {
		t.Fatal(err)
	}
	resp = doNoAuth(t, http.MethodPost, srv.URL+"/api/auth/login", loginBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bob 登录 = %d, want 200", resp.StatusCode)
	}
	bobJWT := decode(t, resp)["token"].(string)

	// 普通用户不能列用户、不能创建用户
	if resp := doAs(t, bobJWT, http.MethodGet, srv.URL+"/api/users", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("普通用户列用户 = %d, want 403", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := doAs(t, bobJWT, http.MethodPost, srv.URL+"/api/users", body); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("普通用户建用户 = %d, want 403", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// 但可以看自己
	if resp := doAs(t, bobJWT, http.MethodGet, fmt.Sprintf("%s/api/users/%d", srv.URL, bobID), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("普通用户看自己 = %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// 管理员列出用户（应含 root 与 bob）
	resp = do(t, http.MethodGet, srv.URL+"/api/users", nil)
	users := decodeArray(t, resp)
	if len(users) != 2 {
		t.Fatalf("用户列表 = %v", users)
	}

	// 管理员改 bob 的角色
	roleBody, err := json.Marshal(map[string]any{"role": "admin"})
	if err != nil {
		t.Fatal(err)
	}
	resp = do(t, http.MethodPut, fmt.Sprintf("%s/api/users/%d", srv.URL, bobID), roleBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("改角色 = %d, want 200", resp.StatusCode)
	}
	if u := decode(t, resp); u["role"] != "admin" {
		t.Fatalf("改角色响应 = %v", u)
	}

	// 管理员不能删自己
	meResp := do(t, http.MethodGet, srv.URL+"/api/auth/me", nil)
	adminID := int64(decode(t, meResp)["id"].(float64))
	if resp := do(t, http.MethodDelete, fmt.Sprintf("%s/api/users/%d", srv.URL, adminID), nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("管理员删自己 = %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// 删除 bob 后他的 token 立即失效
	if resp := do(t, http.MethodDelete, fmt.Sprintf("%s/api/users/%d", srv.URL, bobID), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("删除用户 = %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := doAs(t, bobJWT, http.MethodGet, srv.URL+"/api/auth/me", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("已删用户的 token = %d, want 401", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	_ = authMgr
}

func TestShareAndFileLifecycle(t *testing.T) {
	env, srv := newTestServer(t)
	authMgr, err := auth.NewManager(env.DB)
	if err != nil {
		t.Fatal(err)
	}

	// 建池 + 两块盘（1 data + 1 parity），否则写 chunk 会失败
	resp := do(t, http.MethodPost, srv.URL+"/api/pools", []byte(`{"name":"p","chunk_size_kb":64}`))
	poolID := int64(decode(t, resp)["Id"].(float64))
	for _, d := range []struct {
		path   string
		parity bool
	}{
		{env.Path("d0"), false},
		{env.Path("d1"), true},
	} {
		b, err := json.Marshal(map[string]any{"path": d.path, "add_parity": d.parity})
		if err != nil {
			t.Fatal(err)
		}
		if resp := do(t, http.MethodPost, fmt.Sprintf("%s/api/pools/%d/disks", srv.URL, poolID), b); resp.StatusCode != http.StatusCreated {
			t.Fatalf("加盘 = %d, want 201", resp.StatusCode)
		} else {
			resp.Body.Close()
		}
	}

	// 建 Share
	shareBody, err := json.Marshal(map[string]any{"name": "docs", "pool_id": poolID, "quota_mb": 1024})
	if err != nil {
		t.Fatal(err)
	}
	resp = do(t, http.MethodPost, srv.URL+"/api/shares", shareBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("建 Share = %d, want 201", resp.StatusCode)
	}
	share := decode(t, resp)
	shareID := int64(share["id"].(float64))
	if share["name"] != "docs" || share["permission"] != "admin" {
		t.Fatalf("建 Share 响应 = %v", share)
	}
	// 重名 → 409
	if resp := do(t, http.MethodPost, srv.URL+"/api/shares", shareBody); resp.StatusCode != http.StatusConflict {
		t.Fatalf("重名 Share = %d, want 409", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// 授权 bob 写权限
	bobID, bobJWT := newUserAndLogin(t, env, authMgr, "bob")
	grantBody, err := json.Marshal(map[string]any{"user_id": bobID, "permission": "write"})
	if err != nil {
		t.Fatal(err)
	}
	resp = do(t, http.MethodPost, fmt.Sprintf("%s/api/shares/%d/users", srv.URL, shareID), grantBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("授权 = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// 管理员上传 → 下载 → 列表 → stat
	payload := []byte("hello zenofs api")
	fileURL := fmt.Sprintf("%s/api/shares/%d/files?path=/hello.txt", srv.URL, shareID)
	if resp := do(t, http.MethodPut, fileURL, payload); resp.StatusCode != http.StatusCreated {
		t.Fatalf("上传 = %d, want 201", resp.StatusCode)
	} else if view := decode(t, resp); view["size"] != float64(len(payload)) {
		t.Fatalf("上传响应 = %v", view)
	}
	resp = do(t, http.MethodGet, fileURL, nil)
	if got := readAll(t, resp); string(got) != string(payload) {
		t.Fatalf("下载内容 = %q", got)
	}
	resp = do(t, http.MethodGet, fmt.Sprintf("%s/api/shares/%d/list?path=/", srv.URL, shareID), nil)
	listBody := decode(t, resp)
	entries, _ := listBody["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("列表 = %v", listBody)
	}
	if resp := do(t, http.MethodGet, fmt.Sprintf("%s/api/shares/%d/stat?path=/hello.txt", srv.URL, shareID), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("stat = %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// bob（write 权限）能建目录、改名
	dirBody, err := json.Marshal(map[string]any{"path": "/sub"})
	if err != nil {
		t.Fatal(err)
	}
	if resp := doAs(t, bobJWT, http.MethodPost, fmt.Sprintf("%s/api/shares/%d/folders", srv.URL, shareID), dirBody); resp.StatusCode != http.StatusCreated {
		t.Fatalf("bob 建目录 = %d, want 201", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	renameBody, err := json.Marshal(map[string]any{"from": "/hello.txt", "to": "/sub/moved.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if resp := doAs(t, bobJWT, http.MethodPost, fmt.Sprintf("%s/api/shares/%d/rename", srv.URL, shareID), renameBody); resp.StatusCode != http.StatusOK {
		t.Fatalf("bob 改名 = %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// bob 删除 → 回收站 → 恢复 → 彻底删除
	delURL := fmt.Sprintf("%s/api/shares/%d/files?path=/sub/moved.txt", srv.URL, shareID)
	if resp := doAs(t, bobJWT, http.MethodDelete, delURL, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("bob 删除 = %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	resp = doAs(t, bobJWT, http.MethodGet, fmt.Sprintf("%s/api/shares/%d/recycle", srv.URL, shareID), nil)
	deleted := decodeArray(t, resp)
	if len(deleted) != 1 || deleted[0]["name"] != "moved.txt" || deleted[0]["kind"] != "file" {
		t.Fatalf("回收站 = %v", deleted)
	}
	inodeID := int64(deleted[0]["id"].(float64))

	restoreURL := fmt.Sprintf("%s/api/shares/%d/recycle/%d/restore", srv.URL, shareID, inodeID)
	if resp := doAs(t, bobJWT, http.MethodPost, restoreURL, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("恢复 = %d, want 200", resp.StatusCode)
	} else if view := decode(t, resp); view["path"] != "/sub/moved.txt" {
		t.Fatalf("恢复响应 = %v", view)
	}

	if resp := doAs(t, bobJWT, http.MethodDelete, delURL, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("再次删除 = %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	purgeURL := fmt.Sprintf("%s/api/shares/%d/recycle/%d", srv.URL, shareID, inodeID)
	if resp := doAs(t, bobJWT, http.MethodDelete, purgeURL, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("彻底删除 = %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := doAs(t, bobJWT, http.MethodGet, fmt.Sprintf("%s/api/shares/%d/recycle", srv.URL, shareID), nil); len(decodeArray(t, resp)) != 0 {
		t.Fatalf("彻底删除后回收站应当为空")
	}

	// 撤销授权后 bob 再也看不到这个 Share（按不存在处理）
	if resp := do(t, http.MethodDelete, fmt.Sprintf("%s/api/shares/%d/users/%d", srv.URL, shareID, bobID), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("撤权 = %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := doAs(t, bobJWT, http.MethodGet, fmt.Sprintf("%s/api/shares/%d/list?path=/", srv.URL, shareID), nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("撤权后访问 = %d, want 404", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// 管理员删除 Share（里面有文件，需要 force）
	if resp := do(t, http.MethodDelete, fmt.Sprintf("%s/api/shares/%d", srv.URL, shareID), nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("删除非空 Share = %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := do(t, http.MethodDelete, fmt.Sprintf("%s/api/shares/%d?force=1", srv.URL, shareID), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("强制删除 Share = %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

func TestReadOnlyShareRejectsWrite(t *testing.T) {
	env, srv := newTestServer(t)
	authMgr, err := auth.NewManager(env.DB)
	if err != nil {
		t.Fatal(err)
	}

	// 建池 + 盘 + Share（管理员是创建者，自带 admin 权限）
	resp := do(t, http.MethodPost, srv.URL+"/api/pools", []byte(`{"name":"p","chunk_size_kb":64}`))
	poolID := int64(decode(t, resp)["Id"].(float64))
	diskBody, err := json.Marshal(map[string]any{"path": env.Path("d0")})
	if err != nil {
		t.Fatal(err)
	}
	resp = do(t, http.MethodPost, fmt.Sprintf("%s/api/pools/%d/disks", srv.URL, poolID), diskBody)
	resp.Body.Close()
	parityBody, err := json.Marshal(map[string]any{"path": env.Path("d1"), "add_parity": true})
	if err != nil {
		t.Fatal(err)
	}
	resp = do(t, http.MethodPost, fmt.Sprintf("%s/api/pools/%d/disks", srv.URL, poolID), parityBody)
	resp.Body.Close()

	shareBody, err := json.Marshal(map[string]any{"name": "ro", "pool_id": poolID})
	if err != nil {
		t.Fatal(err)
	}
	resp = do(t, http.MethodPost, srv.URL+"/api/shares", shareBody)
	shareID := int64(decode(t, resp)["id"].(float64))

	// carol 只有读权限
	carolID, carolJWT := newUserAndLogin(t, env, authMgr, "carol")
	grantBody, err := json.Marshal(map[string]any{"user_id": carolID, "permission": "read"})
	if err != nil {
		t.Fatal(err)
	}
	resp = do(t, http.MethodPost, fmt.Sprintf("%s/api/shares/%d/users", srv.URL, shareID), grantBody)
	resp.Body.Close()

	// 读没问题
	if resp := doAs(t, carolJWT, http.MethodGet, fmt.Sprintf("%s/api/shares/%d/list?path=/", srv.URL, shareID), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("只读用户列目录 = %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// 写被拒（403）
	put := func(path string) *http.Response {
		return doAs(t, carolJWT, http.MethodPut, fmt.Sprintf("%s/api/shares/%d/files?path=%s", srv.URL, shareID, path), []byte("x"))
	}
	if resp := put("/nope.txt"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("只读用户上传 = %d, want 403", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	dirBody, err := json.Marshal(map[string]any{"path": "/nope"})
	if err != nil {
		t.Fatal(err)
	}
	if resp := doAs(t, carolJWT, http.MethodPost, fmt.Sprintf("%s/api/shares/%d/folders", srv.URL, shareID), dirBody); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("只读用户建目录 = %d, want 403", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := doAs(t, carolJWT, http.MethodDelete, fmt.Sprintf("%s/api/shares/%d/files?path=/x", srv.URL, shareID), nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("只读用户删除 = %d, want 403", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// 未授权用户访问别人的 Share → 404（不泄露 Share 是否存在）
	daveID, daveJWT := newUserAndLogin(t, env, authMgr, "dave")
	if daveID == carolID {
		t.Fatal("用户 id 应当不同")
	}
	if resp := doAs(t, daveJWT, http.MethodGet, fmt.Sprintf("%s/api/shares/%d/list?path=/", srv.URL, shareID), nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("未授权用户访问 = %d, want 404", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}
