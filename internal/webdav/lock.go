package webdav

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"golang.org/x/net/webdav"
)

var _ webdav.LockSystem = (*noLock)(nil)

// noLock 是"不真的上锁"的 LockSystem。
//
// zenofs 的 vfs 不提供锁（设计如此），这里按约定让 LOCK/UNLOCK 一律成功：
// Office、Finder 这类客户端要求 LOCK 成功才肯写入，所以必须给出一个合法的
// lock token（RFC 4918 要求是 opaquelocktoken URI）；但服务端不做互斥，
// 同一个文件可以被多个客户端并发写——与 SMB/SFTP 的行为保持一致。
//
// 仍然记一份 token → LockDetails，只为把 LOCK 响应里的 lockdiscovery
// （href / owner / timeout）填对，不参与任何准入判断，也不影响任何请求的成败。
// 进程重启后这些记录会丢，但 Refresh/Unlock 对未知 token 同样返回成功，
// 客户端手里的旧 token 依然能用。
type noLock struct {
	mu    sync.Mutex
	locks map[string]webdav.LockDetails
}

func newNoLock() *noLock {
	return &noLock{locks: make(map[string]webdav.LockDetails)}
}

// Confirm 永远放行：不校验任何锁条件（包括 If 头里的 token 与 ETag）。
// x/net 在每个写请求前都会调它，放行意味着"不因为锁拒绝任何请求"。
func (l *noLock) Confirm(time.Time, string, string, ...webdav.Condition) (func(), error) {
	return func() {}, nil
}

// Create 总是成功，返回一个新的 opaquelocktoken。
func (l *noLock) Create(_ time.Time, details webdav.LockDetails) (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := "opaquelocktoken:" + hex.EncodeToString(buf)
	l.mu.Lock()
	l.locks[token] = details
	l.mu.Unlock()
	return token, nil
}

// Refresh 续期。未知 token 也返回成功（服务端不维护真实的锁状态）。
func (l *noLock) Refresh(_ time.Time, token string, duration time.Duration) (webdav.LockDetails, error) {
	l.mu.Lock()
	details, ok := l.locks[token]
	l.mu.Unlock()
	if !ok {
		return webdav.LockDetails{Duration: duration}, nil
	}
	if duration != 0 {
		details.Duration = duration
	}
	return details, nil
}

// Unlock 总是成功（包括未知 token）。
func (l *noLock) Unlock(_ time.Time, token string) error {
	l.mu.Lock()
	delete(l.locks, token)
	l.mu.Unlock()
	return nil
}
