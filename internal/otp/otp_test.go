package otp

import (
	"encoding/base32"
	"strings"
	"testing"
	"time"
)

// RFC 6238 附录 B 的测试向量（SHA-1 密钥 "12345678901234567890"）。
// 表里给的是 8 位值，我们输出 6 位，所以取低 6 位。
func TestCodeRFC6238Vectors(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).
		EncodeToString([]byte("12345678901234567890"))

	cases := []struct {
		unix int64
		want string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
		{20000000000, "353130"},
	}
	for _, c := range cases {
		got, err := Code(secret, time.Unix(c.unix, 0))
		if err != nil {
			t.Fatalf("Code(%d): %v", c.unix, err)
		}
		if got != c.want {
			t.Errorf("Code(%d) = %s，期望 %s", c.unix, got, c.want)
		}
	}
}

func TestVerifyToleratesSkew(t *testing.T) {
	secret, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	now := time.Unix(1700000000, 0)

	prev, err := Code(secret, now.Add(-Period*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	next, err := Code(secret, now.Add(Period*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	// ±1 步长内都接受（客户端时钟可能偏一点）
	if !Verify(secret, prev, now) || !Verify(secret, next, now) {
		t.Fatalf("±1 步长的验证码应当被接受")
	}
	// 超过 3 步就不再接受
	far, err := Code(secret, now.Add(3*Period*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if Verify(secret, far, now) {
		t.Fatalf("3 步之外的验证码不该被接受")
	}
}

func TestVerifyRejectsBadInput(t *testing.T) {
	secret, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0)

	for _, code := range []string{"", "12345", "1234567", "abcdef", "000000"} {
		if Verify(secret, code, now) {
			t.Errorf("Verify(%q) 应当为 false", code)
		}
	}
	if Verify("", "123456", now) {
		t.Errorf("空密钥应当为 false")
	}
	if Verify("not-base32!!", "123456", now) {
		t.Errorf("非法密钥应当为 false")
	}
}

func TestDecodeSecretTolerant(t *testing.T) {
	code := func(secret string) string {
		got, err := Code(secret, time.Unix(59, 0))
		if err != nil {
			t.Fatalf("Code(%q): %v", secret, err)
		}
		return got
	}
	canonical := code("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ")
	// 小写、带空格、带填充、带尾随空白都应解析成同一个密钥
	for _, variant := range []string{
		"gezdgnbvgy3tqojqgezdgnbvgy3tqojq",
		"GEZD GNBV GY3T QOJQ GEZD GNBV GY3T QOJQ",
		"GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ====",
		"  GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ\t",
	} {
		if got := code(variant); got != canonical {
			t.Errorf("变体 %q 解析结果 %s，期望 %s", variant, got, canonical)
		}
	}
}

func TestGenerateSecretIsRandomAndUsable(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 16; i++ {
		secret, err := GenerateSecret()
		if err != nil {
			t.Fatalf("GenerateSecret: %v", err)
		}
		if seen[secret] {
			t.Fatalf("GenerateSecret 产生了重复密钥")
		}
		seen[secret] = true
		if len(secret) != 32 { // 20 字节 → base32 无填充 32 字符
			t.Fatalf("密钥长度 = %d，期望 32", len(secret))
		}
		if _, err := Code(secret, time.Now()); err != nil {
			t.Fatalf("生成的密钥不可用: %v", err)
		}
	}
}

// URI 的完整字符串是前后端的契约：Web 前端会在浏览器里拼同一个地址画二维码，
// 所以这里做精确比对（web/src/totp.test.ts 用的是同一个期望值）。
func TestURIExactFormat(t *testing.T) {
	const want = "otpauth://totp/zenofs:alice?algorithm=SHA1&digits=6&issuer=zenofs&period=30&secret=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	if got := URI("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", "alice", "zenofs"); got != want {
		t.Fatalf("URI = %q\n期望 %q", got, want)
	}
	// 没有 issuer 时标签只有账号名
	if got := URI("ABCDEF", "alice", ""); !strings.HasPrefix(got, "otpauth://totp/alice?") {
		t.Fatalf("无 issuer 的 URI = %q", got)
	}
}
