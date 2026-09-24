package webdav

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/pool"
	"github.com/zzc53/zenofs/internal/testutil"
	"github.com/zzc53/zenofs/internal/token"
)

// davEnv 是一套跑起来的 WebDAV 服务（挂在 http 测试服务器上，前缀 /dav）。
type davEnv struct {
	env    *testutil.Env
	pm     *pool.PoolManager
	srv    *httptest.Server
	secret string
	userID int64
}

func newDavEnv(t *testing.T) *davEnv {
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
	// 没有授权的 Share：不该出现在该用户的视图里。
	env.NewShare(p.Id, testutil.ShareOpts{Name: "hidden"})

	tokens := token.NewManager(env.DB)
	secret, _, err := tokens.Create(user.Id, "dav", 0)
	if err != nil {
		t.Fatalf("建 token: %v", err)
	}

	srv := httptest.NewServer(New(pm, tokens, DefaultPrefix))
	t.Cleanup(srv.Close)
	return &davEnv{env: env, pm: pm, srv: srv, secret: secret, userID: user.Id}
}

// do 发一个请求；auth 为 true 时带上 Basic（用户名 + token）。
func (e *davEnv) do(t *testing.T, method, path string, body []byte, headers map[string]string, auth bool) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rdr)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if auth {
		req.SetBasicAuth("alice", e.secret)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// bodyOf 读走响应体并关闭。
func bodyOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应体: %v", err)
	}
	return string(data)
}

func TestWebDAVAuthentication(t *testing.T) {
	e := newDavEnv(t)

	// 无凭证 → 401 + WWW-Authenticate
	resp := e.do(t, "PROPFIND", "/dav/", nil, map[string]string{"Depth": "1"}, false)
	bodyOf(t, resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无凭证状态码 = %d, want 401", resp.StatusCode)
	}
	if h := resp.Header.Get("WWW-Authenticate"); !strings.HasPrefix(h, "Basic") {
		t.Fatalf("WWW-Authenticate = %q", h)
	}

	// 错误 token → 401
	req, err := http.NewRequest("PROPFIND", e.srv.URL+"/dav/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("alice", "not-the-token")
	resp, err = e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	bodyOf(t, resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误 token 状态码 = %d, want 401", resp.StatusCode)
	}

	// 正确 token → 207（Multi-Status），根目录里能看到 docs
	resp = e.do(t, "PROPFIND", "/dav/", nil, map[string]string{"Depth": "1"}, true)
	body := bodyOf(t, resp)
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("PROPFIND 状态码 = %d, want 207", resp.StatusCode)
	}
	if !strings.Contains(body, "docs") || strings.Contains(body, "hidden") {
		t.Fatalf("根目录视图不对: %s", body)
	}
}

func TestWebDAVFileRoundTripAndOperations(t *testing.T) {
	e := newDavEnv(t)
	payload := []byte("hello from zenofs over webdav\n")

	// PUT（新建 → 201）
	resp := e.do(t, http.MethodPut, "/dav/docs/hello.txt", payload, nil, true)
	bodyOf(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT 新建状态码 = %d, want 201", resp.StatusCode)
	}

	// GET：内容与 ETag
	resp = e.do(t, http.MethodGet, "/dav/docs/hello.txt", nil, nil, true)
	got := bodyOf(t, resp)
	if resp.StatusCode != http.StatusOK || got != string(payload) {
		t.Fatalf("GET = %d %q", resp.StatusCode, got)
	}
	if resp.Header.Get("ETag") == "" {
		t.Fatalf("GET 响应缺少 ETag")
	}

	// HEAD + PROPFIND（Depth: 1）能看到条目与大小
	resp = e.do(t, "PROPFIND", "/dav/docs", nil, map[string]string{"Depth": "1"}, true)
	body := bodyOf(t, resp)
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("PROPFIND 状态码 = %d, want 207", resp.StatusCode)
	}
	if !strings.Contains(body, "hello.txt") || !strings.Contains(body, "getcontentlength") {
		t.Fatalf("PROPFIND 响应不含预期内容: %s", body)
	}

	// MKCOL
	resp = e.do(t, "MKCOL", "/dav/docs/sub", nil, nil, true)
	bodyOf(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("MKCOL 状态码 = %d, want 201", resp.StatusCode)
	}

	// MOVE（带上 Destination 与 Overwrite 头）
	moved := map[string]string{"Destination": e.srv.URL + "/dav/docs/sub/moved.txt"}
	resp = e.do(t, "MOVE", "/dav/docs/hello.txt", nil, moved, true)
	bodyOf(t, resp)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("MOVE 状态码 = %d, want 201/204", resp.StatusCode)
	}
	resp = e.do(t, http.MethodGet, "/dav/docs/sub/moved.txt", nil, nil, true)
	if got := bodyOf(t, resp); got != string(payload) {
		t.Fatalf("MOVE 后内容 = %q", got)
	}

	// COPY
	copied := map[string]string{"Destination": e.srv.URL + "/dav/docs/sub/copy.txt"}
	resp = e.do(t, "COPY", "/dav/docs/sub/moved.txt", nil, copied, true)
	bodyOf(t, resp)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("COPY 状态码 = %d, want 201/204", resp.StatusCode)
	}
	resp = e.do(t, http.MethodGet, "/dav/docs/sub/copy.txt", nil, nil, true)
	if got := bodyOf(t, resp); got != string(payload) {
		t.Fatalf("COPY 后内容 = %q", got)
	}

	// DELETE（目录递归删除 → 204）
	resp = e.do(t, http.MethodDelete, "/dav/docs/sub", nil, nil, true)
	bodyOf(t, resp)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE 状态码 = %d, want 204", resp.StatusCode)
	}
	resp = e.do(t, "PROPFIND", "/dav/docs", nil, map[string]string{"Depth": "1"}, true)
	if body := bodyOf(t, resp); strings.Contains(body, "sub") {
		t.Fatalf("删除后仍能看到 sub: %s", body)
	}

	// 不存在的资源
	resp = e.do(t, http.MethodGet, "/dav/docs/nope.txt", nil, nil, true)
	bodyOf(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("不存在资源状态码 = %d, want 404", resp.StatusCode)
	}
}

