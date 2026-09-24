// Package api 提供 ZenoFS 的 HTTP REST API。
//
// 路由基于 chi 实现，所有端点以 /api 为前缀，
// 请求与响应的 Content-Type 均为 application/json。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/zzc53/zenofs/internal/auth"
	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
	"github.com/zzc53/zenofs/internal/pool"
	"github.com/zzc53/zenofs/internal/token"
	zenowebdav "github.com/zzc53/zenofs/internal/webdav"
	"github.com/zzc53/zenofs/internal/webui"
)

// apiError 是统一的 JSON 错误响应结构。
// Code / StrCode 来自 errs.ZenoError，便于客户端按码判断。
type apiError struct {
	Code    int    `json:"code"`
	StrCode string `json:"str_code"`
	Message string `json:"message"`
	Value   string `json:"value,omitempty"`
}

// ─────────────────────────────────────────────────────────────
// 响应辅助
// ─────────────────────────────────────────────────────────────

// respondJSON 写一个 JSON 响应。
func respondJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// respondErr 把 error 转成 JSON 错误响应：
// errs.ZenoError 按错误码映射成合适的状态码（401 / 403 / 404 / 409 / 400），
// 其它 error 映射为 500。
func respondErr(w http.ResponseWriter, err error) {
	var ze *errs.ZenoError
	if errors.As(err, &ze) {
		status := statusOfCode(ze.Code)
		// 5xx 是服务端自己的毛病：原始错误（可能含表结构等内部细节）只进日志，
		// 回给客户端一句可读的话，而不是让它对着空 message 猜。
		if status >= 500 {
			log.Printf("api: internal error: %v", err)
		}
		msg := ze.Message
		if msg == "" {
			msg = ze.StrCode
		}
		respondJSON(w, status, apiError{
			Code: ze.Code, StrCode: ze.StrCode, Message: msg, Value: ze.Value,
		})
		return
	}
	if err != nil {
		log.Printf("api: internal error: %v", err)
		respondJSON(w, http.StatusInternalServerError, apiError{Message: "internal error"})
	}
}

// errorOf 把 error 转成 apiError（供自己控制状态码的场景使用）。
func errorOf(err error) apiError {
	var ze *errs.ZenoError
	if errors.As(err, &ze) {
		return apiError{Code: ze.Code, StrCode: ze.StrCode, Message: ze.Message, Value: ze.Value}
	}
	return apiError{Message: err.Error()}
}

// statusOfCode 按错误码选 HTTP 状态码：认证失败 401、无权限 403、找不到 404、
// 冲突 409，其余算客户端参数问题（400）。
func statusOfCode(code int) int {
	switch code {
	case errs.ECODE_AUTH_NO_TOKEN, errs.ECODE_AUTH_BAD_TOKEN,
		errs.ECODE_AUTH_BAD_LOGIN, errs.ECODE_OTP_INVALID:
		return http.StatusUnauthorized
	case errs.ECODE_AUTH_NOT_ADMIN:
		return http.StatusForbidden
	case errs.ECODE_USER_NOT_FOUND, errs.ECODE_SHARE_NOT_FOUND,
		errs.ECODE_RECYCLE_NOT_FOUND, errs.ECODE_VFS_NOT_FOUND, errs.ECODE_DISK_NOT_FOUND:
		return http.StatusNotFound
	case errs.ECODE_USER_EXIST, errs.ECODE_SHARE_EXIST, errs.ECODE_VFS_EXIST,
		errs.ECODE_VFS_NOT_EMPTY, errs.ECODE_DISK_EXIST:
		return http.StatusConflict
	case errs.ECODE_DB_BAD_QUERY:
		// 数据库层出错是服务端自己的问题，不该顶着 400 冒充"你参数不对"
		return http.StatusInternalServerError
	case errs.ECODE_VFS_ENCRYPTED:
		// 加密 Share 还没解锁（或已上锁）：423 Locked 比 403 更贴切，
		// 客户端据此提示"先 POST /api/shares/{id}/unlock"
		return http.StatusLocked
	case errs.ECODE_VFS_PERMISSION, errs.ECODE_VFS_READ_ONLY:
		return http.StatusForbidden
	}
	return http.StatusBadRequest
}

