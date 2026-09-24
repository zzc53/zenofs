package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/zzc53/zenofs/internal/db"
)

// versionView 是一个文件版本。
type versionView struct {
	Id          int64  `json:"id"`
	Size        int64  `json:"size"`
	Hash        string `json:"hash"`
	Compression int8   `json:"compression"`
	Encryption  int8   `json:"encryption"`
	CreatedBy   int64  `json:"created_by"`
	CreatedAt   int64  `json:"created_at"`
	IsCurrent   bool   `json:"is_current"`
}

// historyView 是一条元数据变更记录。
type historyView struct {
	Id        int64  `json:"id"`
	Event     string `json:"event"` // created / renamed / moved / deleted / restored
	OldName   string `json:"old_name,omitempty"`
	NewName   string `json:"new_name,omitempty"`
	OldParent string `json:"old_parent,omitempty"` // 目录路径，不是 inode id
	NewParent string `json:"new_parent,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

// eventName 把事件类型转成稳定字符串：比裸数字好认，也方便前端做 i18n。
func eventName(t db.InodeEventType) string {
	switch t {
	case db.InodeCreated:
		return "created"
	case db.InodeRenamed:
		return "renamed"
	case db.InodeMoved:
		return "moved"
	case db.InodeDeleted:
		return "deleted"
	case db.InodeRestored:
		return "restored"
	}
	return "unknown"
}

// registerHistoryRoutes 注册版本与变更记录端点。
//
// 两个 GET 都按 inode id 查而不是路径：回收站里的条目已经没有可访问的路径，
// 但 inode 还在，它当初被改过什么、有哪些版本，照样要能看。
// 恢复版本需要写权限（与其它写操作一致，只读 Share 上返回 403）。
func (s *server) registerHistoryRoutes(r chi.Router) {
	// GET /api/shares/{id}/inodes/{inodeId}/versions —— 版本列表（新的在前）
	r.Get("/shares/{id}/inodes/{inodeId}/versions", func(w http.ResponseWriter, r *http.Request) {
		shareId, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		inodeId, ok := pathID(w, r, "inodeId")
		if !ok {
			return
		}
		fs, _, err := s.shareFS(userOf(r).Id, shareId, false)
		if err != nil {
			respondErr(w, err)
			return
		}
		versions, err := fs.ListVersions(r.Context(), inodeId)
		if err != nil {
			respondErr(w, err)
			return
		}
		views := make([]versionView, 0, len(versions))
		for _, v := range versions {
			views = append(views, versionView{
				Id:          v.Id,
				Size:        v.Size,
				Hash:        v.Hash,
				Compression: v.Compression,
				Encryption:  v.Encryption,
				CreatedBy:   v.CreatedBy,
				CreatedAt:   v.CreatedAt,
				IsCurrent:   v.IsCurrent,
			})
		}
		respondJSON(w, http.StatusOK, views)
	})

	// GET /api/shares/{id}/inodes/{inodeId}/history —— 变更记录（新的在前）
	r.Get("/shares/{id}/inodes/{inodeId}/history", func(w http.ResponseWriter, r *http.Request) {
		shareId, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		inodeId, ok := pathID(w, r, "inodeId")
		if !ok {
			return
		}
		fs, _, err := s.shareFS(userOf(r).Id, shareId, false)
		if err != nil {
			respondErr(w, err)
			return
		}
		rows, err := fs.ListHistory(r.Context(), inodeId)
		if err != nil {
			respondErr(w, err)
			return
		}
		views := make([]historyView, 0, len(rows))
		for _, h := range rows {
			views = append(views, historyView{
				Id:        h.Id,
				Event:     eventName(h.EventType),
				OldName:   h.OldName,
				NewName:   h.NewName,
				OldParent: h.OldParentPath,
				NewParent: h.NewParentPath,
				CreatedAt: h.CreatedAt,
			})
		}
		respondJSON(w, http.StatusOK, views)
	})

	// POST /api/shares/{id}/versions/{versionId}/restore —— 恢复到某个版本
	// 版本 id 本身就指向 inode，不需要再传路径。写权限不足返回 403。
	r.Post("/shares/{id}/versions/{versionId}/restore", func(w http.ResponseWriter, r *http.Request) {
		shareId, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		versionId, ok := pathID(w, r, "versionId")
		if !ok {
			return
		}
		fs, _, err := s.shareFS(userOf(r).Id, shareId, true)
		if err != nil {
			respondErr(w, err)
			return
		}
		fi, err := fs.RestoreVersion(r.Context(), versionId)
		if err != nil {
			respondErr(w, err)
			return
		}
		respondJSON(w, http.StatusOK, viewOfFile(fi))
	})
}
