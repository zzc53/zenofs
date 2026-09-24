// Package api 提供 ZenoFS 的 HTTP REST API。
//
// 路由基于 chi 实现，所有端点以 /api 为前缀，
// 请求与响应的 Content-Type 均为 application/json。
package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
	"github.com/zzc53/zenofs/internal/pool"
	"github.com/zzc53/zenofs/internal/token"
	zenowebdav "github.com/zzc53/zenofs/internal/webdav"
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
// errs.ZenoError 映射为 400 + 结构化错误码，其它 error 映射为 500。
func respondErr(w http.ResponseWriter, err error) {
	var ze *errs.ZenoError
	if errors.As(err, &ze) {
		respondJSON(w, http.StatusBadRequest, apiError{
			Code: ze.Code, StrCode: ze.StrCode, Message: ze.Message, Value: ze.Value,
		})
		return
	}
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, apiError{Message: err.Error()})
	}
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

// NewRouter 创建并返回配置好所有路由的 chi Router。
//
// 端点一览：
//
//	POST   /api/pools                    创建存储池
//	GET    /api/pools/{id}               查询存储池
//	PUT    /api/pools/{id}/offline       存储池下线
//	POST   /api/pools/{id}/rebuild       投递条带重建作业
//	POST   /api/pools/{poolId}/disks     给存储池添加磁盘
//	PUT    /api/disks/{diskId}/swap      替换故障磁盘路径
//	POST   /api/pools/{poolId}/chunks    上传一个 chunk
//	GET    /api/chunks/{id}              读取一个 chunk
//	PUT    /api/chunks/{id}              覆写一个 chunk
//	POST   /api/users/{id}/tokens        创建访问 token（明文只返回一次）
//	GET    /api/users/{id}/tokens        列出某用户的凭证
//	POST   /api/users/{id}/pubkeys       注册 SFTP 公钥
//	DELETE /api/tokens/{id}              吊销凭证
//
// WebDAV 挂在同一个 HTTP 服务上（同一端口，默认 /dav 前缀，见 RouterOptions）。
//
// 返回 http.Handler 而不是 *chi.Mux：WebDAV 的方法集（PROPFIND / COPY / MOVE /
// LOCK ...）不在 chi 预置的方法里，直接 Mount 会被 chi 判成 405，所以最外层按
// 前缀先分发，再落到 chi 的 /api 上。日志与 panic 恢复统一包在最外层。
func NewRouter(pm *pool.PoolManager, tokens *token.Manager, opts RouterOptions) http.Handler {
	r := chi.NewRouter()
	r.Route("/api", func(r chi.Router) {
		registerPoolRoutes(r, pm)
		registerDiskRoutes(r, pm)
		registerChunkRoutes(r, pm)
		registerTokenRoutes(r, tokens)
	})

	prefix := webdavPrefix(opts.WebDAVPrefix)
	if prefix == "" {
		return middleware.Logger(middleware.Recoverer(r))
	}

	// WebDAV 与 REST API 共用一个 HTTP 服务：不额外监听端口，
	// 客户端用 Basic（用户名 + access token）认证，视图是 /<share>/...。
	dav := zenowebdav.New(pm, tokens, prefix)
	dispatch := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == prefix || strings.HasPrefix(req.URL.Path, prefix+"/") {
			dav.ServeHTTP(w, req)
			return
		}
		r.ServeHTTP(w, req)
	})
	return middleware.Logger(middleware.Recoverer(dispatch))
}

// RouterOptions 是 HTTP 路由的可选项。
type RouterOptions struct {
	// WebDAVPrefix 是 WebDAV 的挂载前缀：空表示默认的 zenowebdav.DefaultPrefix（"/dav"），
	// "off" 或 "-" 表示不挂载。不以 "/" 开头时会自动补上。
	WebDAVPrefix string
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
func registerPoolRoutes(r chi.Router, pm *pool.PoolManager) {
	// POST /api/pools —— 创建存储池（name + chunk_size_kb）
	r.Post("/pools", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name      string `json:"name"`
			ChunkSize int64  `json:"chunk_size_kb"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		p, err := pm.AddPool(body.Name, body.ChunkSize)
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
		p, err := pm.GetPool(id)
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
		if err := pm.OfflinePool(id); err != nil {
			respondErr(w, err)
			return
		}
		respondJSON(w, http.StatusOK, map[string]string{"status": "offline"})
	})

	// POST /api/pools/{id}/rebuild —— 为待修复磁盘投递条带重建作业
	// 返回 {"queued": N}；重建由后台 worker 消费，完成后磁盘与池自动恢复 Online。
	r.Post("/pools/{id}/rebuild", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		queued, err := pm.RebuildPool(id)
		if err != nil {
			respondErr(w, err)
			return
		}
		respondJSON(w, http.StatusOK, map[string]int{"queued": queued})
	})
}

// registerDiskRoutes 注册磁盘相关端点。
func registerDiskRoutes(r chi.Router, pm *pool.PoolManager) {
	// POST /api/pools/{poolId}/disks —— 向存储池添加磁盘
	// add_parity = true 时该盘占条带里的 parity 位，否则占 data 位。
	r.Post("/pools/{poolId}/disks", func(w http.ResponseWriter, r *http.Request) {
		poolId, ok := pathID(w, r, "poolId")
		if !ok {
			return
		}
		var body struct {
			Path      string `json:"path"`
			AddParity bool   `json:"add_parity"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		d, err := pm.AddDisk(poolId, body.Path, int8(db.LocalBackend), int8(db.DataDisk), body.AddParity)
		if err != nil {
			respondErr(w, err)
			return
		}
		respondJSON(w, http.StatusCreated, d)
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
		if err := pm.SwapDisk(diskId, body.Path); err != nil {
			respondErr(w, err)
			return
		}
		respondJSON(w, http.StatusOK, map[string]string{"status": "swapped"})
	})
}

// registerChunkRoutes 注册 chunk 读写端点。
func registerChunkRoutes(r chi.Router, pm *pool.PoolManager) {
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
		c, err := pm.AddChunk(poolId, data)
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
		poolId, err := pm.PoolIdOfChunk(id)
		if err != nil {
			respondErr(w, err)
			return
		}
		data, err := pm.ReadChunks(poolId, []int64{id})
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
		chunks, err := pm.WriteChunks([]pool.WriteChunkItem{{ChunkId: id, Data: data}})
		if err != nil {
			respondErr(w, err)
			return
		}
		respondJSON(w, http.StatusOK, chunks[0])
	})
}
