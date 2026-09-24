package vfs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
	"github.com/zzc53/zenofs/internal/testutil"
)

// TestSentinelErrorsAreStandardized 检查每个 POSIX 哨兵都是带 Code / StrCode 的标准错误：
// 协议层既可以用 errors.Is 判定后映射成协议错误码，也可以直接把 Code 透给客户端。
func TestSentinelErrorsAreStandardized(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		code    int
		strCode string
		text    string
	}{
		{"ErrNotExist", ErrNotExist, errs.ECODE_VFS_NOT_FOUND, errs.ESTR_VFS_NOT_FOUND, "vfs: no such file or directory"},
		{"ErrExist", ErrExist, errs.ECODE_VFS_EXIST, errs.ESTR_VFS_EXIST, "vfs: file exists"},
		{"ErrPermission", ErrPermission, errs.ECODE_VFS_PERMISSION, errs.ESTR_VFS_PERMISSION, "vfs: permission denied"},
		{"ErrNotEmpty", ErrNotEmpty, errs.ECODE_VFS_NOT_EMPTY, errs.ESTR_VFS_NOT_EMPTY, "vfs: directory not empty"},
		{"ErrIsDir", ErrIsDir, errs.ECODE_VFS_IS_DIR, errs.ESTR_VFS_IS_DIR, "vfs: is a directory"},
		{"ErrNotDir", ErrNotDir, errs.ECODE_VFS_NOT_DIR, errs.ESTR_VFS_NOT_DIR, "vfs: not a directory"},
		{"ErrReadOnly", ErrReadOnly, errs.ECODE_VFS_READ_ONLY, errs.ESTR_VFS_READ_ONLY, "vfs: read-only filesystem"},
		{"ErrNoSpace", ErrNoSpace, errs.ECODE_VFS_NO_SPACE, errs.ESTR_VFS_NO_SPACE, "vfs: no space left on device"},
		{"ErrInvalid", ErrInvalid, errs.ECODE_VFS_INVALID, errs.ESTR_VFS_INVALID, "vfs: invalid argument"},
		{"ErrNotSupported", ErrNotSupported, errs.ECODE_VFS_NOT_SUPPORTED, errs.ESTR_VFS_NOT_SUPPORTED, "vfs: operation not supported"},
		{"ErrBusy", ErrBusy, errs.ECODE_VFS_BUSY, errs.ESTR_VFS_BUSY, "vfs: resource busy"},
		{"ErrNameTooLong", ErrNameTooLong, errs.ECODE_VFS_NAME_TOO_LONG, errs.ESTR_VFS_NAME_TOO_LONG, "vfs: file name too long"},
		{"ErrLoop", ErrLoop, errs.ECODE_VFS_LOOP, errs.ESTR_VFS_LOOP, "vfs: too many levels of symbolic links"},
		{"ErrCrossDevice", ErrCrossDevice, errs.ECODE_VFS_CROSS_DEVICE, errs.ESTR_VFS_CROSS_DEVICE, "vfs: cross-device link"},
		{"ErrEncrypted", ErrEncrypted, errs.ECODE_VFS_ENCRYPTED, errs.ESTR_VFS_ENCRYPTED, "vfs: share is encrypted"},
	}

	seenCode := make(map[int]string, len(tests))
	seenStr := make(map[string]int, len(tests))
	for _, tt := range tests {
		var ze *errs.ZenoError
		if !errors.As(tt.err, &ze) {
			t.Fatalf("%s: %T 不是 *errs.ZenoError", tt.name, tt.err)
		}
		if ze.Code != tt.code || ze.StrCode != tt.strCode {
			t.Fatalf("%s: Code/StrCode = %d/%s, want %d/%s",
				tt.name, ze.Code, ze.StrCode, tt.code, tt.strCode)
		}
		if ze.Message != tt.text || tt.err.Error() != tt.text {
			t.Fatalf("%s: 文本 = %q / %q, want %q", tt.name, ze.Message, tt.err.Error(), tt.text)
		}
		if prev, ok := seenCode[tt.code]; ok {
			t.Errorf("%s 与 %s 共用数值码 %d", tt.name, prev, tt.code)
		}
		if prev, ok := seenStr[tt.strCode]; ok {
			t.Errorf("%s 与 %d 共用字符串码 %s", tt.name, prev, tt.strCode)
		}
		seenCode[tt.code] = tt.name
		seenStr[tt.strCode] = tt.code

		// 包装之后仍然要能被 errors.Is 命中、被 errors.As 取回码
		wrapped := fmt.Errorf("open %q: %w", "/x", tt.err)
		if !errors.Is(wrapped, tt.err) {
			t.Fatalf("%s: 包装后 errors.Is 失败", tt.name)
		}
		var got *errs.ZenoError
		if !errors.As(wrapped, &got) || got.Code != tt.code {
			t.Fatalf("%s: 包装后 errors.As 取到的码不对: %+v", tt.name, got)
		}
	}
	// 新增哨兵时同步这个清单
	if len(tests) != 15 {
		t.Fatalf("哨兵数量 = %d, want 15", len(tests))
	}
}

