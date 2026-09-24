// Package otp 实现 RFC 6238 的 TOTP（SHA-1 / 30 秒 / 6 位），
// 与 Google Authenticator、Microsoft Authenticator、1Password 等常见 App 兼容。
//
// 密钥是 base32（RFC 4648，无填充）文本；服务端只存密钥本身，验证时按当前时间
// 算出 6 位码与客户端提交的码比对，容忍 ±1 个步长（±30 秒）的时钟漂移。
package otp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/zzc53/zenofs/internal/errs"
)

const (
	// Period 是 TOTP 的步长（秒）。
	Period = 30
	// Digits 是验证码位数。
	Digits = 6
	// SecretBytes 是新密钥的随机长度：20 字节 = SHA-1 的输出长度。
	SecretBytes = 20
	// skewSteps 是允许的时间漂移（步长数）：±1 步 = ±30 秒。
	skewSteps = 1
)

// base32NoPad 是 TOTP 密钥的编码：base32 无填充，App 普遍这么发。
var base32NoPad = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateSecret 生成一个新的 base32 密钥（明文，交给用户录进 App）。
func GenerateSecret() (string, error) {
	buf := make([]byte, SecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", errs.FromError(err, errs.ECODE_CRYPTO_ERROR, errs.ESTR_CRYPTO_ERROR)
	}
	return base32NoPad.EncodeToString(buf), nil
}

// Code 计算 t 时刻的验证码。
func Code(secret string, t time.Time) (string, error) {
	key, err := decodeSecret(secret)
	if err != nil {
		return "", err
	}
	return codeAt(key, uint64(t.Unix()/Period)), nil
}

// Verify 校验客户端提交的六位验证码。
//
// 允许前后各一个步长的时间漂移；比对是常数时间的（不通过耗时泄露信息）。
func Verify(secret, code string, t time.Time) bool {
	code = strings.TrimSpace(code)
	if len(code) != Digits {
		return false
	}
	key, err := decodeSecret(secret)
	if err != nil {
		return false
	}
	counter := t.Unix() / Period
	for delta := int64(-skewSteps); delta <= skewSteps; delta++ {
		if counter+delta < 0 {
			continue
		}
		want := codeAt(key, uint64(counter+delta))
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return true
		}
	}
	return false
}

// URI 返回 otpauth://  URI，便于 App 扫码或手工录入。
func URI(secret, account, issuer string) string {
	q := url.Values{}
	q.Set("secret", secret)
	if issuer != "" {
		q.Set("issuer", issuer)
	}
	q.Set("algorithm", "SHA1")
	q.Set("digits", strconv.Itoa(Digits))
	q.Set("period", strconv.Itoa(Period))

	label := account
	if issuer != "" {
		label = issuer + ":" + account
	}
	return "otpauth://totp/" + url.PathEscape(label) + "?" + q.Encode()
}

// ─────────────────────────────────────────────────────────────
// 内部
// ─────────────────────────────────────────────────────────────

// decodeSecret 解析 base32 密钥：容忍大小写、空格与缺失的填充
// （App 与用户在手工录入时经常这么写）。
func decodeSecret(secret string) ([]byte, error) {
	s := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(secret), " ", ""))
	s = strings.TrimRight(s, "=")
	if s == "" {
		return nil, errs.New(errs.ECODE_OTP_BAD_SECRET, errs.ESTR_OTP_BAD_SECRET, "otp: empty secret", "")
	}
	key, err := base32NoPad.DecodeString(s)
	if err != nil || len(key) == 0 {
		return nil, errs.FromError(err, errs.ECODE_OTP_BAD_SECRET, errs.ESTR_OTP_BAD_SECRET)
	}
	return key, nil
}

// codeAt 按 HOTP（RFC 4226）算某一步的验证码。
func codeAt(key []byte, counter uint64) string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)

	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)

	// 动态截断（RFC 4226 §5.3）：取最后半字节作偏移，取 4 字节并抹掉最高位。
	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff

	mod := uint32(1)
	for i := 0; i < Digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", Digits, value%mod)
}
