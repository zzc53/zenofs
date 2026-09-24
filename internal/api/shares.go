package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
	"github.com/zzc53/zenofs/internal/vfs"
)

// minSharePasswordLen 是加密 Share 口令的最小长度（与用户密码保持一致）。
const minSharePasswordLen = 8

// shareView 是 Share 的对外表示。
type shareView struct {
	Id          int64  `json:"id"`
	Name        string `json:"name"`
	PoolId      int64  `json:"pool_id"`
	QuotaMb     int64  `json:"quota_mb"`
	Compression int8   `json:"compression"`
	Encryption  int8   `json:"encryption"`
	CreatedBy   int64  `json:"created_by"`
	CreatedAt   int64  `json:"created_at"`
	// Encrypted 表示这个 Share 启用了加密；Unlocked 表示密钥此刻是否在进程内存里
	// （LUKS 式的"已打开"状态，重启或 lock 之后会变回 false）。
	//
	// 注意 Unlocked 只对加密 Share 有意义：**未加密的 Share 同样返回 false**（它没有密钥），
	// 所以客户端判断"能不能写"要用 `encrypted && !unlocked`，不能只看 unlocked。
	Encrypted bool `json:"encrypted"`
	Unlocked  bool `json:"unlocked"`
	// Permission 是**当前登录用户**在这个 Share 上的权限（管理员对没授权的 Share 为空）。
	Permission string `json:"permission,omitempty"`
}

// viewOfShare 把模型转成对外表示；perm 为 nil 时不带权限字段。
// Unlocked 来自进程级密钥表，所以必须挂在 server 上。
func (s *server) viewOfShare(sh *db.Share, perm *db.SharePermission) shareView {
	v := shareView{
		Id:          sh.Id,
		Name:        sh.Name,
		PoolId:      sh.PoolId,
		QuotaMb:     sh.Quota,
		Compression: sh.Compression,
		Encryption:  sh.Encryption,
		CreatedBy:   sh.CreatedBy,
		CreatedAt:   sh.CreatedAt,
		Encrypted:   sh.Encryption != vfs.EncryptionNone,
		Unlocked:    vfs.ShareUnlocked(s.pm, sh.Id),
	}
	if perm != nil {
		v.Permission = permissionName(*perm)
	}
	return v
}

// permissionName 把权限枚举翻成可读字符串。
func permissionName(perm db.SharePermission) string {
	switch perm {
	case db.ShareRead:
		return "read"
	case db.ShareWrite:
		return "write"
	case db.ShareAdmin:
		return "admin"
	}
	return "none"
}

// parsePermission 解析权限字符串。
func parsePermission(w http.ResponseWriter, name string) (db.SharePermission, bool) {
	switch name {
	case "read":
		return db.ShareRead, true
	case "write":
		return db.ShareWrite, true
	case "admin":
		return db.ShareAdmin, true
	}
	badRequest(w, "permission 只支持 read / write / admin")
	return db.ShareRead, false
}

// ─────────────────────────────────────────────────────────────
// 路由
// ─────────────────────────────────────────────────────────────

