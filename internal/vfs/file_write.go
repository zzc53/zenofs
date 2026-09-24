package vfs

import (
	"context"
	"database/sql"
	"time"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// beginWrite 为本次写入创建新版本记录（Version 本身的大小/哈希留到 Close 再回填）。
//
// 关键一步：把打开时版本的全部切片映射复制到新版本下。写时复制只重建被写到的
// Idx，其余 Idx 直接沿用旧 chunk——多个版本共享同一个 chunk 是安全的，
// 因为 chunk 不可变且目前不做回收。
func (f *fileHandle) beginWrite(_ context.Context) error {
	v := f.newVersionRecord(f.oldVer.Size)
	if err := f.fs.pm.DbManager.DB.Create(&v).Error; err != nil {
		return errs.DBQuery(err)
	}
	f.newVer = v
	f.newChunks = make(map[int64]db.VersionChunk, len(f.oldChunks))

	if len(f.oldChunks) == 0 {
		return nil
	}
	inherit := make([]db.VersionChunk, 0, len(f.oldChunks))
	for _, vc := range f.oldChunks {
		inherit = append(inherit, db.VersionChunk{
			VersionId: v.Id,
			Idx:       vc.Idx,
			ChunkId:   vc.ChunkId,
			Size:      vc.Size,
			Hash:      vc.Hash,
		})
	}
	if err := f.fs.pm.DbManager.DB.Create(&inherit).Error; err != nil {
		return errs.DBQuery(err)
	}
	for _, vc := range inherit {
		f.newChunks[vc.Idx] = vc
	}
	return nil
}

// checkQuota 判断再增长 grow 字节是否会超出 Share 配额。
// 已用量按"所有 inode 当前版本大小之和"计算（软删除的也计入）。
func (f *fileHandle) checkQuota(grow int64) error {
	if f.fs.share.Quota <= 0 || grow <= 0 {
		return nil
	}
	used, err := f.fs.usedBytes()
	if err != nil {
		return err
	}
	if used+grow > f.fs.share.Quota<<20 {
		return ErrNoSpace
	}
	return nil
}

// ---------------------------------------------------------------
// 写入
// ---------------------------------------------------------------

// Write 从当前偏移写入；Append 模式下每次写入前先移到末尾。
func (f *fileHandle) Write(p []byte) (int, error) {
	if f.append {
		f.offset = f.size()
	}
	n, err := f.WriteAt(p, f.offset)
	f.offset += int64(n)
	return n, err
}

// WriteAt 在指定偏移写入，按切片粒度做写时复制。
// 落在切片中间的写会先取回该切片的现有内容，改完再整体写成一个新 chunk。
func (f *fileHandle) WriteAt(p []byte, off int64) (int, error) {
	if !f.writable {
		return 0, ErrPermission
	}
	if off < 0 {
		return 0, ErrInvalid
	}
	if len(p) == 0 {
		return 0, nil
	}
	grow := off + int64(len(p)) - f.newVer.Size
	if err := f.checkQuota(grow); err != nil {
		return 0, err
	}

	slice := f.fs.sliceSize()
	written := 0
	for len(p) > 0 {
		idx := off / slice
		intra := off % slice
		n := int64(len(p))
		if remain := slice - intra; n > remain {
			n = remain // 一次只处理一个切片
		}
		if err := f.writeIntoSlice(idx, intra, p[:n]); err != nil {
			return written, err
		}
		off += n
		written += int(n)
		p = p[n:]
	}
	if off > f.newVer.Size {
		f.newVer.Size = off // 逻辑大小先记在内存，Close 时回填数据库
	}
	f.dirty = true
	return written, nil
}

// writeIntoSlice 把一段数据写进某个切片。
//
// 该切片会被提升为"活动缓冲"：同一 Idx 上的后续写入直接改内存里的这份明文，
// 切换到别的 Idx（或 Close/Sync）时才把上一份落盘，避免同一切片反复读回存储层。
func (f *fileHandle) writeIntoSlice(idx, intra int64, data []byte) error {
	if !f.hasActive || f.activeIdx != idx {
		buf, err := f.slicePlain(idx)
		if err != nil {
			return err
		}
		if err := f.flushActive(); err != nil {
			return err
		}
		f.activeIdx, f.activeBuf, f.hasActive = idx, buf, true
	}

	end := intra + int64(len(data))
	if end > int64(len(f.activeBuf)) {
		grown := make([]byte, end)
		copy(grown, f.activeBuf)
		f.activeBuf = grown
	}
	copy(f.activeBuf[intra:end], data)
	return nil
}

// flushActive 把活动缓冲落盘（生成新 chunk + 同步写 VersionChunk 记录）。
func (f *fileHandle) flushActive() error {
	if !f.hasActive {
		return nil
	}
	idx, buf := f.activeIdx, f.activeBuf
	f.hasActive, f.activeBuf = false, nil
	return f.putSlice(idx, buf)
}

// putSlice 把一个切片的明文写进存储池，并 upsert 对应的 VersionChunk。
// 全零切片等价于空洞，直接删记录，不占存储。
func (f *fileHandle) putSlice(idx int64, plain []byte) error {
	if isAllZero(plain) {
		return f.dropSlice(idx)
	}
	stored, err := f.fs.encodeSlice(plain, f.newVer.Compression, f.newVer.Encryption)
	if err != nil {
		return err
	}
	chunks, err := f.fs.pm.AddChunks(f.fs.share.PoolId, [][]byte{stored})
	if err != nil {
		return err
	}

	vc := db.VersionChunk{
		VersionId: f.newVer.Id,
		Idx:       idx,
		ChunkId:   chunks[0].Id,
		Size:      int64(len(plain)), // 明文长度；存储层的字节数另算
		Hash:      chunks[0].Hash,
	}
	if err := f.fs.pm.DbManager.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "version_id"}, {Name: "idx"}},
		DoUpdates: clause.AssignmentColumns([]string{"chunk_id", "size", "hash"}),
	}).Create(&vc).Error; err != nil {
		return errs.DBQuery(err)
	}
	f.newChunks[idx] = vc
	return nil
}