// TestVFSErrorsCarryCodesFromRealOperations 从真实操作里取错误码：
// 覆盖所有"当前实现真的会返回"的哨兵（ErrReadOnly / ErrBusy 目前只是预留，没有返回点）。
func TestVFSErrorsCarryCodesFromRealOperations(t *testing.T) {
	env, pm, fs, share := newWritable(t)
	ctx := context.Background()

	writeFile(t, fs, "/f.txt", []byte("x"))
	if err := fs.Mkdir(ctx, "/dir", 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, fs, "/dir/inner.txt", []byte("y"))

	readOnly := NewShareFS(pm, fs.Share(), 1, db.ShareRead)

	// 同池再建一个 Share，用来测跨挂载点操作
	env.NewShare(share.PoolId, testutil.ShareOpts{
		Name: "s2", UserID: 1, Permission: db.ShareWrite,
	})
	root := NewRootFS(pm, 1)

	// Symlink 会把 target 解析成最终 inode，所以公开 API 造不出多跳链；
	// 直接改库造一个自指链接，验证防环兜底（maxSymlinkDepth）。
	if err := fs.Symlink(ctx, "/f.txt", "/self"); err != nil {
		t.Fatal(err)
	}
	var selfLink db.Inode
	if err := env.DB.DB.Where("share_id = ? AND name = ?", share.Id, "self").
		First(&selfLink).Error; err != nil {
		t.Fatal(err)
	}
	if err := env.DB.DB.Model(&db.Inode{}).Where("id = ?", selfLink.Id).
		Update("link_id", selfLink.Id).Error; err != nil {
		t.Fatal(err)
	}

	quotaFS, _ := newShareFS(t, env, pm, share.PoolId, testutil.ShareOpts{
		Name: "quota", UserID: 1, Permission: db.ShareWrite, QuotaMB: 1,
	})

	tests := []struct {
		name string
		run  func() error
		want int
	}{
		{"Stat 不存在", func() error { _, err := fs.Stat(ctx, "/nope"); return err }, errs.ECODE_VFS_NOT_FOUND},
		{"Open 不存在", func() error {
			_, err := fs.Open(ctx, "/nope", OpenFlags{Read: true}, 0)
			return err
		}, errs.ECODE_VFS_NOT_FOUND},
		{"相对路径", func() error { _, err := fs.Stat(ctx, "relative"); return err }, errs.ECODE_VFS_INVALID},
		{"Open 目录", func() error {
			_, err := fs.Open(ctx, "/dir", OpenFlags{Read: true}, 0)
			return err
		}, errs.ECODE_VFS_IS_DIR},
		{"对文件 ReadDir", func() error { _, err := fs.ReadDir(ctx, "/f.txt"); return err }, errs.ECODE_VFS_NOT_DIR},
		{"Mkdir 已存在", func() error { return fs.Mkdir(ctx, "/dir", 0755) }, errs.ECODE_VFS_EXIST},
		{"Remove 非空目录", func() error { return fs.Remove(ctx, "/dir") }, errs.ECODE_VFS_NOT_EMPTY},
		{"名称过长", func() error {
			return fs.Mkdir(ctx, "/"+strings.Repeat("x", maxNameLen+1), 0755)
		}, errs.ECODE_VFS_NAME_TOO_LONG},
		{"Rename 到根", func() error { return fs.Rename(ctx, "/f.txt", "/") }, errs.ECODE_VFS_INVALID},
		{"符号链接自指（防环兜底）", func() error { _, err := fs.Stat(ctx, "/self"); return err }, errs.ECODE_VFS_LOOP},
		{"只读挂载写入", func() error {
			_, err := readOnly.Create(ctx, "/new.txt", 0644)
			return err
		}, errs.ECODE_VFS_PERMISSION},
		{"只读挂载删除", func() error { return readOnly.Remove(ctx, "/f.txt") }, errs.ECODE_VFS_PERMISSION},
		{"超配额写入", func() error {
			f, err := quotaFS.Create(ctx, "/big.bin", 0644)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = f.Write(make([]byte, 2<<20))
			return err
		}, errs.ECODE_VFS_NO_SPACE},
		{"根上的结构性写操作", func() error { return root.Mkdir(ctx, "/new", 0755) }, errs.ECODE_VFS_NOT_SUPPORTED},
		{"未知编解码算法", func() error {
			bad := &ShareFS{
				pm:     pm,
				share:  db.Share{Id: share.Id, PoolId: share.PoolId, Compression: 99},
				userID: 1,
				perm:   db.ShareWrite,
			}
			return bad.requireCodec()
		}, errs.ECODE_VFS_NOT_SUPPORTED},
		{"跨 Share 改名", func() error { return root.Rename(ctx, "/s/f.txt", "/s2/f.txt") }, errs.ECODE_VFS_CROSS_DEVICE},
	}

	for _, tt := range tests {
		err := tt.run()
		if err == nil {
			t.Fatalf("%s: 期望报错，实际成功", tt.name)
		}
		var ze *errs.ZenoError
		if !errors.As(err, &ze) {
			t.Fatalf("%s: 错误 %v (%T) 没有标准化", tt.name, err, err)
		}
		if ze.Code != tt.want {
			t.Fatalf("%s: Code = %d (%s), want %d", tt.name, ze.Code, ze.StrCode, tt.want)
		}
	}
}

// TestEncryptedErrorCarriesCode 加密 Share 未提供口令时的错误也要带码（协议层据此回 403）。
func TestEncryptedErrorCarriesCode(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 1, 0, 4096)
	share := env.NewShare(p.Id, testutil.ShareOpts{
		Name: "priv", UserID: 1, Permission: db.ShareWrite, Encryption: EncryptionAESGCM,
	})
	setter := NewShareFS(pm, share, 1, db.ShareWrite)
	if err := setter.SetPassword("pw"); err != nil {
		t.Fatal(err)
	}

	// 新会话（未提供口令）
	fs := NewShareFS(pm, env.ReloadShare(share), 1, db.ShareWrite)
	_, err := fs.Create(t.Context(), "/f.txt", 0644)
	if !errors.Is(err, ErrEncrypted) {
		t.Fatalf("Create = %v, want ErrEncrypted", err)
	}
	var ze *errs.ZenoError
	if !errors.As(err, &ze) || ze.Code != errs.ECODE_VFS_ENCRYPTED {
		t.Fatalf("Code = %+v, want ECODE_VFS_ENCRYPTED", ze)
	}
}