// registerShareRoutes 注册 Share 管理与文件访问端点。
//
// 管理（创建/改/删/授权）仅管理员；读写文件要求该用户在 share_users 里有授权，
// 管理员同样如此——授权才是唯一的访问依据。
func (s *server) registerShareRoutes(r chi.Router) {
	// GET /api/shares —— 列出 Share（管理员看全部，普通用户只看自己有授权的）
	r.Get("/shares", func(w http.ResponseWriter, r *http.Request) {
		u := userOf(r)
		shares, perms, err := s.listShares(u)
		if err != nil {
			respondErr(w, err)
			return
		}
		views := make([]shareView, 0, len(shares))
		for i := range shares {
			perm, ok := perms[shares[i].Id]
			if ok {
				views = append(views, s.viewOfShare(&shares[i], &perm))
			} else {
				views = append(views, s.viewOfShare(&shares[i], nil))
			}
		}
		respondJSON(w, http.StatusOK, views)
	})

	// POST /api/shares —— 创建 Share（管理员）
	// 文件切片大小不在请求里：它取自所选池的 chunk_size_kb（见 vfs.sliceSize）。
	// 请求：{"name":"docs","pool_id":1,"quota_mb":1024,"compression":0}
	r.Post("/shares", func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r) {
			return
		}
		var body struct {
			Name        string `json:"name"`
			PoolId      int64  `json:"pool_id"`
			QuotaMb     int64  `json:"quota_mb"`
			Compression int8   `json:"compression"`
			Encryption  int8   `json:"encryption"`
			Password    string `json:"password"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		name := strings.TrimSpace(body.Name)
		if name == "" {
			badRequest(w, "name 不能为空")
			return
		}
		if _, err := s.pm.GetPool(body.PoolId); err != nil {
			respondErr(w, err)
			return
		}
		if body.Encryption != vfs.EncryptionNone && body.Password == "" {
			badRequest(w, "启用加密必须同时提供 password")
			return
		}
		if body.Compression != vfs.CompressionNone && body.Compression != vfs.CompressionZstd {
			badRequest(w, "compression 只支持 0（无）/ 1（zstd）")
			return
		}
		// 加密必须在"还是空的 Share"上一次做成（类似 LUKS 的 luksFormat）：给了 password
		// 就启用加密并立刻解锁；已有数据的 Share 不能事后加密——老 chunk 还是明文，
		// 读的时候却按密文解，数据就毁了。
		encryption := vfs.EncryptionNone
		if body.Password != "" {
			if len(body.Password) < minSharePasswordLen {
				badRequest(w, "password 至少 8 位")
				return
			}
			encryption = vfs.EncryptionAESGCM
		}
		sh := db.Share{
			Name:        name,
			PoolId:      body.PoolId,
			Quota:       body.QuotaMb,
			Compression: body.Compression,
			Encryption:  encryption,
			CreatedBy:   userOf(r).Id,
		}
		if err := s.pm.DbManager.DB.Create(&sh).Error; err != nil {
			if isUniqueViolation(err) {
				respondErr(w, errs.New(errs.ECODE_SHARE_EXIST, errs.ESTR_SHARE_EXIST,
					"share name already taken", name))
				return
			}
			respondErr(w, errs.DBQuery(err))
			return
		}
		// 创建者自动拿到 admin 权限，否则他自己都访问不了刚建的 Share
		perm := db.ShareAdmin
		if err := s.pm.DbManager.DB.Create(&db.ShareUser{
			ShareId: sh.Id, UserId: userOf(r).Id, Permission: perm,
		}).Error; err != nil {
			respondErr(w, errs.DBQuery(err))
			return
		}

		// 加密 Share：初始化口令（salt + 校验值落库）并立刻解锁，密钥进进程内存
		if encryption != vfs.EncryptionNone {
			owner := vfs.NewShareFS(s.pm, sh, userOf(r).Id, perm)
			if err := owner.SetPassword(body.Password); err != nil {
				// 口令没设成就别留半成品：这个 Share 谁都读不了
				s.pm.DbManager.DB.Where("share_id = ?", sh.Id).Delete(&db.ShareUser{})
				s.pm.DbManager.DB.Delete(&db.Share{}, sh.Id)
				respondErr(w, err)
				return
			}
			reloaded, err := s.getShare(sh.Id) // 带上刚写入的 EncryptionKeyHash
			if err != nil {
				respondErr(w, err)
				return
			}
			if err := vfs.UnlockShare(s.pm, *reloaded, body.Password); err != nil {
				respondErr(w, err)
				return
			}
			sh = *reloaded
		}
		respondJSON(w, http.StatusCreated, s.viewOfShare(&sh, &perm))
	})

	// GET /api/shares/{id} —— 查询 Share
	r.Get("/shares/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		u := userOf(r)
		sh, err := s.getShare(id)
		if err != nil {
			respondErr(w, err)
			return
		}
		perm, granted, err := s.sharePermOf(id, u.Id)
		if err != nil {
			respondErr(w, err)
			return
		}
		if !granted && u.Role != db.UserAdmin {
			respondErr(w, shareNotFound(id))
			return
		}
		if granted {
			respondJSON(w, http.StatusOK, s.viewOfShare(sh, &perm))
			return
		}
		respondJSON(w, http.StatusOK, s.viewOfShare(sh, nil))
	})

	// PUT /api/shares/{id} —— 改配额 / 切片 / 压缩（管理员）
	r.Put("/shares/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r) {
			return
		}
		id, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		if _, err := s.getShare(id); err != nil {
			respondErr(w, err)
			return
		}
		var body struct {
			QuotaMb     *int64 `json:"quota_mb"`
			Compression *int8  `json:"compression"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		updates := map[string]any{}
		if body.QuotaMb != nil {
			if *body.QuotaMb < 0 {
				badRequest(w, "quota_mb 不能为负")
				return
			}
			updates["quota"] = *body.QuotaMb
		}
		if body.Compression != nil {
			if *body.Compression != vfs.CompressionNone && *body.Compression != vfs.CompressionZstd {
				badRequest(w, "compression 只支持 0（无）/ 1（zstd）")
				return
			}
			updates["compression"] = *body.Compression
		}
		if len(updates) > 0 {
			if err := s.pm.DbManager.DB.Model(&db.Share{}).Where("id = ?", id).Updates(updates).Error; err != nil {
				respondErr(w, errs.DBQuery(err))
				return
			}
		}
		sh, err := s.getShare(id)
		if err != nil {
			respondErr(w, err)
			return
		}
		perm, granted, err := s.sharePermOf(id, userOf(r).Id)
		if err != nil {
			respondErr(w, err)
			return
		}
		if granted {
			respondJSON(w, http.StatusOK, s.viewOfShare(sh, &perm))
			return
		}
		respondJSON(w, http.StatusOK, s.viewOfShare(sh, nil))
	})

	// DELETE /api/shares/{id} —— 删除 Share（管理员）
	// 里面有未删除的文件时返回 409；?force=1 时连同元数据一起删掉。
	r.Delete("/shares/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r) {
			return
		}
		id, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		if _, err := s.getShare(id); err != nil {
			respondErr(w, err)
			return
		}
		force := r.URL.Query().Get("force") == "1"

		var live int64
		if err := s.pm.DbManager.DB.Model(&db.Inode{}).
			Where("share_id = ? AND deleted = 0", id).Count(&live).Error; err != nil {
			respondErr(w, errs.DBQuery(err))
			return
		}
		if live > 0 && !force {
			badRequest(w, "Share 里还有文件，先清空或加 ?force=1 一并删除元数据")
			return
		}
		if err := s.deleteShareMeta(id); err != nil {
			respondErr(w, err)
			return
		}
		vfs.LockShare(s.pm, id) // Share 没了，内存里的密钥也没必要留着
		respondJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	})

	// GET /api/shares/{id}/users —— 列出授权（管理员）
	r.Get("/shares/{id}/users", func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r) {
			return
		}
		id, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		if _, err := s.getShare(id); err != nil {
			respondErr(w, err)
			return
		}
		var grants []db.ShareUser
		if err := s.pm.DbManager.DB.Where("share_id = ?", id).Order("user_id").Find(&grants).Error; err != nil {
			respondErr(w, errs.DBQuery(err))
			return
		}
		users, err := s.auth.ListUsers()
		if err != nil {
			respondErr(w, err)
			return
		}
		names := make(map[int64]string, len(users))
		for _, u := range users {
			names[u.Id] = u.Username
		}
		out := make([]map[string]any, 0, len(grants))
		for _, g := range grants {
			out = append(out, map[string]any{
				"user_id":    g.UserId,
				"username":   names[g.UserId],
				"permission": permissionName(g.Permission),
			})
		}
		respondJSON(w, http.StatusOK, out)
	})

	// POST /api/shares/{id}/users —— 授权（管理员）
	// 请求：{"user_id":2,"permission":"write"}
	r.Post("/shares/{id}/users", func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r) {
			return
		}
		id, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		if _, err := s.getShare(id); err != nil {
			respondErr(w, err)
			return
		}
		var body struct {
			UserId     int64  `json:"user_id"`
			Permission string `json:"permission"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		if _, err := s.auth.GetUser(body.UserId); err != nil {
			respondErr(w, err)
			return
		}
		perm, ok := parsePermission(w, body.Permission)
		if !ok {
			return
		}
		// 同一 (share, user) 只保留一条授权：先删再建
		if err := s.pm.DbManager.Tx(func(tx *gorm.DB) error {
			if err := tx.Where("share_id = ? AND user_id = ?", id, body.UserId).
				Delete(&db.ShareUser{}).Error; err != nil {
				return errs.DBQuery(err)
			}
			if err := tx.Create(&db.ShareUser{
				ShareId: id, UserId: body.UserId, Permission: perm,
			}).Error; err != nil {
				return errs.DBQuery(err)
			}
			return nil
		}); err != nil {
			respondErr(w, err)
			return
		}
		respondJSON(w, http.StatusOK, map[string]any{
			"user_id":    body.UserId,
			"permission": permissionName(perm),
		})
	})

	// DELETE /api/shares/{id}/users/{userId} —— 撤销授权（管理员）
	r.Delete("/shares/{id}/users/{userId}", func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r) {
			return
		}
		id, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		userId, ok := pathID(w, r, "userId")
		if !ok {
			return
		}
		res := s.pm.DbManager.DB.Where("share_id = ? AND user_id = ?", id, userId).Delete(&db.ShareUser{})
		if res.Error != nil {
			respondErr(w, errs.DBQuery(res.Error))
			return
		}
		if res.RowsAffected == 0 {
			respondErr(w, errs.New(errs.ECODE_USER_NOT_FOUND, errs.ESTR_USER_NOT_FOUND,
				"user has no grant on this share", strconv.FormatInt(userId, 10)))
			return
		}
		respondJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
	})

	// GET /api/shares/{id}/usage —— 容量与用量（前端画配额进度条用）
	r.Get("/shares/{id}/usage", func(w http.ResponseWriter, r *http.Request) {
		shareId, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		fs, sh, err := s.shareFS(userOf(r).Id, shareId, false)
		if err != nil {
			respondErr(w, err)
			return
		}
		info, err := fs.StatFS(r.Context(), "/")
		if err != nil {
			respondErr(w, err)
			return
		}
		respondJSON(w, http.StatusOK, map[string]any{
			"share_id":    sh.Id,
			"quota_mb":    sh.Quota,
			"total_bytes": info.TotalBytes,
			"used_bytes":  info.UsedBytes,
			"free_bytes":  info.FreeBytes,
			"total_files": info.TotalFiles,
			"free_files":  info.FreeFiles,
		})
	})

	// POST /api/shares/{id}/unlock —— 解密：用口令解锁，密钥只进进程内存（管理员）
	// 请求：{"password":"..."}；响应里 unlocked=true 表示当前已在内存里可用。
	r.Post("/shares/{id}/unlock", func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r) {
			return
		}
		id, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		sh, err := s.getShare(id)
		if err != nil {
			respondErr(w, err)
			return
		}
		var body struct {
			Password string `json:"password"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		// 口令错 → 403（ErrPermission），未启用加密 → 400（ErrInvalid）
		if err := vfs.UnlockShare(s.pm, *sh, body.Password); err != nil {
			respondErr(w, err)
			return
		}
		s.respondShareView(w, sh, userOf(r).Id)
	})

	// POST /api/shares/{id}/lock —— 取消解密：丢掉内存里的密钥（类似 LUKS close）
	r.Post("/shares/{id}/lock", func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r) {
			return
		}
		id, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		sh, err := s.getShare(id)
		if err != nil {
			respondErr(w, err)
			return
		}
		vfs.LockShare(s.pm, id)
		s.respondShareView(w, sh, userOf(r).Id)
	})

	s.registerFileRoutes(r)
	s.registerRecycleRoutes(r)
	s.registerHistoryRoutes(r)
}

