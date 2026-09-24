package pool

import "sync"

// Keyring 是进程级的 Share 解锁密钥表：**只在内存里，永不落库**。
//
// 语义跟 LUKS 的 open/close 一致：管理员用口令 unlock 一次，派生出的密钥就留在
// 进程内存里，所有协议（HTTP API / WebDAV / SMB / SFTP）共用同一份；lock 或者进程
// 重启之后密钥就没了，需要重新 unlock。落库的只有"这个 Share 启用了加密 + 口令校验值"，
// 密钥本身不持久化——这是刻意的。
//
// 并发安全；nil 接收者视为"没有密钥表"（Get 返回 false），方便零值 PoolManager。
type Keyring struct {
	mu   sync.RWMutex
	keys map[int64][]byte
}

// NewKeyring 创建空的密钥表。
func NewKeyring() *Keyring {
	return &Keyring{keys: make(map[int64][]byte)}
}

// Set 放入（或覆盖）某个 Share 的密钥。内部会拷贝一份，
// 避免调用方后续改动影响密钥表里的内容。
func (k *Keyring) Set(shareID int64, key []byte) {
	if k == nil {
		return
	}
	cp := append([]byte(nil), key...)
	k.mu.Lock()
	k.keys[shareID] = cp
	k.mu.Unlock()
}

// Get 取出某个 Share 的密钥。返回的切片归密钥表所有，调用方只读。
func (k *Keyring) Get(shareID int64) ([]byte, bool) {
	if k == nil {
		return nil, false
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	key, ok := k.keys[shareID]
	return key, ok
}

// Delete 丢弃某个 Share 的密钥，并把内存里的字节清零。
func (k *Keyring) Delete(shareID int64) {
	if k == nil {
		return
	}
	k.mu.Lock()
	if key, ok := k.keys[shareID]; ok {
		for i := range key {
			key[i] = 0
		}
		delete(k.keys, shareID)
	}
	k.mu.Unlock()
}

// IDs 返回当前已解锁的 Share id（顺序不保证）。
func (k *Keyring) IDs() []int64 {
	if k == nil {
		return nil
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	out := make([]int64, 0, len(k.keys))
	for id := range k.keys {
		out = append(out, id)
	}
	return out
}
