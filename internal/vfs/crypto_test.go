package vfs

import (
	"bytes"
	"errors"
	"testing"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/testutil"
)

func TestCompressSliceRoundTrip(t *testing.T) {
	tests := []struct {
		name  string
		plain []byte
	}{
		{"空切片", nil},
		{"单字节", []byte("x")},
		{"短文本", []byte("hello zenofs")},
		{"可压缩", bytes.Repeat([]byte("abcd"), 256*1024)},
		{"不可压缩", testutil.RandBytes(1, 256*1024)},
		{"混合", append(bytes.Repeat([]byte("zenofs-"), 1024), testutil.RandBytes(2, 4096)...)},
	}
	for _, tt := range tests {
		stored, err := compressSlice(tt.plain, CompressionZstd)
		if err != nil {
			t.Fatalf("%s: compressSlice: %v", tt.name, err)
		}
		got, err := decompressSlice(stored, CompressionZstd)
		if err != nil {
			t.Fatalf("%s: decompressSlice: %v", tt.name, err)
		}
		if !bytes.Equal(got, tt.plain) {
			t.Fatalf("%s: 往返不一致（%d → %d → %d 字节）", tt.name, len(tt.plain), len(stored), len(got))
		}
	}
}

func TestCompressSliceEffect(t *testing.T) {
	// 高冗余数据必须真的被压小
	plain := bytes.Repeat([]byte("abcd"), 256*1024)
	stored, err := compressSlice(plain, CompressionZstd)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) >= len(plain)/2 {
		t.Fatalf("可压缩数据只从 %d 压到 %d", len(plain), len(stored))
	}

	// 不可压缩数据只会因为帧头略微膨胀（zstd 存原始块）
	random := testutil.RandBytes(3, 1<<20)
	stored, err = compressSlice(random, CompressionZstd)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) > len(random)+(1<<10) {
		t.Fatalf("不可压缩数据膨胀过多: %d → %d", len(random), len(stored))
	}
}

func TestCompressSliceNoneAndUnknownAlgo(t *testing.T) {
	plain := []byte("passthrough")

	got, err := compressSlice(plain, CompressionNone)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("CompressionNone 应该原样返回: %v", err)
	}
	got, err = decompressSlice(plain, CompressionNone)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("CompressionNone 解压应该原样返回: %v", err)
	}

	unknown := int8(99)
	if _, err := compressSlice(plain, unknown); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("未知压缩算法 = %v, want ErrNotSupported", err)
	}
	if _, err := decompressSlice(plain, unknown); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("未知压缩算法 = %v, want ErrNotSupported", err)
	}
}

func TestDeriveKeyIsDeterministic(t *testing.T) {
	salt := bytes.Repeat([]byte{7}, saltLen)
	k1 := deriveKey("password", salt)
	k2 := deriveKey("password", salt)
	if len(k1) != keyLen {
		t.Fatalf("密钥长度 = %d, want %d", len(k1), keyLen)
	}
	if !bytes.Equal(k1, k2) {
		t.Fatal("同一口令 + 同一盐应得到同一密钥")
	}
	if bytes.Equal(k1, deriveKey("password2", salt)) {
		t.Fatal("不同口令不该得到同一密钥")
	}
	if bytes.Equal(k1, deriveKey("password", bytes.Repeat([]byte{8}, saltLen))) {
		t.Fatal("不同盐不该得到同一密钥")
	}
}