// respondShareView 回一个 Share 视图（带上当前用户在这个 Share 上的权限，如果有）。
func (s *server) respondShareView(w http.ResponseWriter, sh *db.Share, userId int64) {
	perm, granted, err := s.sharePermOf(sh.Id, userId)
	if err != nil {
		respondErr(w, err)
		return
	}
	if granted {
		respondJSON(w, http.StatusOK, s.viewOfShare(sh, &perm))
		return
	}
	respondJSON(w, http.StatusOK, s.viewOfShare(sh, nil))
}

// ─────────────────────────────────────────────────────────────
// 内部工具
// ─────────────────────────────────────────────────────────────

// getShare 按 id 取 Share，不存在时返回 SHARE_NOT_FOUND。
func (s *server) getShare(id int64) (*db.Share, error) {
	var sh db.Share
	err := s.pm.DbManager.DB.First(&sh, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, shareNotFound(id)
	}
	if err != nil {
		return nil, errs.DBQuery(err)
	}
	return &sh, nil
}

// shareNotFound 造一个 Share 不存在的错误。
func shareNotFound(id int64) error {
	return errs.New(errs.ECODE_SHARE_NOT_FOUND, errs.ESTR_SHARE_NOT_FOUND,
		"share not found", strconv.FormatInt(id, 10))
}

