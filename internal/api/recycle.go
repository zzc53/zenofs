package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// deletedView 是回收站里的一个条目：文件元数据 + 谁在什么时候删的。
type deletedView struct {
	fileView
	DeletedAt int64 `json:"deleted_at"`
	DeletedBy int64 `json:"deleted_by"`
}

// registerRecycleRoutes 注册回收站端点。
//
// 删除（DELETE /shares/{id}/files）只做软删除，条目留在这里；恢复就是把标记抹掉，
// 彻底删除才移除元数据记录（存储层的 chunk 由后续 GC 处理，见 vfs.Purge 的注释）。
func (s *server) registerRecycleRoutes(r chi.Router) {
	// GET /api/shares/{id}/recycle —— 回收站列表（最近删除的在前）
	r.Get("/shares/{id}/recycle", func(w http.ResponseWriter, r *http.Request) {
		shareId, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		fs, _, err := s.shareFS(userOf(r).Id, shareId, false)
		if err != nil {
			respondErr(w, err)
			return
		}
		entries, err := fs.ListDeleted(r.Context())
		if err != nil {
			respondErr(w, err)
			return
		}
		views := make([]deletedView, 0, len(entries))
		for _, e := range entries {
			views = append(views, deletedView{
				fileView:  viewOfFile(e.FileInfo),
				DeletedAt: e.DeletedAt,
				DeletedBy: e.DeletedBy,
			})
		}
		respondJSON(w, http.StatusOK, views)
	})

	// POST /api/shares/{id}/recycle/{inodeId}/restore —— 恢复
	// 原父目录还在就放回原位，否则回到 Share 根；同名冲突返回 409。
	r.Post("/shares/{id}/recycle/{inodeId}/restore", func(w http.ResponseWriter, r *http.Request) {
		shareId, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		inodeId, ok := pathID(w, r, "inodeId")
		if !ok {
			return
		}
		fs, _, err := s.shareFS(userOf(r).Id, shareId, true)
		if err != nil {
			respondErr(w, err)
			return
		}
		fi, err := fs.Restore(r.Context(), inodeId)
		if err != nil {
			respondErr(w, err)
			return
		}
		respondJSON(w, http.StatusOK, viewOfFile(fi))
	})

	// DELETE /api/shares/{id}/recycle —— 清空回收站
	r.Delete("/shares/{id}/recycle", func(w http.ResponseWriter, r *http.Request) {
		shareId, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		fs, _, err := s.shareFS(userOf(r).Id, shareId, true)
		if err != nil {
			respondErr(w, err)
			return
		}
		n, err := fs.PurgeAll(r.Context())
		if err != nil {
			respondErr(w, err)
			return
		}
		respondJSON(w, http.StatusOK, map[string]int{"cleared": n})
	})

	// DELETE /api/shares/{id}/recycle/{inodeId} —— 彻底删除一个条目
	r.Delete("/shares/{id}/recycle/{inodeId}", func(w http.ResponseWriter, r *http.Request) {
		shareId, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		inodeId, ok := pathID(w, r, "inodeId")
		if !ok {
			return
		}
		fs, _, err := s.shareFS(userOf(r).Id, shareId, true)
		if err != nil {
			respondErr(w, err)
			return
		}
		if err := fs.Purge(r.Context(), inodeId); err != nil {
			respondErr(w, err)
			return
		}
		respondJSON(w, http.StatusOK, map[string]string{"status": "purged"})
	})
}
