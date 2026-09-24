// Package webui 把构建好的前端嵌进二进制，并提供静态资源服务。
//
// 前端源码在仓库的 web/ 目录（Vite + Preact + TypeScript），构建产物输出到本包的
// dist/ 子目录，跟着仓库一起提交；`go build` 时整个 dist 会被 embed 进来，
// 所以部署仍然只有一个二进制。
//
// 前端挂在 HTTP 服务的根路径上，与 /api、/dav 共存（见 internal/api 的 dispatch）：
// 未知路径一律回 index.html，前端自己用 hash 路由（#/files/1）决定显示什么。
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// assets 是 Vite 的构建产物。
//
//go:embed all:dist
var assets embed.FS

// Handler 返回前端的 http.Handler。
func Handler() http.Handler {
	sub, err := fs.Sub(assets, "dist")
	if err != nil {
		// 只有 embed 的目录结构被改坏时才可能走到这里
		panic("webui: 嵌入的 dist 目录不可读: " + err.Error())
	}
	files := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" || name == "." || name == "index.html" {
			// 注意 index.html 也要直接给出去：http.FileServer 会把 /index.html
			// 301 重定向到 ./，对前端来说那是多余的往返。
			serveIndex(w, sub)
			return
		}
		if _, err := fs.Stat(sub, name); err != nil {
			// 前端是 hash 路由，这些"看起来像路径"的请求都由前端自己解析，
			// 所以统一回 index.html，刷新页面不会 404。
			serveIndex(w, sub)
			return
		}
		if strings.HasPrefix(name, "assets/") {
			// Vite 给产物文件名带了内容 hash，可以放心让浏览器长期缓存
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		files.ServeHTTP(w, r)
	})
}

// serveIndex 输出 index.html（禁缓存：前端升级后刷新就能拿到新版本）。
func serveIndex(w http.ResponseWriter, sub fs.FS) {
	data, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.Error(w, "webui: 缺少 index.html（先在 web/ 目录跑 npm run build）", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(data)
}