// dropSlice 把一个切片退回空洞状态（删除本版本的切片记录）。
func (f *fileHandle) dropSlice(idx int64) error {
	delete(f.newChunks, idx)
	return wrapDB(f.fs.pm.DbManager.DB.
		Where("version_id = ? AND idx = ?", f.newVer.Id, idx).
		Delete(&db.VersionChunk{}).Error)
}

// ---------------------------------------------------------------
// 截断
// ---------------------------------------------------------------

// Truncate 调整文件大小（接口方法）。
func (f *fileHandle) Truncate(size int64) error { return f.truncateTo(size) }

// truncateTo 把文件调整到指定大小。
// 缩小会丢弃尾部切片，并把截断点所在的切片裁到截断点（见 trimSlice）；
// 扩大不写任何数据，中间自然形成稀疏空洞。
func (f *fileHandle) truncateTo(size int64) error {
	if !f.writable {
		return ErrPermission
	}
	if size < 0 {
		return ErrInvalid
	}
	if size == f.newVer.Size {
		return nil
	}
	if err := f.checkQuota(size - f.newVer.Size); err != nil {
		return err
	}

	if size < f.newVer.Size {
		slice := f.fs.sliceSize()
		lastKept := (size + slice - 1) / slice // 从这个 Idx 起整片丢弃
		intra := size % slice                  // 截断点在边界切片内的位置；0 表示正好落在切片边界

		// 活动切片：整个落在截断点之后就丢掉，跨过截断点就只留前半段。
		// intra == 0 时活动切片正好是被完整保留的那一片，不能动。
		switch {
		case f.hasActive && f.activeIdx >= lastKept:
			f.hasActive, f.activeBuf = false, nil
		case f.hasActive && intra != 0 && f.activeIdx == size/slice && intra < int64(len(f.activeBuf)):
			f.activeBuf = f.activeBuf[:intra]
		}
		for idx := range f.newChunks {
			if idx >= lastKept {
				delete(f.newChunks, idx)
			}
		}
		if err := wrapDB(f.fs.pm.DbManager.DB.
			Where("version_id = ? AND idx >= ?", f.newVer.Id, lastKept).
			Delete(&db.VersionChunk{}).Error); err != nil {
			return err
		}
		// 截断点落在切片内部：已落盘的边界切片也要裁掉尾部，
		// 否则以后再把文件扩大会把被截掉的数据读回来（POSIX 要求新区域读作零）。
		if intra != 0 {
			if err := f.trimSlice(size/slice, intra); err != nil {
				return err
			}
		}
	}

	f.newVer.Size = size
	f.dirty = true
	return nil
}