func TestEncryptDecryptSlice(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 1, 0, 4096)
	fs, _ := newShareFS(t, env, pm, p.Id, testutil.ShareOpts{
		Name: "enc", UserID: 1, Permission: db.ShareWrite, Encryption: EncryptionAESGCM,
	})

	plain := []byte("sensitive payload")

	// 未设置口令 / 会话里没有密钥：不能加密
	if fs.HasKey() {
		t.Fatal("刚创建的 Share 不该持有密钥")
	}
	if _, err := fs.encryptSlice(plain, EncryptionAESGCM); !errors.Is(err, ErrEncrypted) {
		t.Fatalf("未提供口令时加密 = %v, want ErrEncrypted", err)
	}
	if _, err := fs.decryptSlice(plain, EncryptionAESGCM); !errors.Is(err, ErrEncrypted) {
		t.Fatalf("未提供口令时解密 = %v, want ErrEncrypted", err)
	}

	// 设置口令后即可加解密
	if err := fs.SetPassword("pw"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if err := fs.UsePassword("pw"); err != nil {
		t.Fatalf("UsePassword: %v", err)
	}
	if !fs.HasKey() {
		t.Fatal("提供口令后 HasKey 应为 true")
	}

	stored, err := fs.encryptSlice(plain, EncryptionAESGCM)
	if err != nil {
		t.Fatalf("encryptSlice: %v", err)
	}
	if bytes.Contains(stored, plain) {
		t.Fatal("密文里出现了明文")
	}
	if len(stored) < nonceLen+len(plain) {
		t.Fatalf("密文长度 = %d, 至少要 nonce(%d) + 明文(%d)", len(stored), nonceLen, len(plain))
	}

	got, err := fs.decryptSlice(stored, EncryptionAESGCM)
	if err != nil {
		t.Fatalf("decryptSlice: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("解密结果与明文不一致")
	}

	// 每次加密用随机 nonce，同一明文两次密文应不同
	again, err := fs.encryptSlice(plain, EncryptionAESGCM)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(again, stored) {
		t.Fatal("两次加密结果相同，说明 nonce 没有随机化")
	}

	// 篡改密文：GCM 认证失败
	tampered := append([]byte(nil), stored...)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := fs.decryptSlice(tampered, EncryptionAESGCM); !errors.Is(err, ErrInvalid) {
		t.Fatalf("篡改后解密 = %v, want ErrInvalid", err)
	}

	// 太短的数据装不下 nonce
	if _, err := fs.decryptSlice([]byte("short"), EncryptionAESGCM); !errors.Is(err, ErrInvalid) {
		t.Fatalf("过短的密文 = %v, want ErrInvalid", err)
	}

	// 换一份口令/盐的 ShareFS 解不开别人的密文
	other, _ := newShareFS(t, env, pm, p.Id, testutil.ShareOpts{
		Name: "enc2", UserID: 1, Permission: db.ShareWrite, Encryption: EncryptionAESGCM,
	})
	if err := other.SetPassword("other-pw"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if err := other.UsePassword("other-pw"); err != nil {
		t.Fatal(err)
	}
	if _, err := other.decryptSlice(stored, EncryptionAESGCM); !errors.Is(err, ErrInvalid) {
		t.Fatalf("用别人的密钥解密 = %v, want ErrInvalid", err)
	}
}

func TestEncryptionNonePassthrough(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 1, 0, 4096)
	fs, _ := newShareFS(t, env, pm, p.Id, testutil.ShareOpts{
		Name: "plain", UserID: 1, Permission: db.ShareWrite,
	})

	plain := []byte("not encrypted")
	stored, err := fs.encryptSlice(plain, EncryptionNone)
	if err != nil || !bytes.Equal(stored, plain) {
		t.Fatalf("EncryptionNone 加密应原样返回: %v", err)
	}
	got, err := fs.decryptSlice(stored, EncryptionNone)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("EncryptionNone 解密应原样返回: %v", err)
	}

	unknown := int8(42)
	if _, err := fs.encryptSlice(plain, unknown); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("未知加密算法 = %v, want ErrNotSupported", err)
	}
	if _, err := fs.decryptSlice(plain, unknown); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("未知加密算法 = %v, want ErrNotSupported", err)
	}
}