// badRequest 写一个 400 响应。
func badRequest(w http.ResponseWriter, msg string) {
	respondJSON(w, http.StatusBadRequest, apiError{Message: msg})
}

// ─────────────────────────────────────────────────────────────
// 请求辅助
// ─────────────────────────────────────────────────────────────

// pathID 解析路径参数中的整型 ID；失败时写 400 并返回 false。
func pathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	if err != nil {
		badRequest(w, "invalid path id: "+name)
		return 0, false
	}
	return id, true
}

// decodeBody 解析 JSON 请求体；失败时写 400 并返回 false。
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		badRequest(w, "invalid json body")
		return false
	}
	return true
}

// readBody 读取请求体的全部字节；失败时写 400 并返回 false。
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		badRequest(w, "read body failed")
		return nil, false
	}
	return data, true
}

// ─────────────────────────────────────────────────────────────
// 路由
// ─────────────────────────────────────────────────────────────

// NewRouter 创建并返回配置好所有路由的 HTTP 处理器。
//
// 端点一览（除 bootstrap / login 与 WebDAV 之外都要 Authorization: Bearer <JWT>）：
//
//	POST   /api/auth/bootstrap            创建第一个用户（管理员，免认证）
//	POST   /api/auth/login                用户名 + 密码 + 六位验证码 → JWT
//	GET    /api/auth/me                   当前登录用户
//	GET    /api/users                     列出用户（管理员）
//	POST   /api/users                     创建用户（管理员）
//	GET    /api/users/{id}                查询用户（管理员或本人）
//	PUT    /api/users/{id}                改密码 / 角色 / 重置 OTP
//	DELETE /api/users/{id}                删除用户（管理员）
//	GET    /api/shares                    列出可见的 Share
//	POST   /api/shares                    创建 Share（管理员）
//	GET    /api/shares/{id}               查询 Share
//	PUT    /api/shares/{id}               改配额 / 压缩（管理员）
//	DELETE /api/shares/{id}               删除 Share（管理员）
//	POST   /api/shares/{id}/users         授权某个用户（管理员）
//	DELETE /api/shares/{id}/users/{userId} 撤销授权（管理员）
//	GET    /api/shares/{id}/list          列出目录（?path=/dir）
//	GET    /api/shares/{id}/stat          查询元数据（?path=/a.txt）
//	GET    /api/shares/{id}/files         下载文件（?path=/a.txt）
//	PUT    /api/shares/{id}/files         上传文件（?path=/a.txt，body 为原始字节）
//	DELETE /api/shares/{id}/files         删除并进回收站（?path=/a.txt）
//	POST   /api/shares/{id}/folders       新建目录（{"path":"/dir"}）
//	POST   /api/shares/{id}/rename        改名 / 移动（{"from":"/a","to":"/b"}）
//	GET    /api/shares/{id}/recycle       回收站列表
//	POST   /api/shares/{id}/recycle/{inodeId}/restore  恢复
//	DELETE /api/shares/{id}/recycle/{inodeId}          彻底删除
//	DELETE /api/shares/{id}/recycle                    清空回收站
//	GET    /api/pools                     列出存储池
//	POST   /api/pools                     创建存储池
//	GET    /api/pools/{id}                查询存储池
//	PUT    /api/pools/{id}/offline        存储池下线
//	GET    /api/pools/{id}/disks          列出池里的磁盘
//	POST   /api/pools/{poolId}/disks      给存储池添加磁盘（type=data|cache，data 可选 add_parity）
//	POST   /api/pools/{id}/rebuild        投递条带重建作业（重建 / reconstruct）
//	POST   /api/pools/{id}/reconstruct    同 rebuild
//	PUT    /api/disks/{diskId}/swap       替换故障磁盘路径
//	DELETE /api/disks/{diskId}            删除缓存盘（连同缓存文件与记录）
//	POST   /api/pools/{poolId}/chunks     上传一个 chunk
//	GET    /api/chunks/{id}               读取一个 chunk
//	PUT    /api/chunks/{id}               覆写一个 chunk
//	POST   /api/users/{id}/tokens         创建访问 token（明文只返回一次）
//	GET    /api/users/{id}/tokens         列出某用户的凭证
//	POST   /api/users/{id}/pubkeys        注册 SFTP 公钥
//	DELETE /api/tokens/{id}               吊销凭证
//
// 返回 http.Handler 而不是 *chi.Mux：WebDAV 的方法集（PROPFIND / COPY / MOVE /
// LOCK ...）不在 chi 预置的方法里，直接 Mount 会被 chi 判成 405，所以最外层按
// 前缀先分发，再落到 chi 的 /api 上。日志与 panic 恢复统一包在最外层。
func NewRouter(opts Options) http.Handler {
	s := &server{pm: opts.PoolManager, tokens: opts.Tokens, auth: opts.Auth}

	r := chi.NewRouter()
	r.Route("/api", func(r chi.Router) {
		// 免认证：裸状态、首个用户 bootstrap、登录
		r.Get("/status", s.handleStatus)
		r.Post("/auth/bootstrap", s.handleBootstrap)
		r.Post("/auth/login", s.handleLogin)

		// 其余端点一律要 JWT
		r.Group(func(r chi.Router) {
			r.Use(s.authMiddleware)
			r.Get("/auth/me", s.handleMe)
			s.registerUserRoutes(r)
			s.registerShareRoutes(r)
			s.registerPoolRoutes(r)
			s.registerDiskRoutes(r)
			s.registerChunkRoutes(r)
			s.registerTaskRoutes(r)
			s.registerTokenRoutes(r)
		})
	})

	prefix := webdavPrefix(opts.WebDAVPrefix)

	// WebDAV 与 REST API 共用一个 HTTP 服务：不额外监听端口，
	// 客户端用 Basic（用户名 + access token）认证，视图是 /<share>/...。
	dav := http.Handler(http.NotFoundHandler())
	if prefix != "" {
		dav = zenowebdav.New(s.pm, s.tokens, prefix)
	}
	// 前端（静态资源 + index.html 回退）挂在根路径上
	ui := http.Handler(http.NotFoundHandler())
	if !opts.DisableWebUI {
		ui = webui.Handler()
	}

	// 保留路径段：/api 与 WebDAV 的前缀（含默认的 /dav）都不参与前端的 index.html 兜底。
	// 否则 WebDAV 一旦关闭或改了前缀，浏览器访问 /dav/ 会拿到一张 HTML 页面，
	// 让人以为是前端坏了——那种情况应该明确回 404。
	davPaths := []string{zenowebdav.DefaultPrefix}
	if prefix != "" && prefix != zenowebdav.DefaultPrefix {
		davPaths = append(davPaths, prefix)
	}
	matches := func(p string, prefixes ...string) bool {
		for _, pre := range prefixes {
			if p == pre || strings.HasPrefix(p, pre+"/") {
				return true
			}
		}
		return false
	}

	dispatch := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case matches(req.URL.Path, davPaths...):
			if prefix == "" || !matches(req.URL.Path, prefix) {
				http.NotFound(w, req)
				return
			}
			dav.ServeHTTP(w, req)
		case matches(req.URL.Path, "/api"):
			r.ServeHTTP(w, req)
		default:
			ui.ServeHTTP(w, req)
		}
	})
	// stripAccessToken 放最外层：先把它从 URL 里摘走，访问日志里就不留 token。
	return stripAccessToken(middleware.Logger(middleware.Recoverer(dispatch)))
}

