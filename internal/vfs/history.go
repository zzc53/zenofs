package vfs

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
)

// VersionEntry 是一个文件版本（给"版本历史"界面用）。
type VersionEntry struct {
	Id          int64
	Size        int64
	Hash        string
	Compression int8
	Encryption  int8
	CreatedBy   int64
	CreatedAt   int64
	IsCurrent   bool // 是否是该文件当前的版本
}

// HistoryEntry 是一条元数据变更记录（创建 / 改名 / 移动 / 删除 / 恢复）。
//
// 改名和移动共用一个事件结构：OldName/NewName 描述名字，Old/NewParentId 描述位置，
// 移动事件里名字通常不变、改名事件里父目录通常不变。
type HistoryEntry struct {
	Id        int64
	EventType db.InodeEventType
	OldName   string
	NewName   string

	// 新旧父目录用**路径**而不是 inode id：id 对用户没有意义。
	// 父目录自己也被删掉时路径重建不出来，那时留空。
	OldParentPath string
	NewParentPath string

	CreatedAt int64
}

// inodeInShare 取出属于本 Share 的 inode。
//
// 与 deletedInode 的区别：这里**允许已软删除**的 inode——回收站里也要能看历史和版本。
func (fs *ShareFS) inodeInShare(inodeId int64) (db.Inode, error) {
	var in db.Inode
	err := fs.pm.DbManager.DB.Where("id = ? AND share_id = ?", inodeId, fs.share.Id).First(&in).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return db.Inode{}, ErrNotExist
	}
	if err != nil {
		return db.Inode{}, errs.DBQuery(err)
	}
	return in, nil
}

// ListVersions 列出某个 inode 的所有版本，最新的在前。
//
// 每次写入都会生成一个新版本（见 fileHandle.Close），所以这里列出的就是内容的
// 变更历史。旧版本的 chunk 一直留在存储池里，恢复只是把 Inode 的当前版本指回去。
func (fs *ShareFS) ListVersions(_ context.Context, inodeId int64) ([]VersionEntry, error) {
	in, err := fs.inodeInShare(inodeId)
	if err != nil {
		return nil, err
	}

	var vs []db.Version
	if err := fs.pm.DbManager.DB.Where("inode_id = ?", inodeId).Order("id DESC").Find(&vs).Error; err != nil {
		return nil, errs.DBQuery(err)
	}

	var current int64
	if in.VersionId.Valid {
		current = in.VersionId.Int64
	}
	out := make([]VersionEntry, 0, len(vs))
	for _, v := range vs {
		out = append(out, VersionEntry{
			Id:          v.Id,
			Size:        v.Size,
			Hash:        v.Hash,
			Compression: v.Compression,
			Encryption:  v.Encryption,
			CreatedBy:   v.CreatedBy,
			CreatedAt:   v.CreatedAt,
			IsCurrent:   v.Id == current,
		})
	}
	return out, nil
}

// RestoreVersion 把某个历史版本重新设为该文件的当前版本，返回恢复后的元数据。
//
// 需要写权限。这一步只改 Inode 上的版本指针，不搬运任何 chunk：旧版本的数据一直
// 在（版本只增不减），所以"恢复"是即时的。
func (fs *ShareFS) RestoreVersion(_ context.Context, versionId int64) (FileInfo, error) {
	if err := fs.requireWrite(); err != nil {
		return FileInfo{}, err
	}

	var ver db.Version
	err := fs.pm.DbManager.DB.First(&ver, versionId).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return FileInfo{}, ErrNotExist
	}
	if err != nil {
		return FileInfo{}, errs.DBQuery(err)
	}

	in, err := fs.inodeInShare(ver.InodeId)
	if err != nil {
		return FileInfo{}, err
	}
	if in.Kind != db.InodeFile {
		return FileInfo{}, ErrInvalid
	}
	if in.Deleted != 0 {
		// 先把它从回收站还原出来，再谈恢复某个版本，免得绕开回收站那套冲突检查
		return FileInfo{}, ErrInvalid
	}

	now := time.Now().Unix()
	if err := fs.pm.DbManager.Tx(func(tx *gorm.DB) error {
		return wrapDB(tx.Model(&db.Inode{}).Where("id = ?", in.Id).Updates(map[string]any{
			"version_id": ver.Id,
			"updated_by": fs.userID,
			"updated_at": now,
		}).Error)
	}); err != nil {
		return FileInfo{}, err
	}

	in.VersionId = sql.NullInt64{Int64: ver.Id, Valid: true}
	in.UpdatedAt = now
	p, err := fs.pathOf(in.Id)
	if err != nil {
		p = "/" + in.Name
	}
	return fs.buildInfo(in, p, ver.Size), nil
}

// ListHistory 列出某个 inode 的元数据变更记录，最新的在前。
//
// 已软删除的 inode 也能查：回收站里要能看到它当初被改名/移动/删除的经过。
func (fs *ShareFS) ListHistory(_ context.Context, inodeId int64) ([]HistoryEntry, error) {
	if _, err := fs.inodeInShare(inodeId); err != nil {
		return nil, err
	}

	var rows []db.InodeHistory
	if err := fs.pm.DbManager.DB.Where("inode_id = ?", inodeId).
		Order("id DESC").Find(&rows).Error; err != nil {
		return nil, errs.DBQuery(err)
	}

	// 同一批记录里父目录高度重复，缓存一下省掉重复的逐级回溯
	pathCache := make(map[int64]string)
	pathOf := func(parent sql.NullInt64) string {
		if !parent.Valid || parent.Int64 == 0 {
			return "/"
		}
		if p, ok := pathCache[parent.Int64]; ok {
			return p
		}
		p, err := fs.pathOf(parent.Int64)
		if err != nil {
			p = "" // 父目录也没了，路径重建不出来
		}
		pathCache[parent.Int64] = p
		return p
	}

	out := make([]HistoryEntry, 0, len(rows))
	for _, h := range rows {
		out = append(out, HistoryEntry{
			Id:            h.Id,
			EventType:     h.EventType,
			OldName:       h.OldName.String,
			NewName:       h.NewName.String,
			OldParentPath: pathOf(h.OldParentId),
			NewParentPath: pathOf(h.NewParentId),
			CreatedAt:     h.CreatedAt,
		})
	}
	return out, nil
}