func TestEncodeDecodeSliceCombined(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 1, 0, 4096)
	fs, _ := newShareFS(t, env, pm, p.Id, testutil.ShareOpts{
		Name: "both", UserID: 1, Permission: db.ShareWrite,
		Compression: CompressionZstd, Encryption: EncryptionAESGCM,
	})
	if err := fs.SetPassword("pw"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if err := fs.UsePassword("pw"); err != nil {
		t.Fatal(err)
	}

	plain := bytes.Repeat([]byte("compress-then-encrypt "), 512)
	stored, err := fs.encodeSlice(plain, CompressionZstd, EncryptionAESGCM)
	if err != nil {
		t.Fatalf("encodeSlice: %v", err)
	}
	if len(stored) >= len(plain) {
		t.Fatalf("压缩 + 加密后反而更大: %d → %d", len(plain), len(stored))
	}
	got, err := fs.decodeSlice(stored, CompressionZstd, EncryptionAESGCM)
	if err != nil {
		t.Fatalf("decodeSlice: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("压缩 + 加密往返不一致")
	}

	// 不压缩不加密
	raw, err := fs.encodeSlice(plain, CompressionNone, EncryptionNone)
	if err != nil || !bytes.Equal(raw, plain) {
		t.Fatalf("None/None 应原样返回: %v", err)
	}
}

func TestSetPasswordAndUsePasswordLifecycle(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 1, 0, 4096)

	// 未启用加密的 Share：SetPassword 无效，UsePassword 直接成功
	plainFS, _ := newShareFS(t, env, pm, p.Id, testutil.ShareOpts{
		Name: "plain", UserID: 1, Permission: db.ShareWrite,
	})
	if err := plainFS.SetPassword("pw"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("未启用加密时 SetPassword = %v, want ErrInvalid", err)
	}
	if err := plainFS.UsePassword("whatever"); err != nil {
		t.Fatalf("未启用加密时 UsePassword = %v, want nil", err)
	}
	if plainFS.HasKey() {
		t.Fatal("未启用加密的 Share 不该有会话密钥")
	}

	// 启用加密的 Share：设置口令 → 错误口令被拒 → 正确口令可打开
	encFS, share := newShareFS(t, env, pm, p.Id, testutil.ShareOpts{
		Name: "enc", UserID: 1, Permission: db.ShareWrite, Encryption: EncryptionAESGCM,
	})
	if err := encFS.UsePassword("pw"); err == nil {
		t.Fatal("还没设过口令时 UsePassword 应该失败")
	}
	if err := encFS.SetPassword("pw"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	// 库里落下的是 salt || blake3(key)
	var row db.Share
	if err := env.DB.DB.First(&row, share.Id).Error; err != nil {
		t.Fatal(err)
	}
	if len(row.EncryptionKeyHash) != saltLen+verifyLen {
		t.Fatalf("encryption_key_hash 长度 = %d, want %d", len(row.EncryptionKeyHash), saltLen+verifyLen)
	}

	// SetPassword 之后的会话已经持有密钥，此时给错口令只会报错、不会破坏已有会话
	if err := encFS.UsePassword("wrong"); !errors.Is(err, ErrPermission) {
		t.Fatalf("错误口令 = %v, want ErrPermission", err)
	}
	if err := encFS.SetPassword("again"); !errors.Is(err, ErrExist) {
		t.Fatalf("重复设置口令 = %v, want ErrExist", err)
	}

	// 新会话（重新读库里的行）：错误口令拒绝、正确口令放行
	fresh := NewShareFS(pm, env.ReloadShare(share), 1, db.ShareWrite)
	if fresh.HasKey() {
		t.Fatal("新会话不该持有密钥")
	}
	if err := fresh.UsePassword("wrong"); !errors.Is(err, ErrPermission) {
		t.Fatalf("错误口令 = %v, want ErrPermission", err)
	}
	if fresh.HasKey() {
		t.Fatal("口令错误时不该拿到密钥")
	}
	if err := fresh.UsePassword("pw"); err != nil {
		t.Fatalf("UsePassword: %v", err)
	}
	if !fresh.HasKey() {
		t.Fatal("提供口令失败")
	}

	fresh.ClearKey()
	if fresh.HasKey() {
		t.Fatal("Lock 之后不该还有密钥")
	}
	if err := fresh.UsePassword("pw"); err != nil {
		t.Fatalf("重新提供口令: %v", err)
	}
}

func TestRequireCodec(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 1, 0, 4096)

	// 未知算法：构造一个算法字段非法的 Share
	fs := &ShareFS{pm: pm, share: db.Share{Id: 1, PoolId: p.Id, Compression: 9}, userID: 1, perm: db.ShareWrite}
	if err := fs.requireCodec(); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("未知压缩算法 = %v, want ErrNotSupported", err)
	}
	fs.share.Compression = CompressionNone
	fs.share.Encryption = 9
	if err := fs.requireCodec(); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("未知加密算法 = %v, want ErrNotSupported", err)
	}

	// 启用加密但未提供口令 → ErrEncrypted
	fs.share.Encryption = EncryptionAESGCM
	if err := fs.requireCodec(); !errors.Is(err, ErrEncrypted) {
		t.Fatalf("未提供口令 = %v, want ErrEncrypted", err)
	}

	// 未启用加密 → 通过
	fs.share.Encryption = EncryptionNone
	if err := fs.requireCodec(); err != nil {
		t.Fatalf("requireCodec = %v, want nil", err)
	}
}
