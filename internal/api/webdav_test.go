package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zzc53/zenofs/internal/pool"
	"github.com/zzc53/zenofs/internal/testutil"
	"github.com/zzc53/zenofs/internal/token"
)

// WebDAV 必须和 REST API 挂在同一个 HTTP 服务上（同一端口）：
// /api 照常，/dav 走 WebDAV 处理器（未认证 → 401）。
func TestWebDAVMountedOnSameRouter(t *testing.T) {
	_, srv := newTestServer(t)

	resp := do(t, http.MethodGet, srv.URL+"/api/pools/1", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("/api 状态码 = %d, want 400（池不存在）", resp.StatusCode)
	}

	resp = do(t, "PROPFIND", srv.URL+"/dav/", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/dav 未认证状态码 = %d, want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got == "" {
		t.Fatalf("401 响应缺少 WWW-Authenticate")
	}
}

// WEBDAV_PREFIX=off 时不挂载 /dav，/api 不受影响。
func TestWebDAVDisabledByOption(t *testing.T) {
	env := testutil.New(t)
	pm := pool.New(env.DB, []pool.ChunkHandler{pool.NewLocalChunkHandler()})
	srv := httptest.NewServer(NewRouter(Options{PoolManager: pm, Tokens: token.NewManager(env.DB), WebDAVPrefix: "off"}))
	t.Cleanup(srv.Close)

	resp := do(t, "PROPFIND", srv.URL+"/dav/", nil)
	resp.Body.Close()
	// chi 对"未知方法 + 未注册路径"可能回 405 而不是 404；两者都说明
	// 这里没有 WebDAV 处理器（装了会是 401）。
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("关闭后 /dav 状态码 = %d, want 404/405", resp.StatusCode)
	}

	resp = do(t, http.MethodGet, srv.URL+"/api/pools/1", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("/api 状态码 = %d, want 400", resp.StatusCode)
	}
}

// 自定义前缀（不以 / 开头会自动补上）。
func TestWebDAVCustomPrefix(t *testing.T) {
	env := testutil.New(t)
	pm := pool.New(env.DB, []pool.ChunkHandler{pool.NewLocalChunkHandler()})
	srv := httptest.NewServer(NewRouter(Options{PoolManager: pm, Tokens: token.NewManager(env.DB), WebDAVPrefix: "files"}))
	t.Cleanup(srv.Close)

	resp := do(t, "PROPFIND", srv.URL+"/files/", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("自定义前缀 /files 状态码 = %d, want 401", resp.StatusCode)
	}
	resp = do(t, "PROPFIND", srv.URL+"/dav/", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("默认前缀应当未挂载，状态码 = %d, want 404/405", resp.StatusCode)
	}
}