// sharePermOf 取某用户在某 Share 上的权限；第二个返回值表示是否有授权记录。
func (s *server) sharePermOf(shareId, userId int64) (db.SharePermission, bool, error) {
	var grant db.ShareUser
	err := s.pm.DbManager.DB.Where("share_id = ? AND user_id = ?", shareId, userId).First(&grant).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, errs.DBQuery(err)
	}
	return grant.Permission, true, nil
}

// shareFS 取出当前用户对某个 Share 的挂载点。
//
// 没有授权时按"Share 不存在"处理（不泄露 Share 名）；管理员也不例外——
// 访问文件一律以 share_users 为准。
func (s *server) shareFS(userId, shareId int64, needWrite bool) (*vfs.ShareFS, *db.Share, error) {
	sh, err := s.getShare(shareId)
	if err != nil {
		return nil, nil, err
	}
	perm, granted, err := s.sharePermOf(shareId, userId)
	if err != nil {
		return nil, nil, err
	}
	if !granted {
		return nil, nil, shareNotFound(shareId)
	}
	if needWrite && perm < db.ShareWrite {
		return nil, nil, vfs.ErrPermission
	}
	return vfs.NewShareFS(s.pm, *sh, userId, perm), sh, nil
}

// listShares 列出某个用户能看到的 Share 及他自己的权限。
func (s *server) listShares(u *db.User) ([]db.Share, map[int64]db.SharePermission, error) {
	var grants []db.ShareUser
	if err := s.pm.DbManager.DB.Where("user_id = ?", u.Id).Find(&grants).Error; err != nil {
		return nil, nil, errs.DBQuery(err)
	}
	perms := make(map[int64]db.SharePermission, len(grants))
	ids := make([]int64, 0, len(grants))
	for _, g := range grants {
		perms[g.ShareId] = g.Permission
		ids = append(ids, g.ShareId)
	}

	var shares []db.Share
	q := s.pm.DbManager.DB.Order("id")
	if u.Role != db.UserAdmin {
		if len(ids) == 0 {
			return nil, perms, nil
		}
		q = q.Where("id IN ?", ids)
	}
	if err := q.Find(&shares).Error; err != nil {
		return nil, nil, errs.DBQuery(err)
	}
	return shares, perms, nil
}

