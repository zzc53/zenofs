package vfs

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"sync"

	"github.com/klauspost/compress/zstd"
	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/errs"
	"github.com/zzc53/zenofs/internal/hash"
	"golang.org/x/crypto/pbkdf2"
)

// ---------------------------------------------------------------
// 压缩
// ---------------------------------------------------------------
//
// 每个切片独立压缩成一整条 zstd 帧。这样每片都能单独解压，
// 不需要读入整个文件，随机读与"只重建被写到的切片"才成立。
// 帧头只有几字节到十几字节，相对 MB 级切片可以忽略。

// 压缩算法编号，与 Share.Compression / Version.Compression 对应。
const (
	CompressionNone int8 = 0 // 不压缩
	CompressionZstd int8 = 1 // Zstandard（klauspost/compress/zstd），每片一条独立帧
)

// zstd 编解码器整个进程复用一份：Encoder.EncodeAll / Decoder.DecodeAll 都允许并发调用，
// 而编码器初始化（窗口缓冲）开销不小，切片又是 MB 级别的高频调用，不适合每片新建。
var (
	zstdOnce    sync.Once
	zstdEnc     *zstd.Encoder
	zstdDec     *zstd.Decoder
	zstdInitErr error
)

// zstdCodecs 惰性初始化进程共用的 zstd 编解码器。
func zstdCodecs() (*zstd.Encoder, *zstd.Decoder, error) {
	zstdOnce.Do(func() {
		// 编码并发度压到 1：EncodeAll 每次调用只借用一个块编码器，默认值（GOMAXPROCS）
		// 会常驻多份 8MB 窗口缓冲；写路径本身的并发交给上层。压缩成为写瓶颈时可调大。
		if zstdEnc, zstdInitErr = zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1)); zstdInitErr != nil {
			return
		}
		// 解码保持默认并发度（GOMAXPROCS）：随机读要能并发解压。
		zstdDec, zstdInitErr = zstd.NewReader(nil)
	})
	return zstdEnc, zstdDec, zstdInitErr
}

// zstdError 把 zstd 的错误按本文件惯例归到 CRYPTO_ERROR（编解码失败同一类）。
func zstdError(err error) error {
	return errs.FromError(err, errs.ECODE_CRYPTO_ERROR, errs.ESTR_CRYPTO_ERROR)
}

// ---------------------------------------------------------------
// 加密
// ---------------------------------------------------------------
//
// 每个切片独立用 AES-256-GCM 加密，密文格式为：nonce(12B) || ciphertext+tag。
// 每片用随机 nonce；GCM 自带认证，密文被篡改时解密会直接失败。
//
// 密钥由用户口令经 PBKDF2-HMAC-SHA256 派生，只保存在会话内存（ShareFS.key），
// 不落库。落库的只有 Share.EncryptionKeyHash：salt(16B) || blake3(密钥)(32B)，
// 前者供派生使用，后者用于校验口令是否正确。

// 加密算法编号，与 Share.Encryption / Version.Encryption 对应。
const (
	EncryptionNone   int8 = 0 // 不加密
	EncryptionAESGCM int8 = 1 // AES-256-GCM
)

const (
	// pbkdf2Iter 是密钥派生的迭代次数（OWASP 对 PBKDF2-SHA256 的建议量级）。
	pbkdf2Iter = 600000
	// saltLen 是盐长度。
	saltLen = 16
	// keyLen 是 AES-256 的密钥长度。
	keyLen = 32
	// verifyLen 是口令校验值（blake3(密钥)）的长度。
	verifyLen = 32
	// nonceLen 是 AES-GCM 的标准 nonce 长度。
	nonceLen = 12
)

// ---------------------------------------------------------------
// 口令与密钥
// ---------------------------------------------------------------

// SetPassword 为首次启用加密的 Share 设置口令：生成随机 salt、派生密钥，
// 并把 salt 与校验值写进 Share.EncryptionKeyHash；密钥留在会话内存里。
func (fs *ShareFS) SetPassword(password string) error {
	if fs.share.Encryption == EncryptionNone {
		return ErrInvalid // 该 Share 没有启用加密
	}
	if len(fs.share.EncryptionKeyHash) != 0 {
		return ErrExist // 已经设置过口令，改用 Unlock
	}

	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return errs.FromError(err, errs.ECODE_CRYPTO_ERROR, errs.ESTR_CRYPTO_ERROR)
	}
	blob := make([]byte, 0, saltLen+verifyLen)
	blob = append(blob, salt...)
	key := deriveKey(password, salt)
	blob = append(blob, hash.Sum(key)...)

	if err := fs.pm.DbManager.DB.Model(&db.Share{}).Where("id = ?", fs.share.Id).
		Update("encryption_key_hash", blob).Error; err != nil {
		return errs.DBQuery(err)
	}
	fs.share.EncryptionKeyHash = blob
	fs.key = key
	return nil
}

// Unlock 用口令派生密钥并解锁该 Share；口令不对返回 ErrPermission。
// 未启用加密的 Share 无需解锁，直接成功。
func (fs *ShareFS) Unlock(password string) error {
	if fs.share.Encryption == EncryptionNone {
		return nil
	}
	blob := fs.share.EncryptionKeyHash
	if len(blob) != saltLen+verifyLen {
		return ErrInvalid // 还没有设置过口令
	}
	salt, want := blob[:saltLen], blob[saltLen:]
	key := deriveKey(password, salt)
	if !hash.Equal(key, want) {
		return ErrPermission
	}
	fs.key = key
	return nil
}