// Options 是 HTTP 服务的依赖与可选项。
type Options struct {
	PoolManager *pool.PoolManager // Share / 文件 / chunk 端点都用它
	Tokens      *token.Manager    // 协议访问凭证（SMB / SFTP / WebDAV）
	Auth        *auth.Manager     // 用户与 JWT
	// WebDAVPrefix 是 WebDAV 的挂载前缀：空表示默认的 zenowebdav.DefaultPrefix（"/dav"），
	// "off" 或 "-" 表示不挂载。不以 "/" 开头时会自动补上。
	WebDAVPrefix string
	// DisableWebUI 关掉根路径上的前端静态资源（接口-only 部署时用）。
	DisableWebUI bool
}

// server 把 API 处理函数需要的依赖收在一起。
type server struct {
	pm     *pool.PoolManager
	tokens *token.Manager
	auth   *auth.Manager
}

// userKey 是登录用户在 request context 里的键。
type userKey struct{}

// authMiddleware 校验 Authorization: Bearer <JWT>，并把用户放进 context。
func (s *server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.auth == nil {
			// 没装配认证（如只跑存储层测试）时不拦，保持旧行为
			next.ServeHTTP(w, r)
			return
		}
		token := bearerToken(r)
		if token == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="zenofs"`)
			respondJSON(w, http.StatusUnauthorized, errorOf(auth.ErrNoToken))
			return
		}
		u, err := s.auth.Parse(token)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="zenofs"`)
			respondJSON(w, http.StatusUnauthorized, errorOf(err))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, u)))
	})
}

