package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// get 发一个 GET 并返回响应。
func get(t *testing.T, h http.Handler, path string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

// 根路径与"看起来像前端路由"的路径都应当拿到 index.html；index.html 禁缓存。
func TestServesIndexWithFallback(t *testing.T) {
	h := Handler()
	for _, p := range []string{"/", "/index.html", "/files/1", "/admin/shares", "/whatever"} {
		resp := get(t, h, p)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", p, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Fatalf("GET %s 的 Content-Type = %q，期望 HTML", p, ct)
		}
		if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
			t.Fatalf("GET %s 的 Cache-Control = %q，index.html 应当 no-cache", p, cc)
		}
	}
}

// 目录穿越不该读到 dist 之外的东西（embed 本身也只暴露 dist，这里确认不会 panic/200 乱给）。
func TestHandlerRejectsTraversal(t *testing.T) {
	h := Handler()
	resp := get(t, h, "/../webui.go")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("穿越路径 = %d，期望回落到 index.html（200）", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("穿越路径不该拿到非 HTML 内容: %q", ct)
	}
}
