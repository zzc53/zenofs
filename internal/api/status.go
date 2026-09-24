package api

import (
	"encoding/json"
	"net/http"
	"runtime/debug"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
)

// statusView 是 GET /api/status 的响应：只放前端引导流程需要的、不敏感的信息。
type statusView struct {
	Version         string `json:"version"`
	BootstrapNeeded bool   `json:"bootstrap_needed"`
}

// handleStatus 免认证地报告"系统里是否还没有用户"。
//
// 前端启动时先调它：需要 bootstrap 就走首启引导（建管理员 → pool → disk → share），
// 否则直接进登录页。
func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	needed := false
	if s.auth != nil {
		if has, err := s.auth.HasUsers(); err == nil {
			needed = !has
		}
	}
	respondJSON(w, http.StatusOK, statusView{Version: buildVersion(), BootstrapNeeded: needed})
}

// buildVersion 返回二进制里记录的版本；开发构建（go run）下是 "dev"。
func buildVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return "dev"
}

// taskView 是后台任务（重建 / 修复）的对外表示。
type taskView struct {
	Id        int64           `json:"id"`
	Name      string          `json:"name"`
	Status    string          `json:"status"` // pending / running / success / failed
	Message   string          `json:"message"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
	CreatedAt int64           `json:"created_at"`
	UpdatedAt int64           `json:"updated_at"`
}

// registerTaskRoutes 注册后台任务端点。
func (s *server) registerTaskRoutes(r chi.Router) {
	// GET /api/tasks?limit=50 —— 列出后台任务（管理员），最近的在前
	r.Get("/tasks", func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r) {
			return
		}
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
				limit = n
			}
		}
		var tasks []db.Task
		if err := s.pm.DbManager.DB.Order("id DESC").Limit(limit).Find(&tasks).Error; err != nil {
			respondErr(w, errs.DBQuery(err))
			return
		}
		views := make([]taskView, 0, len(tasks))
		for i := range tasks {
			t := &tasks[i]
			views = append(views, taskView{
				Id:        t.Id,
				Name:      t.Name,
				Status:    taskStatusName(t.Status),
				Message:   t.Message,
				Metadata:  json.RawMessage(t.Metadata),
				CreatedAt: t.CreatedAt,
				UpdatedAt: t.UpdatedAt,
			})
		}
		respondJSON(w, http.StatusOK, views)
	})
}

// taskStatusName 把任务状态枚举翻成可读字符串。
func taskStatusName(s db.TaskStatus) string {
	switch s {
	case db.TaskPending:
		return "pending"
	case db.TaskRunning:
		return "running"
	case db.TaskSuccess:
		return "success"
	case db.TaskFail:
		return "failed"
	}
	return "unknown"
}