// userOf 取当前登录用户（中间件已经保证存在）。
func userOf(r *http.Request) *db.User {
	u, _ := r.Context().Value(userKey{}).(*db.User)
	return u
}

// requireAdmin 检查管理员权限；不是管理员时写响应并返回 false。
func requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	u := userOf(r)
	if u == nil || u.Role != db.UserAdmin {
		respondErr(w, auth.ErrNotAdmin)
		return false
	}
	return true
}

// webdavPrefix 解析 WebDAV 的挂载前缀；返回空串表示关闭。
func webdavPrefix(v string) string {
	switch v = strings.TrimSpace(v); v {
	case "off", "-":
		return ""
	case "":
		return zenowebdav.DefaultPrefix
	}
	if !strings.HasPrefix(v, "/") {
		v = "/" + v
	}
	return strings.TrimSuffix(v, "/")
}

// registerPoolRoutes 注册存储池相关端点。
func (s *server) registerPoolRoutes(r chi.Router) {
	// GET /api/pools —— 列出存储池
	r.Get("/pools", func(w http.ResponseWriter, r *http.Request) {
		var pools []db.Pool
		if err := s.pm.DbManager.DB.Order("id").Find(&pools).Error; err != nil {
			respondErr(w, errs.DBQuery(err))
			return
		}
		respondJSON(w, http.StatusOK, pools)
	})

	// GET /api/pools/{id}/disks —— 列出池里的磁盘
	r.Get("/pools/{id}/disks", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		if _, err := s.pm.GetPool(id); err != nil {
			respondErr(w, err)
			return
		}
		var disks []db.Disk
		if err := s.pm.DbManager.DB.Where("pool_id = ?", id).Order("id").Find(&disks).Error; err != nil {
			respondErr(w, errs.DBQuery(err))
			return
		}
		respondJSON(w, http.StatusOK, disks)
	})

	// POST /api/pools —— 创建存储池（name + chunk_size_kb）
	r.Post("/pools", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name      string `json:"name"`
			ChunkSize int64  `json:"chunk_size_kb"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		p, err := s.pm.AddPool(body.Name, body.ChunkSize)
		if err != nil {
			respondErr(w, err)
			return
		}
		respondJSON(w, http.StatusCreated, p)
	})

	// GET /api/pools/{id} —— 查询存储池
	r.Get("/pools/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		p, err := s.pm.GetPool(id)
		if err != nil {
			respondErr(w, err)
			return
		}
		respondJSON(w, http.StatusOK, p)
	})

	// PUT /api/pools/{id}/offline —— 将存储池标记为离线
	r.Put("/pools/{id}/offline", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		if err := s.pm.OfflinePool(id); err != nil {
			respondErr(w, err)
			return
		}
		respondJSON(w, http.StatusOK, map[string]string{"status": "offline"})
	})

	// POST /api/pools/{id}/rebuild —— 为待修复磁盘投递条带重建作业
	// 返回 {"queued": N}；重建由后台 worker 消费，完成后磁盘与池自动恢复 Online。
	r.Post("/pools/{id}/rebuild", s.handleRebuildPool)
	// POST /api/pools/{id}/reconstruct —— 与 rebuild 同义（换个更常见的叫法）
	r.Post("/pools/{id}/reconstruct", s.handleRebuildPool)
}

// handleRebuildPool 是 rebuild / reconstruct 的公共处理。
func (s *server) handleRebuildPool(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	queued, err := s.pm.RebuildPool(id)
	if err != nil {
		respondErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]int{"queued": queued})
}

// registerDiskRoutes 注册磁盘相关端点。
func (s *server) registerDiskRoutes(r chi.Router) {
	// POST /api/pools/{poolId}/disks —— 向存储池添加磁盘
	//
	// 请求：{"path":"/var/lib/zenofs/d0","type":"data","add_parity":false}
	//
	//	type        "data"（默认，参与条带化）| "cache"（仅做读缓存）
	//	add_parity  仅对 data 盘有意义：true 时这块盘为池增加一个**校验分片**。
	//	            条带里的 data / parity 槽位是每个条带按打乱后的盘序分配的，
	//	            所以它不是"把这块盘固定成校验盘"，只是调整池的 Parities 计数。
	r.Post("/pools/{poolId}/disks", func(w http.ResponseWriter, r *http.Request) {
		poolId, ok := pathID(w, r, "poolId")
		if !ok {
			return
		}
		if _, err := s.pm.GetPool(poolId); err != nil {
			respondErr(w, err)
			return
		}
		var body struct {
			Path      string `json:"path"`
			Type      string `json:"type"`
			AddParity bool   `json:"add_parity"`
		}
		if !decodeBody(w, r, &body) {
			return
		}

		diskType := db.DataDisk
		switch body.Type {
		case "", "data":
		case "cache":
			diskType = db.CacheDisk
			if body.AddParity {
				// 缓存盘不参与条带，谈不上"校验分片"，明确拒掉而不是悄悄忽略
				badRequest(w, "缓存盘不能计入校验分片（add_parity 只对数据盘有效）")
				return
			}
		default:
			badRequest(w, `type 只支持 "data" / "cache"`)
			return
		}

		d, err := s.pm.AddDisk(poolId, body.Path, int8(db.LocalBackend), int8(diskType), body.AddParity)
		if err != nil {
			respondErr(w, err)
			return
		}
		respondJSON(w, http.StatusCreated, d)
	})

	// DELETE /api/disks/{diskId} —— 删除一块缓存盘（管理员）
	//
	// 只允许删缓存盘：数据盘是唯一数据副本，删掉会丢数据，要走"下线 / 换盘 + 重建"。
	// 删除时会一并清掉这块盘上的缓存文件与 read_caches 记录。
	r.Delete("/disks/{diskId}", func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r) {
			return
		}
		diskId, ok := pathID(w, r, "diskId")
		if !ok {
			return
		}
		removed, err := s.pm.DeleteCacheDisk(diskId)
		if err != nil {
			respondErr(w, err)
			return
		}
		respondJSON(w, http.StatusOK, map[string]any{"status": "deleted", "removed_caches": removed})
	})

	// PUT /api/disks/{diskId}/swap —— 替换故障磁盘路径
	// 会把该盘置为 Repair 并把所属池下线，重建完成后自动恢复。
	r.Put("/disks/{diskId}/swap", func(w http.ResponseWriter, r *http.Request) {
		diskId, ok := pathID(w, r, "diskId")
		if !ok {
			return
		}
		var body struct {
			Path string `json:"path"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		if err := s.pm.SwapDisk(diskId, body.Path); err != nil {
			respondErr(w, err)
			return
		}
		respondJSON(w, http.StatusOK, map[string]string{"status": "swapped"})
	})
}