// Lock 清除会话里的密钥。
func (fs *ShareFS) Lock() {
	for i := range fs.key {
		fs.key[i] = 0
	}
	fs.key = nil
}

// Unlocked 报告当前会话是否已持有可用密钥。
func (fs *ShareFS) Unlocked() bool {
	return len(fs.key) == keyLen
}

// deriveKey 用 PBKDF2-HMAC-SHA256 从口令派生 AES-256 密钥。
func deriveKey(password string, salt []byte) []byte {
	return pbkdf2.Key([]byte(password), salt, pbkdf2Iter, keyLen, sha256.New)
}

// aead 返回绑定当前会话密钥的 AES-256-GCM。
func (fs *ShareFS) aead() (cipher.AEAD, error) {
	if !fs.Unlocked() {
		return nil, ErrLocked
	}
	block, err := aes.NewCipher(fs.key)
	if err != nil {
		return nil, errs.FromError(err, errs.ECODE_CRYPTO_ERROR, errs.ESTR_CRYPTO_ERROR)
	}
	return cipher.NewGCM(block)
}

// ---------------------------------------------------------------
// 切片编解码
// ---------------------------------------------------------------

// encodeSlice 把切片明文转成落存储层的形式：先压缩，再加密。
// comp/enc 取自该切片所属的 Version，这样改算法不会影响历史数据。
func (fs *ShareFS) encodeSlice(plain []byte, comp, enc int8) ([]byte, error) {
	out := plain
	var err error
	if out, err = compressSlice(out, comp); err != nil {
		return nil, err
	}
	if out, err = fs.encryptSlice(out, enc); err != nil {
		return nil, err
	}
	return out, nil
}

// decodeSlice 与 encodeSlice 相反：先解密，再解压。
func (fs *ShareFS) decodeSlice(stored []byte, comp, enc int8) ([]byte, error) {
	out := stored
	var err error
	if out, err = fs.decryptSlice(out, enc); err != nil {
		return nil, err
	}
	if out, err = decompressSlice(out, comp); err != nil {
		return nil, err
	}
	return out, nil
}

// requireCodec 检查写路径所需的编解码条件：
// 算法要认识，启用加密时要处于已解锁状态。
func (fs *ShareFS) requireCodec() error {
	switch fs.share.Compression {
	case CompressionNone, CompressionZstd:
	default:
		return ErrNotSupported
	}
	switch fs.share.Encryption {
	case EncryptionNone:
	case EncryptionAESGCM:
		if !fs.Unlocked() {
			return ErrLocked
		}
	default:
		return ErrNotSupported
	}
	return nil
}

// compressSlice 压缩一个切片；algo 为 CompressionNone 时原样返回。
func compressSlice(plain []byte, algo int8) ([]byte, error) {
	switch algo {
	case CompressionNone:
		return plain, nil
	case CompressionZstd:
		enc, _, err := zstdCodecs()
		if err != nil {
			return nil, zstdError(err)
		}
		// EncodeAll 把结果写进新缓冲（dst 为 nil），不会碰 plain。
		return enc.EncodeAll(plain, nil), nil
	default:
		return nil, ErrNotSupported
	}
}

// decompressSlice 解压一个切片；algo 为 CompressionNone 时原样返回。
func decompressSlice(stored []byte, algo int8) ([]byte, error) {
	switch algo {
	case CompressionNone:
		return stored, nil
	case CompressionZstd:
		_, dec, err := zstdCodecs()
		if err != nil {
			return nil, zstdError(err)
		}
		out, err := dec.DecodeAll(stored, nil)
		if err != nil {
			return nil, zstdError(err)
		}
		return out, nil
	default:
		return nil, ErrNotSupported
	}
}

// encryptSlice 加密一个切片，输出 nonce || ciphertext+tag。
func (fs *ShareFS) encryptSlice(plain []byte, algo int8) ([]byte, error) {
	switch algo {
	case EncryptionNone:
		return plain, nil
	case EncryptionAESGCM:
		aead, err := fs.aead()
		if err != nil {
			return nil, err
		}
		nonce := make([]byte, nonceLen)
		if _, err := rand.Read(nonce); err != nil {
			return nil, errs.FromError(err, errs.ECODE_CRYPTO_ERROR, errs.ESTR_CRYPTO_ERROR)
		}
		return aead.Seal(nonce, nonce, plain, nil), nil
	default:
		return nil, ErrNotSupported
	}
}

// decryptSlice 解密一个切片；认证失败（被篡改或密钥不对）返回 ErrInvalid。
func (fs *ShareFS) decryptSlice(stored []byte, algo int8) ([]byte, error) {
	switch algo {
	case EncryptionNone:
		return stored, nil
	case EncryptionAESGCM:
		if len(stored) < nonceLen {
			return nil, ErrInvalid
		}
		aead, err := fs.aead()
		if err != nil {
			return nil, err
		}
		nonce, ct := stored[:nonceLen], stored[nonceLen:]
		plain, err := aead.Open(nil, nonce, ct, nil)
		if err != nil {
			return nil, ErrInvalid // GCM 认证失败
		}
		return plain, nil
	default:
		return nil, ErrNotSupported
	}
}
