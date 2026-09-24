package smb

import (
	"strings"

	"github.com/jfjallid/go-smb/ntlmssp"
	gsmb "github.com/jfjallid/go-smb/smb"
	"github.com/jfjallid/go-smb/smb/server"
	"github.com/jfjallid/go-smb/smb/unicode"

	"github.com/zzc53/zenofs/internal/token"
)

var _ server.Authenticator = (*tokenAuthenticator)(nil)

// tokenAuthenticator 用 access_tokens 表做 NTLMv2 校验。
//
// 客户端把"用户名 + token"当账号密码发过来；服务端按用户名取出该用户
// 全部未过期 token 的 NT hash（库里只有单向摘要），逐个试算 NTLMv2 proof。
// 命中即认证通过，并把"登录名 → 用户 id"记进缓存，供 VFS 层挂载鉴权。
type tokenAuthenticator struct {
	tokens *token.Manager
	users  *userCache
}

// Verify 实现 server.Authenticator。
//
// 与 go-smb 的 MapAuthenticator 的区别只在于"表从哪来"：它是一张静态的
// 用户→NT hash 表，这里换成 access_tokens 的实时查询（带过期判断），
// 并且一个用户可能有多条 token，所以逐条试算。
func (a *tokenAuthenticator) Verify(c *server.Conn, auth *ntlmssp.Authenticate, serverChallenge [8]byte) ([]byte, uint32) {
	if auth == nil {
		return nil, gsmb.StatusLogonFailure
	}
	username, err := unicode.FromUnicodeString(auth.UserName)
	if err != nil || strings.TrimSpace(username) == "" {
		return nil, gsmb.StatusLogonFailure
	}

	userID, hashes, err := a.tokens.SMBHashes(username)
	if err != nil || len(hashes) == 0 {
		// 用户不存在、没有任何 token、或 token 全部过期——一律按登录失败处理，
		// 不区分具体原因，避免向客户端暴露用户名是否存在。
		return nil, gsmb.StatusLogonFailure
	}

	key := strings.ToLower(username)
	for _, ntHash := range hashes {
		m := &server.MapAuthenticator{
			Accounts: map[string]*server.Account{key: {NTHash: ntHash}},
		}
		sessionKey, status := m.Verify(c, auth, serverChallenge)
		if status == gsmb.StatusOk {
			a.users.remember(username, userID)
			return sessionKey, gsmb.StatusOk
		}
	}
	return nil, gsmb.StatusLogonFailure
}
