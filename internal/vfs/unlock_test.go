package vfs

import (
	"context"
	"errors"
	"testing"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/pool"
	"github.com/zzc53/zenofs/internal/testutil"
)

// newEncrypted 建一个启用了加密的 Share 并设好口令，返回带密钥的挂载实例。
func newEncrypted(t *testing.T, password string) (*pool.PoolManager, db.Share, *ShareFS) {
	t.Helper()
	env, pm := newEnv(t)
	p := env.NewPool("p", 2, 1, 8192)
	share := env.NewShare(p.Id, testutil.ShareOpts{
		Name: "enc", UserID: 1, Permission: db.ShareWrite, Encryption: EncryptionAESGCM,
	})
	fs := NewShareFS(pm, share, 1, db.ShareWrite)
	if err := fs.SetPassword(password); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	return pm, env.ReloadShare(share), fs
}

// TestKeyringUnlockLock 覆盖 LUKS 式的进程级解锁：解锁后新挂载实例（模拟别的会话/协议）
// 也能读到密文，上锁后立刻读不了。
func TestKeyringUnlockLock(t *testing.T) {
	pm, share, fs := newEncrypted(t, "pw-123456")
	ctx := t.Context()
	payload := []byte("encrypted content")

	// 写入（这个实例持有会话密钥）
	if err := writeFileErr(fs, "/secret.txt", payload); err != nil {
		t.Fatalf("写入: %v", err)
	}

	// 另一个挂载实例（没有会话密钥、也没解锁）读不了
	fresh := NewShareFS(pm, share, 1, db.ShareWrite)
	if _, err := fresh.Open(ctx, "/secret.txt", OpenFlags{Read: true}, 0); !errors.Is(err, ErrEncrypted) {
		t.Fatalf("未解锁时读 err = %v，期望 ErrEncrypted", err)
	}
	if ShareUnlocked(pm, share.Id) {
		t.Fatalf("还没解锁，ShareUnlocked 应为 false")
	}

	// 口令错 → ErrPermission，且不会把密钥放进密钥表
	if err := UnlockShare(pm, share, "wrong-pw"); !errors.Is(err, ErrPermission) {
		t.Fatalf("错误口令 err = %v，期望 ErrPermission", err)
	}
	if ShareUnlocked(pm, share.Id) {
		t.Fatalf("口令错时不该解锁")
	}

	// 正确口令 → 解锁，另一个实例立刻可读
	if err := UnlockShare(pm, share, "pw-123456"); err != nil {
		t.Fatalf("UnlockShare: %v", err)
	}
	if !ShareUnlocked(pm, share.Id) {
		t.Fatalf("解锁后 ShareUnlocked 应为 true")
	}
	if got := readFile(t, fresh, "/secret.txt", int64(len(payload))); string(got) != string(payload) {
		t.Fatalf("解锁后读回 = %q，期望 %q", got, payload)
	}

	// 上锁 → 密钥从内存消失，再读就失败
	LockShare(pm, share.Id)
	if ShareUnlocked(pm, share.Id) {
		t.Fatalf("上锁后 ShareUnlocked 应为 false")
	}
	again := NewShareFS(pm, share, 1, db.ShareWrite)
	if _, err := again.Open(ctx, "/secret.txt", OpenFlags{Read: true}, 0); !errors.Is(err, ErrEncrypted) {
		t.Fatalf("上锁后读 err = %v，期望 ErrEncrypted", err)
	}

	// 重新解锁还能读（数据没坏）
	if err := UnlockShare(pm, share, "pw-123456"); err != nil {
		t.Fatalf("再次 UnlockShare: %v", err)
	}
	if got := readFile(t, NewShareFS(pm, share, 1, db.ShareWrite), "/secret.txt", int64(len(payload))); string(got) != string(payload) {
		t.Fatalf("重新解锁后读回 = %q", got)
	}
}

func TestUnlockRejectsPlainShareAndMissingPassword(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 2, 1, 8192)

	// 没启用加密的 Share：解锁没有意义
	plain := env.NewShare(p.Id, testutil.ShareOpts{Name: "plain", UserID: 1, Permission: db.ShareWrite})
	if err := UnlockShare(pm, plain, "whatever"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("未加密 Share 解锁 err = %v，期望 ErrInvalid", err)
	}

	// 启用了加密但还没设置过口令：同样 ErrInvalid
	blank := env.NewShare(p.Id, testutil.ShareOpts{
		Name: "blank", UserID: 1, Permission: db.ShareWrite, Encryption: EncryptionAESGCM,
	})
	if err := UnlockShare(pm, blank, "pw-123456"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("未设口令解锁 err = %v，期望 ErrInvalid", err)
	}
}

// 锁上后写也要失败（不只是读），否则会出现"密钥没了却还在加密写"的坏数据。
func TestLockedShareRejectsWrite(t *testing.T) {
	pm, share, _ := newEncrypted(t, "pw-123456")
	ctx := t.Context()

	LockShare(pm, share.Id)
	fs := NewShareFS(pm, share, 1, db.ShareWrite)
	if _, err := fs.Open(ctx, "/new.txt", OpenFlags{Write: true, Create: true}, 0o644); !errors.Is(err, ErrEncrypted) {
		t.Fatalf("上锁后写入 err = %v，期望 ErrEncrypted", err)
	}
}

// writeFileErr 是同包 helpers 里 writeFile 的可失败版本（需要断言错误）。
func writeFileErr(fs *ShareFS, path string, data []byte) error {
	f, err := fs.Create(context.Background(), path, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
