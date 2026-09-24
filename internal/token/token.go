// Package token 管理协议访问凭证（access token）。
//
// 一条凭证（db.AccessToken）要么是随机 token，要么是 SSH 公钥：
//
//	token —— 明文只在创建时返回一次，库里只留两份单向摘要：
//	         BLAKE3-256(token) 供"客户端把 token 原样交上来比对"的协议
//	         （SFTP 密码、WebDAV、HTTP API）；
//	         MD4(UTF-16LE(token))（即 MS-NLMP 的 NTOWFv1）供 SMB 的 NTLMv2 校验——
//	         NTLM 的 HMAC-MD5 密钥必须就是它，无法用别的摘要替代。
//	         两份摘要都不可逆，而 token 是 32 字节随机串，所以即便库被拖走，
//	         也无法还原明文或离线爆破。
//
//	公钥 —— 不是秘密（随私钥持有者公开），明文落库，另记指纹
//	        （SHA256:base64）供按 key 查表与展示。
//
// 两种凭证都可以带过期时间（ExpiresAt，Unix 秒，0 表示永不过期）。
// 依赖只需 db（存）+ errs（错误码），不绑定任何协议库，SMB/SFTP/WebDAV 共用。
package token

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
	"github.com/zzc53/zenofs/internal/hash"
	"golang.org/x/crypto/md4"
	"golang.org/x/crypto/ssh"
	"gorm.io/gorm"
)

// TokenBytes 是新 token 的随机熵（字节）。256 bit 足以让落库的摘要
// 无法被离线爆破还原。
const TokenBytes = 32

// lastUsedGranularity 是 LastUsedAt 的写库节流粒度。
// 认证可能很频繁（WebDAV 每个请求一次），小于这个间隔就不重复写库。
const lastUsedGranularity = int64(60)

// 凭证相关的哨兵错误，调用方用 errors.Is 判定后映射成各自的协议错误。
var (
	// ErrInvalid 表示凭证无效（token 摘要对不上、公钥指纹对不上、或凭证不属于该用户）。
	ErrInvalid = errs.New(errs.ECODE_TOKEN_INVALID, errs.ESTR_TOKEN_INVALID, "access token: invalid", "")
	// ErrExpired 表示凭证已过期。
	ErrExpired = errs.New(errs.ECODE_TOKEN_EXPIRED, errs.ESTR_TOKEN_EXPIRED, "access token: expired", "")
	// ErrNotFound 表示凭证不存在（按 id 查询/吊销时）。
	ErrNotFound = errs.New(errs.ECODE_TOKEN_NOT_FOUND, errs.ESTR_TOKEN_NOT_FOUND, "access token: not found", "")
	// ErrBadUser 表示归属用户不存在。
	ErrBadUser = errs.New(errs.ECODE_TOKEN_BAD_USER, errs.ESTR_TOKEN_BAD_USER, "access token: unknown user", "")
	// ErrBadKey 表示 SSH 公钥格式非法。
	ErrBadKey = errs.New(errs.ECODE_TOKEN_BAD_KEY, errs.ESTR_TOKEN_BAD_KEY, "access token: bad ssh public key", "")
)