// deleteShareMeta 删除 Share 及其全部文件元数据（inode/version/version_chunk）与授权。
//
// 存储层的 chunk 不物理删除（pool 没有单块删除能力），留给后续 GC，与回收站的
// 彻底删除保持一致。
func (s *server) deleteShareMeta(shareId int64) error {
	return s.pm.DbManager.Tx(func(tx *gorm.DB) error {
		var inodeIds, versionIds []int64
		if err := tx.Model(&db.Inode{}).Where("share_id = ?", shareId).Pluck("id", &inodeIds).Error; err != nil {
			return errs.DBQuery(err)
		}
		if len(inodeIds) > 0 {
			if err := tx.Model(&db.Version{}).Where("inode_id IN ?", inodeIds).Pluck("id", &versionIds).Error; err != nil {
				return errs.DBQuery(err)
			}
			if len(versionIds) > 0 {
				if err := tx.Where("version_id IN ?", versionIds).Delete(&db.VersionChunk{}).Error; err != nil {
					return errs.DBQuery(err)
				}
			}
			if err := tx.Where("inode_id IN ?", inodeIds).Delete(&db.Version{}).Error; err != nil {
				return errs.DBQuery(err)
			}
			if err := tx.Where("id IN ?", inodeIds).Delete(&db.Inode{}).Error; err != nil {
				return errs.DBQuery(err)
			}
			if err := tx.Where("inode_id IN ?", inodeIds).Delete(&db.InodeHistory{}).Error; err != nil {
				return errs.DBQuery(err)
			}
		}
		if err := tx.Where("share_id = ?", shareId).Delete(&db.ShareUser{}).Error; err != nil {
			return errs.DBQuery(err)
		}
		if err := tx.Delete(&db.Share{}, shareId).Error; err != nil {
			return errs.DBQuery(err)
		}
		return nil
	})
}

// isUniqueViolation 判断是否是唯一索引冲突（各驱动都给不出稳定错误码，只能看文本）。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique") || strings.Contains(msg, "duplicate")
}