// registerChunkRoutes 注册 chunk 读写端点。
func (s *server) registerChunkRoutes(r chi.Router) {
	// POST /api/pools/{poolId}/chunks —— 上传数据，返回分配到的 chunk 元数据
	r.Post("/pools/{poolId}/chunks", func(w http.ResponseWriter, r *http.Request) {
		poolId, ok := pathID(w, r, "poolId")
		if !ok {
			return
		}
		data, ok := readBody(w, r)
		if !ok {
			return
		}
		c, err := s.pm.AddChunk(poolId, data)
		if err != nil {
			respondErr(w, err)
			return
		}
		respondJSON(w, http.StatusCreated, c)
	})

	// GET /api/chunks/{id} —— 完整读取一个 chunk（直接返回原始字节）
	r.Get("/chunks/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		poolId, err := s.pm.PoolIdOfChunk(id)
		if err != nil {
			respondErr(w, err)
			return
		}
		data, err := s.pm.ReadChunks(poolId, []int64{id})
		if err != nil {
			respondErr(w, err)
			return
		}
		w.Write(data[0])
	})

	// PUT /api/chunks/{id} —— 完整覆写一个 chunk
	r.Put("/chunks/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		data, ok := readBody(w, r)
		if !ok {
			return
		}
		chunks, err := s.pm.WriteChunks([]pool.WriteChunkItem{{ChunkId: id, Data: data}})
		if err != nil {
			respondErr(w, err)
			return
		}
		respondJSON(w, http.StatusOK, chunks[0])
	})
}

