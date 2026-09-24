package api

import (
	"context"
	"io"
	"log"
	"net/http"
	"path"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/zzc53/zenofs/internal/vfs"
)

// fileView 是文件 / 目录的对外表示。
type fileView struct {
	Id     int64  `json:"id"`
	Name   string `json:"name"`
	Path   string `json:"path"`
	Kind   string `json:"kind"` // file / dir / link
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"` // POSIX 权限位（含类型位）
	Mtime  int64  `json:"mtime"`
	Target string `json:"target,omitempty"`
}

// viewOfFile 把 vfs 元数据转成对外表示。
func viewOfFile(fi vfs.FileInfo) fileView {
	return fileView{
		Id:     fi.Id,
		Name:   fi.Name,
		Path:   fi.Path,
		Kind:   kindName(fi.Kind),
		Size:   fi.Size,
		Mode:   uint32(fi.Mode),
		Mtime:  fi.Mtime.Unix(),
		Target: fi.Target,
	}
}

// kindName 把 vfs 的条目类型翻成可读字符串。
func kindName(k vfs.Kind) string {
	switch k {
	case vfs.KindDir:
		return "dir"
	case vfs.KindSymlink:
		return "link"
	}
	return "file"
}

// queryPath 取 ?path=，缺省为挂载根 "/"。
func queryPath(r *http.Request) string {
	p := r.URL.Query().Get("path")
	if p == "" {
		return "/"
	}
	return p
}

// registerFileRoutes 注册文件读写端点。
//
// 读要求 ShareRead、写要求 ShareWrite（见 shareFS）；删除是软删除，进回收站，
// 用 recycle.go 里的端点恢复或彻底清除。
func (s *server) registerFileRoutes(r chi.Router) {
	// GET /api/shares/{id}/list?path=/dir —— 列出目录
	r.Get("/shares/{id}/list", func(w http.ResponseWriter, r *http.Request) {
		shareId, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		fs, _, err := s.shareFS(userOf(r).Id, shareId, false)
		if err != nil {
			respondErr(w, err)
			return
		}
		p := queryPath(r)
		entries, err := fs.ReadDir(r.Context(), p)
		if err != nil {
			respondErr(w, err)
			return
		}
		views := make([]fileView, 0, len(entries))
		for _, e := range entries {
			views = append(views, viewOfFile(e))
		}
		respondJSON(w, http.StatusOK, map[string]any{
			"path":    p,
			"entries": views,
		})
	})

	// GET /api/shares/{id}/stat?path=/a.txt —— 查询元数据
	r.Get("/shares/{id}/stat", func(w http.ResponseWriter, r *http.Request) {
		shareId, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		fs, _, err := s.shareFS(userOf(r).Id, shareId, false)
		if err != nil {
			respondErr(w, err)
			return
		}
		fi, err := fs.Stat(r.Context(), queryPath(r))
		if err != nil {
			respondErr(w, err)
			return
		}
		respondJSON(w, http.StatusOK, viewOfFile(fi))
	})

	// GET /api/shares/{id}/files?path=/a.txt —— 下载（支持 Range）
	r.Get("/shares/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		shareId, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		fs, _, err := s.shareFS(userOf(r).Id, shareId, false)
		if err != nil {
			respondErr(w, err)
			return
		}
		p := queryPath(r)
		f, err := fs.Open(r.Context(), p, vfs.OpenFlags{Read: true}, 0)
		if err != nil {
			respondErr(w, err)
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			respondErr(w, err)
			return
		}
		if fi.Kind == vfs.KindDir {
			respondErr(w, vfs.ErrIsDir)
			return
		}
		// ServeContent 处理 Range、If-Modified-Since 与 Content-Type
		http.ServeContent(w, r, path.Base(p), fi.Mtime, f)
	})

	// PUT /api/shares/{id}/files?path=/a.txt —— 上传（body 就是文件内容）
	r.Put("/shares/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		shareId, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		fs, _, err := s.shareFS(userOf(r).Id, shareId, true)
		if err != nil {
			respondErr(w, err)
			return
		}
		p := queryPath(r)
		if p == "/" {
			badRequest(w, "path 不能是根目录")
			return
		}
		f, err := fs.Open(r.Context(), p,
			vfs.OpenFlags{Read: false, Write: true, Create: true, Truncate: true}, 0o644)
		if err != nil {
			respondErr(w, err)
			return
		}
		if _, err := io.Copy(f, r.Body); err != nil {
			// 传输中断（客户端断开、连接被掐等）：把这半个文件清掉，免得列表里留下一个
			// 看着正常、其实不完整的文件。vfs 在 Close 时才提交版本，所以必须先 Close
			// 再删；用独立的 context，因为这时候请求上下文往往已经取消了。
			if closeErr := f.Close(); closeErr != nil {
				log.Printf("upload %s: 关闭句柄失败: %v", p, closeErr)
			}
			if rmErr := fs.Remove(context.Background(), p); rmErr != nil {
				log.Printf("upload %s: 清理未完成的文件失败: %v", p, rmErr)
			}
			badRequest(w, "upload interrupted: "+err.Error())
			return
		}
		// vfs 在 Close 时提交新版本，所以必须先 Close 再 Stat
		if err := f.Close(); err != nil {
			respondErr(w, err)
			return
		}
		fi, err := fs.Stat(r.Context(), p)
		if err != nil {
			respondErr(w, err)
			return
		}
		respondJSON(w, http.StatusCreated, viewOfFile(fi))
	})

	// DELETE /api/shares/{id}/files?path=/a.txt —— 删除（软删除，进回收站）
	r.Delete("/shares/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		shareId, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		fs, _, err := s.shareFS(userOf(r).Id, shareId, true)
		if err != nil {
			respondErr(w, err)
			return
		}
		if err := fs.Remove(r.Context(), queryPath(r)); err != nil {
			respondErr(w, err)
			return
		}
		respondJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	})

	// POST /api/shares/{id}/folders —— 新建目录 {"path":"/dir/sub"}
	r.Post("/shares/{id}/folders", func(w http.ResponseWriter, r *http.Request) {
		shareId, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		fs, _, err := s.shareFS(userOf(r).Id, shareId, true)
		if err != nil {
			respondErr(w, err)
			return
		}
		var body struct {
			Path string `json:"path"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		if strings.TrimSpace(body.Path) == "" {
			badRequest(w, "path 不能为空")
			return
		}
		if err := fs.Mkdir(r.Context(), body.Path, 0o755); err != nil {
			respondErr(w, err)
			return
		}
		fi, err := fs.Stat(r.Context(), body.Path)
		if err != nil {
			respondErr(w, err)
			return
		}
		respondJSON(w, http.StatusCreated, viewOfFile(fi))
	})

	// POST /api/shares/{id}/rename —— 改名 / 移动 {"from":"/a","to":"/b"}
	r.Post("/shares/{id}/rename", func(w http.ResponseWriter, r *http.Request) {
		shareId, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		fs, _, err := s.shareFS(userOf(r).Id, shareId, true)
		if err != nil {
			respondErr(w, err)
			return
		}
		var body struct {
			From string `json:"from"`
			To   string `json:"to"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		if strings.TrimSpace(body.From) == "" || strings.TrimSpace(body.To) == "" {
			badRequest(w, "from / to 都不能为空")
			return
		}
		if err := fs.Rename(r.Context(), body.From, body.To); err != nil {
			respondErr(w, err)
			return
		}
		fi, err := fs.Stat(r.Context(), body.To)
		if err != nil {
			respondErr(w, err)
			return
		}
		respondJSON(w, http.StatusOK, viewOfFile(fi))
	})
}