func TestWebDAVLockUnlockAlwaysSucceeds(t *testing.T) {
	e := newDavEnv(t)

	resp := e.do(t, http.MethodPut, "/dav/docs/locked.txt", []byte("x"), nil, true)
	bodyOf(t, resp)

	// LOCK 要带 lockinfo 请求体（空 body 会被当成"刷新锁"并要求 If 头）。
	lockInfo := []byte(`<?xml version="1.0" encoding="utf-8" ?>
<D:lockinfo xmlns:D="DAV:">
  <D:lockscope><D:exclusive/></D:lockscope>
  <D:locktype><D:write/></D:locktype>
  <D:owner><D:href>mailto:alice@example.com</D:href></D:owner>
</D:lockinfo>`)
	resp = e.do(t, "LOCK", "/dav/docs/locked.txt", lockInfo, nil, true)
	body := bodyOf(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("LOCK 状态码 = %d, want 200", resp.StatusCode)
	}
	lockToken := resp.Header.Get("Lock-Token")
	if lockToken == "" || !strings.Contains(lockToken, "opaquelocktoken:") {
		t.Fatalf("LOCK 响应缺少合法 token: %q", lockToken)
	}
	if !strings.Contains(body, "lockdiscovery") {
		t.Fatalf("LOCK 响应体不含 lockdiscovery: %s", body)
	}

	// 别人拿着别的凭证也能写（服务端不做互斥）
	req, err := http.NewRequest(http.MethodPut, e.srv.URL+"/dav/docs/locked.txt", strings.NewReader("y"))
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("alice", e.secret)
	req.Header.Set("If", "("+lockToken+")")
	resp2, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	bodyOf(t, resp2)
	if resp2.StatusCode >= 400 {
		t.Fatalf("带锁写入被拒: %d", resp2.StatusCode)
	}

	// UNLOCK → 204
	resp = e.do(t, "UNLOCK", "/dav/docs/locked.txt", nil,
		map[string]string{"Lock-Token": lockToken}, true)
	bodyOf(t, resp)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("UNLOCK 状态码 = %d, want 204", resp.StatusCode)
	}
	// 未知 token 的 UNLOCK 也成功（vfs 不维护锁状态）
	resp = e.do(t, "UNLOCK", "/dav/docs/locked.txt", nil,
		map[string]string{"Lock-Token": "<opaquelocktoken:0000>"}, true)
	bodyOf(t, resp)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("未知 token UNLOCK 状态码 = %d, want 204", resp.StatusCode)
	}
}

func TestWebDAVISolationAndPathTraversal(t *testing.T) {
	e := newDavEnv(t)

	// 未授权的 Share 看不见（RootFS 只列 share_users 里有记录的）
	resp := e.do(t, "PROPFIND", "/dav/hidden", nil, map[string]string{"Depth": "1"}, true)
	bodyOf(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("未授权 Share 状态码 = %d, want 404", resp.StatusCode)
	}
	// 根不允许被删除
	resp = e.do(t, http.MethodDelete, "/dav/", nil, nil, true)
	bodyOf(t, resp)
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("删除根状态码 = %d, want 403/405", resp.StatusCode)
	}
	// 路径穿越（%2e%2e 解码后是 ".."）必须被拒
	resp = e.do(t, http.MethodGet, "/dav/docs/%2e%2e/%2e%2e/etc/passwd", nil, nil, true)
	bodyOf(t, resp)
	if resp.StatusCode < 400 {
		t.Fatalf("路径穿越状态码 = %d，应当被拒", resp.StatusCode)
	}
}

func TestWebDAVReadOnlyShareRejectsWrite(t *testing.T) {
	e := newDavEnv(t)
	if err := e.env.DB.DB.Model(&db.ShareUser{}).
		Where("user_id = ?", e.userID).
		Update("permission", db.ShareRead).Error; err != nil {
		t.Fatalf("改权限: %v", err)
	}

	// 读没问题
	resp := e.do(t, "PROPFIND", "/dav/docs", nil, map[string]string{"Depth": "1"}, true)
	bodyOf(t, resp)
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("只读 Share PROPFIND 状态码 = %d, want 207", resp.StatusCode)
	}
	// 写被拒
	resp = e.do(t, http.MethodPut, "/dav/docs/nope.txt", []byte("x"), nil, true)
	bodyOf(t, resp)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("只读 Share PUT 状态码 = %d, want 403", resp.StatusCode)
	}
	resp = e.do(t, "MKCOL", "/dav/docs/nope", nil, nil, true)
	bodyOf(t, resp)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("只读 Share MKCOL 状态码 = %d, want 403", resp.StatusCode)
	}
}