// queryTokenKey 是"从 ?access_token= 摘下来的 token"在 context 里的键。
type queryTokenKey struct{}

// stripAccessToken 把 ?access_token= 从 URL 里摘出来放进 context，再往下传。
//
// 为什么需要它：浏览器用 <a href> 下载或在新窗口打开文件时发不出 Authorization 头，
// 只能把 token 放 URL；而访问日志会把整条 URL 打出来。先摘掉再记日志，日志里就不留 token。
func stripAccessToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery == "" {
			next.ServeHTTP(w, r)
			return
		}
		q := r.URL.Query()
		tok := strings.TrimSpace(q.Get("access_token"))
		if tok == "" {
			next.ServeHTTP(w, r)
			return
		}
		q.Del("access_token")
		clone := r.Clone(r.Context())
		clone.URL.RawQuery = q.Encode()
		// RequestURI 也要一起改写：chi 的访问日志直接打它（不是 URL），
		// 只改 URL 的话 token 照样会落到日志里。
		clone.RequestURI = clone.URL.RequestURI()
		clone = clone.WithContext(context.WithValue(clone.Context(), queryTokenKey{}, tok))
		next.ServeHTTP(w, clone)
	})
}

// bearerToken 取请求里的 JWT：优先 Authorization 头，其次 ?access_token=。
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	if hdr := strings.TrimSpace(r.Header.Get("Authorization")); hdr != "" {
		if strings.HasPrefix(hdr, prefix) {
			return strings.TrimSpace(hdr[len(prefix):])
		}
		return ""
	}
	tok, _ := r.Context().Value(queryTokenKey{}).(string)
	return tok
}
