package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
	"github.com/zzc53/zenofs/internal/pool"
)

type apiError struct {
	Code    int    `json:"code"`
	StrCode string `json:"str_code"`
	Message string `json:"message"`
	Value   string `json:"value,omitempty"`
}

func respondJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func respondErr(w http.ResponseWriter, err error) {
	if ze, ok := err.(*errs.ZenoError); ok {
		respondJSON(w, http.StatusBadRequest, apiError{
			Code: ze.Code, StrCode: ze.StrCode, Message: ze.Message, Value: ze.Value,
		})
	} else if err != nil {
		respondJSON(w, http.StatusInternalServerError, apiError{Message: err.Error()})
	}
}

// NewRouter 创建并返回配置好所有路由的 chi Router。
func NewRouter(pm *pool.PoolManager, _ *db.DbManager) *chi.Mux {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Route("/api", func(r chi.Router) {
		// ── Pool ──
		r.Post("/pools", func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Name      string `json:"name"`
				ChunkSize int64  `json:"chunk_size_kb"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				respondJSON(w, http.StatusBadRequest, apiError{Message: "invalid json"})
				return
			}
			p, err := pm.AddPool(body.Name, body.ChunkSize)
			if err != nil {
				respondErr(w, err)
				return
			}
			respondJSON(w, http.StatusCreated, p)
		})

		r.Get("/pools/{id}", func(w http.ResponseWriter, r *http.Request) {
			id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
			p, err := pm.GetPool(id)
			if err != nil {
				respondErr(w, err)
				return
			}
			respondJSON(w, http.StatusOK, p)
		})

		r.Put("/pools/{id}/offline", func(w http.ResponseWriter, r *http.Request) {
			id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
			if err := pm.OfflinePool(id); err != nil {
				respondErr(w, err)
				return
			}
			respondJSON(w, http.StatusOK, map[string]string{"status": "offline"})
		})

		// ── Disk ──
		r.Post("/pools/{poolId}/disks", func(w http.ResponseWriter, r *http.Request) {
			poolId, _ := strconv.ParseInt(chi.URLParam(r, "poolId"), 10, 64)
			var body struct {
				Path      string `json:"path"`
				AddParity bool   `json:"add_parity"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				respondJSON(w, http.StatusBadRequest, apiError{Message: "invalid json"})
				return
			}
			d, err := pm.AddDisk(poolId, body.Path, int8(db.LocalBackend), int8(db.DataDisk), body.AddParity)
			if err != nil {
				respondErr(w, err)
				return
			}
			respondJSON(w, http.StatusCreated, d)
		})

		r.Put("/disks/{diskId}/swap", func(w http.ResponseWriter, r *http.Request) {
			diskId, _ := strconv.ParseInt(chi.URLParam(r, "diskId"), 10, 64)
			var body struct {
				Path string `json:"path"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				respondJSON(w, http.StatusBadRequest, apiError{Message: "invalid json"})
				return
			}
			if err := pm.SwapDisk(diskId, body.Path); err != nil {
				respondErr(w, err)
				return
			}
			respondJSON(w, http.StatusOK, map[string]string{"status": "swapped"})
		})

		// ── Chunk ──
		r.Post("/pools/{poolId}/chunks", func(w http.ResponseWriter, r *http.Request) {
			poolId, _ := strconv.ParseInt(chi.URLParam(r, "poolId"), 10, 64)
			var body struct {
				Data [][]byte `json:"data"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				respondJSON(w, http.StatusBadRequest, apiError{Message: "invalid json"})
				return
			}
			chunks, err := pm.AddChunks(poolId, body.Data)
			if err != nil {
				respondErr(w, err)
				return
			}
			respondJSON(w, http.StatusCreated, chunks)
		})

		r.Post("/chunks/write", func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				PoolId int64                `json:"pool_id"`
				Items  []pool.WriteChunkItem `json:"items"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				respondJSON(w, http.StatusBadRequest, apiError{Message: "invalid json"})
				return
			}
			chunks, err := pm.WriteChunks(body.PoolId, body.Items)
			if err != nil {
				respondErr(w, err)
				return
			}
			respondJSON(w, http.StatusOK, chunks)
		})

		r.Post("/chunks/read", func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				PoolId   int64   `json:"pool_id"`
				ChunkIds []int64 `json:"chunk_ids"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				respondJSON(w, http.StatusBadRequest, apiError{Message: "invalid json"})
				return
			}
			data, err := pm.ReadChunks(body.PoolId, body.ChunkIds)
			if err != nil {
				respondErr(w, err)
				return
			}
			respondJSON(w, http.StatusOK, map[string]any{"data": data})
		})

		// ── Repair ──
		r.Post("/pools/{poolId}/reconstruct", func(w http.ResponseWriter, r *http.Request) {
			poolId, _ := strconv.ParseInt(chi.URLParam(r, "poolId"), 10, 64)
			pm.ReconstructStripes(poolId)
			respondJSON(w, http.StatusOK, map[string]string{"status": "reconstruct started"})
		})
	})

	return r
}