// Generate 生成一个新的随机 token 明文（base64url，无填充）。
func Generate() (string, error) {
	buf := make([]byte, TokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", errs.FromError(err, errs.ECODE_CRYPTO_ERROR, errs.ESTR_CRYPTO_ERROR)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// Sum 计算 token 的落库摘要（BLAKE3-256），用于"原样提交再比对"的协议。
func Sum(plain string) []byte {
	return hash.Sum([]byte(plain))
}

// NTHash 计算 token 的 NT hash：MD4(UTF-16LE(plain))，即 MS-NLMP 的 NTOWFv1。
//
// MD4 本身早已被攻破，但这里是协议兼容要求（NTLMv2 的 HMAC-MD5 用它当密钥），
// 不是安全选择；保密性来自 token 的高熵，而不是哈希强度。
func NTHash(plain string) []byte {
	units := utf16.Encode([]rune(plain))
	buf := make([]byte, len(units)*2)
	for i, u := range units {
		binary.LittleEndian.PutUint16(buf[i*2:], u)
	}
	h := md4.New()
	h.Write(buf)
	return h.Sum(nil)
}

// Expired 判断凭证是否已过期（ExpiresAt 为 0 表示永不过期）。
func Expired(t *db.AccessToken) bool {
	return t.ExpiresAt > 0 && t.ExpiresAt <= time.Now().Unix()
}

// Manager 读写 access_tokens 表，并提供三种协议共用的认证入口。
type Manager struct {
	db *db.DbManager
}

// NewManager 创建凭证管理器。
func NewManager(dbManager *db.DbManager) *Manager {
	return &Manager{db: dbManager}
}

// ─────────────────────────────────────────────────────────────
// 创建 / 查询 / 吊销
// ─────────────────────────────────────────────────────────────

// Create 新建一条随机 token 凭证，返回明文（只返回这一次，库里查不回来）。
// name 是备注；expiresAt 为 Unix 秒，0 表示永不过期。
func (m *Manager) Create(userId int64, name string, expiresAt int64) (string, *db.AccessToken, error) {
	if err := m.requireUser(userId); err != nil {
		return "", nil, err
	}
	plain, err := Generate()
	if err != nil {
		return "", nil, err
	}
	tok := &db.AccessToken{
		UserId:    userId,
		Name:      name,
		Kind:      db.TokenSecret,
		TokenHash: Sum(plain),
		NTHash:    NTHash(plain),
		ExpiresAt: expiresAt,
	}
	if err := m.db.DB.Create(tok).Error; err != nil {
		return "", nil, errs.DBQuery(err)
	}
	return plain, tok, nil
}

// CreatePublicKey 注册一条 SSH 公钥凭证。
// authorizedKey 是 authorized_keys 里的一行（"ssh-ed25519 AAAA... comment"），
// 解析后按规范形式落库（去掉多余空白、注释保留），并记录 SHA256 指纹。
func (m *Manager) CreatePublicKey(userId int64, name, authorizedKey string, expiresAt int64) (*db.AccessToken, error) {
	if err := m.requireUser(userId); err != nil {
		return nil, err
	}
	key, err := ParsePublicKey(authorizedKey)
	if err != nil {
		return nil, err
	}
	tok := &db.AccessToken{
		UserId:      userId,
		Name:        name,
		Kind:        db.TokenPublicKey,
		PublicKey:   string(ssh.MarshalAuthorizedKey(key)),
		Fingerprint: ssh.FingerprintSHA256(key),
		ExpiresAt:   expiresAt,
	}
	if err := m.db.DB.Create(tok).Error; err != nil {
		return nil, errs.DBQuery(err)
	}
	return tok, nil
}

// ParsePublicKey 解析 authorized_keys 行，非法时返回 ErrBadKey。
func ParsePublicKey(authorizedKey string) (ssh.PublicKey, error) {
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(authorizedKey)))
	if err != nil {
		return nil, errs.FromError(err, errs.ECODE_TOKEN_BAD_KEY, errs.ESTR_TOKEN_BAD_KEY)
	}
	return key, nil
}

// List 列出某个用户的全部凭证（含摘要字段，调用方自行决定是否外发）。
// 可按 kind 过滤：kind 为 nil 时不过滤。
func (m *Manager) List(userId int64, kind *db.AccessTokenKind) ([]db.AccessToken, error) {
	q := m.db.DB.Where("user_id = ?", userId)
	if kind != nil {
		q = q.Where("kind = ?", *kind)
	}
	var toks []db.AccessToken
	if err := q.Order("id").Find(&toks).Error; err != nil {
		return nil, errs.DBQuery(err)
	}
	return toks, nil
}

// Get 按 id 取一条凭证，不存在时返回 ErrNotFound。
func (m *Manager) Get(id int64) (*db.AccessToken, error) {
	var tok db.AccessToken
	err := m.db.DB.First(&tok, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, errs.DBQuery(err)
	}
	return &tok, nil
}

