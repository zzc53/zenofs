// Package webdav 把 internal/vfs 暴露成 WebDAV，并**挂在 zenofs 的 HTTP API 服务上**
// （同一个端口、默认 /dav 前缀），不单独监听端口。
//
// # 认证
//
// HTTP Basic：用户名 = zenofs 用户名，密码 = access token（见 internal/token）。
// 与 SMB/SFTP 一样，同一个 token 可以登所有协议，过期即拒绝。
//
// # 视图与权限
//
// 该用户可见的 Share 列表就是根：/dav/<share>/...（vfs.RootFS）。
// 能不能读写由 share_users 的授权决定：只读 Share 上的 PUT/MKCOL/DELETE/MOVE
// 会拿到 403。
//
// # 锁
//
// zenofs 的 vfs 不提供锁（设计如此），所以 LOCK/UNLOCK 一律成功：客户端能拿到
// 合法的 lock token 继续工作（Office、Finder 这类客户端要求 LOCK 成功才写入），
// 但服务端不做互斥——与 SMB/SFTP 的行为一致。细节见 lock.go。
package webdav

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"golang.org/x/net/webdav"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/pool"
	"github.com/zzc53/zenofs/internal/token"
	"github.com/zzc53/zenofs/internal/vfs"
)

// DefaultPrefix 是 WebDAV 的默认挂载前缀。
const DefaultPrefix = "/dav"

// userIDKey 是认证结果在 request context 里的键。
type userIDKey struct{}

// Handler 是挂在 API 服务上的 WebDAV 处理器。
type Handler struct {
	pm     *pool.PoolManager
	tokens *token.Manager
	dav    *webdav.Handler

	mu    sync.Mutex
	roots map[int64]*vfs.RootFS // user id → 聚合挂载点（缓存 Share 视图与实例）
}

var _ http.Handler = (*Handler)(nil)

// New 创建 WebDAV 处理器。
//
// prefix 是 URL 前缀（空表示 DefaultPrefix），**必须与路由挂载的位置一致**：
// x/net 的 Handler 用它来剥离请求路径，前缀不匹配会回 404。
func New(pm *pool.PoolManager, tokens *token.Manager, prefix string) *Handler {
	if prefix == "" {
		prefix = DefaultPrefix
	}
	h := &Handler{pm: pm, tokens: tokens, roots: make(map[int64]*vfs.RootFS)}
	h.dav = &webdav.Handler{
		Prefix:     prefix,
		FileSystem: &davFS{h: h},
		LockSystem: newNoLock(),
		Logger: func(r *http.Request, err error) {
			if err != nil {
				log.Printf("webdav: %s %s: %v", r.Method, r.URL.Path, err)
			}
		},
	}
	return h
}

// ServeHTTP 先做 Basic（用户名 + token）认证，再交给 x/net 的 WebDAV 处理器。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	if !h.authorizeWrite(w, r, userID) {
		return
	}
	ctx := context.WithValue(r.Context(), userIDKey{}, userID)
	h.dav.ServeHTTP(w, r.WithContext(ctx))
}

// writeMethod 判断方法是否会改数据（需要在进处理器之前先查写权限）。
func writeMethod(method string) bool {
	switch method {
	case "PUT", "MKCOL", "DELETE", "MOVE", "COPY", "PROPPATCH":
		return true
	}
	return false
}

// authorizeWrite 在进入 WebDAV 处理器之前先判写权限。
//
// 原因是 x/net 的 handlePut 会把 OpenFile 的任何错误都当成 404：只读 Share 上的
// 上传会得到误导性的"资源不存在"，客户端（Finder 等）还会反复重试。
// 这里提前按 Share 的授权判一次，只读就明确回 403。
// Share 不存在、或该用户对它没有授权时不拦——交给下游回 404，避免暴露 Share 名。
func (h *Handler) authorizeWrite(w http.ResponseWriter, r *http.Request, userID int64) bool {
	if !writeMethod(r.Method) {
		return true
	}
	root := h.root(userID)
	for _, p := range writeTargets(r) {
		name := shareOf(h.dav.Prefix, p)
		if name == "" {
			continue
		}
		_, perm, err := root.Share(name)
		if err != nil {
			continue
		}
		if perm < db.ShareWrite {
			http.Error(w, "read-only share", http.StatusForbidden)
			return false
		}
	}
	return true
}

// writeTargets 返回一次写请求会改到的路径：
// MOVE 会同时改源与目标，COPY 只写目标，其余写请求就是请求路径本身。
func writeTargets(r *http.Request) []string {
	switch r.Method {
	case "MOVE":
		return []string{r.URL.Path, destinationPath(r)}
	case "COPY":
		return []string{destinationPath(r)}
	default:
		return []string{r.URL.Path}
	}
}

// destinationPath 从 MOVE/COPY 的 Destination 头里取出路径（没有则返回空串）。
func destinationPath(r *http.Request) string {
	hdr := r.Header.Get("Destination")
	if hdr == "" {
		return ""
	}
	u, err := url.Parse(hdr)
	if err != nil {
		return ""
	}
	return u.Path
}

// shareOf 从 WebDAV 路径里取出第一段（Share 名）；路径非法或指向根时返回空串。
func shareOf(prefix, p string) string {
	clean, err := cleanPath(strings.TrimPrefix(p, prefix))
	if err != nil {
		return ""
	}
	segs := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	if len(segs) == 0 {
		return ""
	}
	return segs[0]
}

// authenticate 校验 HTTP Basic：用户名 + access token。
//
// 失败时统一回 401 + WWW-Authenticate，不区分"用户不存在 / token 不对 / 已过期"，
// 避免暴露用户是否存在；客户端（Finder、Cyberduck、Windows）看到 401 会带上
// 凭证重试，所以 OPTIONS 也走这条路径不影响互操作。
func (h *Handler) authenticate(w http.ResponseWriter, r *http.Request) (int64, bool) {
	user, password, ok := r.BasicAuth()
	if !ok {
		unauthorized(w)
		return 0, false
	}
	tok, err := h.tokens.AuthenticateSecret(user, password)
	if err != nil {
		unauthorized(w)
		return 0, false
	}
	return tok.UserId, true
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="zenofs", charset="UTF-8"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// root 返回某个用户的聚合挂载点（按用户缓存）。
func (h *Handler) root(userID int64) *vfs.RootFS {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.roots[userID]
	if !ok {
		r = vfs.NewRootFS(h.pm, userID)
		h.roots[userID] = r
	}
	return r
}
