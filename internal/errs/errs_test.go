package errs

import (
	"errors"
	"fmt"
	"testing"
)

func TestNew(t *testing.T) {
	e := New(ECODE_CHUNK_EMPTY, ESTR_CHUNK_EMPTY, "empty data", "3")
	if e.Code != ECODE_CHUNK_EMPTY || e.StrCode != ESTR_CHUNK_EMPTY {
		t.Fatalf("码不对: %+v", e)
	}
	if e.InnerErr != nil {
		t.Fatalf("New 不该带 InnerErr: %v", e.InnerErr)
	}
	if got, want := e.Error(), "empty data: 3"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}

	// 没有上下文值时只输出消息
	if got := New(ECODE_POOL_BAD, ESTR_POOL_BAD, "bad pool", "").Error(); got != "bad pool" {
		t.Fatalf("Error() = %q, want %q", got, "bad pool")
	}
}

func TestFromError(t *testing.T) {
	inner := errors.New("disk full")
	e := FromError(inner, ECODE_FILE_WRITE, ESTR_FILE_WRITE)
	if e.Code != ECODE_FILE_WRITE {
		t.Fatalf("Code = %d", e.Code)
	}
	if !errors.Is(e.InnerErr, inner) {
		t.Fatal("InnerErr 没有保留原始错误")
	}
	// Error() 优先暴露原始错误，便于定位底层原因
	if e.Error() != "disk full" {
		t.Fatalf("Error() = %q", e.Error())
	}
}

func TestDBQuery(t *testing.T) {
	e := DBQuery(fmt.Errorf("no such table: inodes"))
	if e.Code != ECODE_DB_BAD_QUERY || e.StrCode != ESTR_DB_BAD_QUERY {
		t.Fatalf("DBQuery 码不对: %+v", e)
	}
	if e.Error() != "no such table: inodes" {
		t.Fatalf("Error() = %q", e.Error())
	}
}

func TestErrorsAsZenoError(t *testing.T) {
	var err error = New(ECODE_POOL_OFFLINE, ESTR_POOL_OFFLINE, "pool is offline", "7")

	var ze *ZenoError
	if !errors.As(err, &ze) {
		t.Fatal("errors.As 取不到 *ZenoError")
	}
	if ze.Code != ECODE_POOL_OFFLINE || ze.Value != "7" {
		t.Fatalf("取到的错误不对: %+v", ze)
	}
}

func TestUnwrap(t *testing.T) {
	inner := errors.New("boom")
	e := FromError(inner, ECODE_FILE_WRITE, ESTR_FILE_WRITE)
	// Unwrap 让调用方仍能用 errors.Is 判断底层原因
	if !errors.Is(e, inner) {
		t.Fatal("errors.Is 应该能穿透到 InnerErr")
	}

	// New 出来的错误没有 InnerErr，Unwrap 返回 nil
	if got := New(ECODE_POOL_BAD, ESTR_POOL_BAD, "x", "").Unwrap(); got != nil {
		t.Fatalf("Unwrap = %v, want nil", got)
	}

	// 多层包装：errors.As 取到最外层，errors.Is 能一路穿透
	outer := FromError(e, ECODE_DB_BAD_QUERY, ESTR_DB_BAD_QUERY)
	var ze *ZenoError
	if !errors.As(outer, &ze) || ze.Code != ECODE_DB_BAD_QUERY {
		t.Fatalf("errors.As 应取到最外层的 ZenoError: %+v", ze)
	}
	if !errors.Is(outer, inner) {
		t.Fatal("errors.Is 应该能穿过两层包装")
	}
}

func TestErrorCodesAreUnique(t *testing.T) {
	// API 响应里 Code 是给客户端判断用的，数值码与字符串码都不能重复
	pairs := []struct {
		code int
		str  string
	}{
		{ECODE_DB_BAD_DSN, ESTR_DB_BAD_DSN},
		{ECODE_DB_BAD_CONN, ESTR_DB_BAD_CONN},
		{ECODE_DB_BAD_QUERY, ESTR_DB_BAD_QUERY},
		{ECODE_POOL_BAD_NAME, ESTR_POOL_BAD_NAME},
		{ECODE_POOL_BAD, ESTR_POOL_BAD},
		{ECODE_DISK_BAD_BACKEND, ESTR_DISK_BAD_BACKEND},
		{ECODE_DISK_BAD_TYPE, ESTR_DISK_BAD_TYPE},
		{ECODE_DISK_OFFLINE, ESTR_DISK_OFFLINE},
		{ECODE_POOL_OFFLINE, ESTR_POOL_OFFLINE},
		{ECODE_CRYPTO_ERROR, ESTR_CRYPTO_ERROR},
		{ECODE_FILE_WRITE, ESTR_FILE_WRITE},
		{ECODE_CHUNK_EMPTY, ESTR_CHUNK_EMPTY},
		{ECODE_CHUNK_SIZE_EXCEED, ESTR_CHUNK_SIZE_EXCEED},
		{ECODE_CHUNK_NOT_FOUND, ESTR_CHUNK_NOT_FOUND},
		{ECODE_VFS_NOT_FOUND, ESTR_VFS_NOT_FOUND},
		{ECODE_VFS_EXIST, ESTR_VFS_EXIST},
		{ECODE_VFS_PERMISSION, ESTR_VFS_PERMISSION},
		{ECODE_VFS_NOT_EMPTY, ESTR_VFS_NOT_EMPTY},
		{ECODE_VFS_IS_DIR, ESTR_VFS_IS_DIR},
		{ECODE_VFS_NOT_DIR, ESTR_VFS_NOT_DIR},
		{ECODE_VFS_READ_ONLY, ESTR_VFS_READ_ONLY},
		{ECODE_VFS_NO_SPACE, ESTR_VFS_NO_SPACE},
		{ECODE_VFS_INVALID, ESTR_VFS_INVALID},
		{ECODE_VFS_NOT_SUPPORTED, ESTR_VFS_NOT_SUPPORTED},
		{ECODE_VFS_BUSY, ESTR_VFS_BUSY},
		{ECODE_VFS_NAME_TOO_LONG, ESTR_VFS_NAME_TOO_LONG},
		{ECODE_VFS_LOOP, ESTR_VFS_LOOP},
		{ECODE_VFS_CROSS_DEVICE, ESTR_VFS_CROSS_DEVICE},
		{ECODE_VFS_ENCRYPTED, ESTR_VFS_ENCRYPTED},
	}
	seenCode := make(map[int]string, len(pairs))
	seenStr := make(map[string]int, len(pairs))
	for _, p := range pairs {
		if prev, ok := seenCode[p.code]; ok {
			t.Errorf("数值码 %d 被 %q 与 %q 共用", p.code, prev, p.str)
		}
		if prev, ok := seenStr[p.str]; ok {
			t.Errorf("字符串码 %q 被 %d 与 %d 共用", p.str, prev, p.code)
		}
		seenCode[p.code] = p.str
		seenStr[p.str] = p.code
	}
	// 新增错误码时同步这个清单，避免测试悄悄失效
	if len(seenStr) != 29 {
		t.Fatalf("错误码数量 = %d, want 29", len(seenStr))
	}
}