// Revoke 删除一条凭证（不可恢复），不存在时返回 ErrNotFound。
func (m *Manager) Revoke(id int64) error {
	res := m.db.DB.Delete(&db.AccessToken{}, id)
	if res.Error != nil {
		return errs.DBQuery(res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// ─────────────────────────────────────────────────────────────
// 认证
// ─────────────────────────────────────────────────────────────

// AuthenticateSecret 用"用户名 + token 明文"认证（SFTP 密码、WebDAV、HTTP API）。
//
// 摘要比对通过后还要求凭证归属该用户；用户名不存在、token 对不上、
// 或 token 属于别人，都返回同一个 ErrInvalid，避免暴露用户是否存在。
func (m *Manager) AuthenticateSecret(username, plain string) (*db.AccessToken, error) {
	if plain == "" {
		return nil, ErrInvalid
	}
	user, err := m.userByName(username)
	if err != nil {
		return nil, err
	}
	tok, err := m.bySecret(plain)
	if err != nil {
		return nil, err
	}
	if tok.UserId != user.Id {
		return nil, ErrInvalid
	}
	m.Touch(tok)
	return tok, nil
}

// AuthenticatePublicKey 用 SSH 公钥认证（SFTP）。
// 按指纹查表后还会复核公钥本体，避免指纹碰撞或伪造的分支。
func (m *Manager) AuthenticatePublicKey(username string, key ssh.PublicKey) (*db.AccessToken, error) {
	if key == nil {
		return nil, ErrInvalid
	}
	user, err := m.userByName(username)
	if err != nil {
		return nil, err
	}
	var tok db.AccessToken
	err = m.db.DB.Where("fingerprint = ? AND kind = ?", ssh.FingerprintSHA256(key), db.TokenPublicKey).
		First(&tok).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrInvalid
	}
	if err != nil {
		return nil, errs.DBQuery(err)
	}
	if tok.PublicKey != string(ssh.MarshalAuthorizedKey(key)) || tok.UserId != user.Id {
		return nil, ErrInvalid
	}
	if Expired(&tok) {
		return nil, ErrExpired
	}
	m.Touch(&tok)
	return &tok, nil
}

// bySecret 按 token 摘要找未过期的凭证。
func (m *Manager) bySecret(plain string) (*db.AccessToken, error) {
	var tok db.AccessToken
	err := m.db.DB.Where("token_hash = ? AND kind = ?", Sum(plain), db.TokenSecret).First(&tok).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrInvalid
	}
	if err != nil {
		return nil, errs.DBQuery(err)
	}
	if Expired(&tok) {
		return nil, ErrExpired
	}
	return &tok, nil
}

// SMBHashes 返回某个用户全部未过期的 SMB 凭证摘要（NT hash），
// 供 NTLMv2 校验逐个试算；同时返回该用户的 id 用于后续挂载鉴权。
// 用户名大小写不敏感，与 NTLM 的行为一致。
func (m *Manager) SMBHashes(username string) (int64, [][]byte, error) {
	user, err := m.userByName(username)
	if err != nil {
		return 0, nil, err
	}
	var toks []db.AccessToken
	if err := m.db.DB.Where("user_id = ? AND kind = ?", user.Id, db.TokenSecret).Find(&toks).Error; err != nil {
		return 0, nil, errs.DBQuery(err)
	}
	var hashes [][]byte
	for i := range toks {
		// 只认 16 字节的 NT hash：长度不对说明记录被写过别的形态，跳过而不是崩。
		if len(toks[i].NTHash) != 16 || Expired(&toks[i]) {
			continue
		}
		hashes = append(hashes, toks[i].NTHash)
	}
	return user.Id, hashes, nil
}

// Touch 记一次成功使用（节流写库，best-effort，失败不影响认证结果）。
func (m *Manager) Touch(t *db.AccessToken) {
	now := time.Now().Unix()
	if t.LastUsedAt != 0 && now-t.LastUsedAt < lastUsedGranularity {
		return
	}
	t.LastUsedAt = now
	m.db.DB.Model(&db.AccessToken{}).Where("id = ?", t.Id).Update("last_used_at", now)
}

// ─────────────────────────────────────────────────────────────
// 内部工具
// ─────────────────────────────────────────────────────────────

// requireUser 校验归属用户存在。
func (m *Manager) requireUser(userId int64) error {
	var user db.User
	err := m.db.DB.First(&user, userId).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrBadUser
	}
	if err != nil {
		return errs.DBQuery(err)
	}
	return nil
}

// userByName 按登录名查用户（大小写不敏感），不存在时返回 ErrInvalid——
// 认证路径上不区分"用户不存在"和"凭证不对"。
func (m *Manager) userByName(username string) (*db.User, error) {
	var user db.User
	err := m.db.DB.Where("lower(username) = lower(?)", strings.TrimSpace(username)).First(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrInvalid
	}
	if err != nil {
		return nil, errs.DBQuery(err)
	}
	return &user, nil
}