// trimSlice 把某个切片的明文裁到 size 字节并重新落盘（生成新的 chunk）。
//
// 只在截断点落在切片内部时调用：被截掉的那半段必须真的从存储层消失，
// 否则后来扩大文件会把旧数据读回来。
func (f *fileHandle) trimSlice(idx, size int64) error {
	if f.hasActive && f.activeIdx == idx {
		// 活动缓冲是这份内容的唯一副本，直接在内存里截断即可
		if int64(len(f.activeBuf)) > size {
			f.activeBuf = f.activeBuf[:size]
		}
		return nil
	}
	buf, err := f.slicePlain(idx)
	if err != nil {
		return err
	}
	if buf == nil || int64(len(buf)) <= size {
		return nil // 空洞，或本来就短于截断点（不含被截掉的数据）
	}
	// 拷一份再落盘：putSlice 之后 buf 仍可能被这个句柄复用
	return f.putSlice(idx, append([]byte(nil), buf[:size]...))
}

// ---------------------------------------------------------------
// 提交与同步
// ---------------------------------------------------------------

// Sync 把活动切片落盘（SMB FLUSH / SFTP FSYNC）。
// 注意新切片本来就是实时落盘的，这里只需把尚未落盘的活动缓冲刷出去。
func (f *fileHandle) Sync() error {
	if !f.writable {
		return nil
	}
	return f.flushActive()
}

// Close 提交本次写入：落盘活动缓冲、回填版本大小、把 Inode 的当前版本切过去。
// 打开后一个字节都没写时，丢弃这个空版本，Inode 保持原版本。
func (f *fileHandle) Close() error {
	if f.closed {
		return nil
	}
	f.closed = true
	if !f.writable {
		return nil
	}
	if err := f.flushActive(); err != nil {
		return err
	}
	if !f.dirty {
		return f.discardNewVersion()
	}

	now := time.Now().Unix()
	return f.fs.pm.DbManager.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&db.Version{}).Where("id = ?", f.newVer.Id).
			Updates(map[string]any{"size": f.newVer.Size}).Error; err != nil {
			return errs.DBQuery(err)
		}
		return wrapDB(tx.Model(&db.Inode{}).Where("id = ?", f.inode.Id).
			Updates(map[string]any{
				"version_id": f.newVer.Id,
				"updated_by": f.fs.userID,
				"updated_at": now,
			}).Error)
	})
}

// discardNewVersion 丢弃没有产生任何写入的空版本。
func (f *fileHandle) discardNewVersion() error {
	if f.newVer.Id == 0 {
		return nil
	}
	return wrapDB(f.fs.pm.DbManager.DB.
		Where("id = ?", f.newVer.Id).Delete(&db.Version{}).Error)
}

// newVersionRecord 组装新版本行；版本链指向打开时的版本。
func (f *fileHandle) newVersionRecord(size int64) db.Version {
	v := db.Version{
		InodeId:     f.inode.Id,
		Size:        size,
		Encryption:  f.fs.share.Encryption,
		Compression: f.fs.share.Compression,
		CreatedBy:   f.fs.userID,
	}
	if f.oldVer.Id != 0 {
		v.ParentId = sql.NullInt64{Int64: f.oldVer.Id, Valid: true}
	}
	return v
}

// isAllZero 判断切片是否全零——全零等价于空洞，不占存储。
func isAllZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}
