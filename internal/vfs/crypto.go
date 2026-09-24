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
	"github.com/zzc53/zenofs/internal/pool"
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
	// gcmTagLen 是 AES-GCM 的认证标签长度（crypto/cipher 的 GCM 固定 16 字节）。
	gcmTagLen = 16
)

// sliceOverheadFor 估算一个切片编码后会比明文多出多少字节（取上界，宁可多留一点）。
//
// 为什么需要它：写进池的是**编码后**的字节，而池会拒绝超过 chunk 上限的块。
// 切片大小必须从池的上限里扣掉这部分余量，否则"文件大小正好是切片整数倍"时，
// 满切片会被存储层以 CHUNK_SIZE_EXCEED 拒收。
//
//   - 压缩：zstd 对不可压缩数据（随机字节、已经压过的文件）不会变小，反而要加帧头与
//     块头；参照 ZSTD 的官方上界（膨胀约 size/256），再留 64 字节给帧头。
//   - 加密：密文前面有 nonce(12)，尾部有认证标签(16)。
func sliceOverheadFor(size int64, comp, enc int8) int64 {
	var n int64
	if comp == CompressionZstd {
		n += size/256 + 64
	}
	if enc == EncryptionAESGCM {
		n += nonceLen + gcmTagLen
	}
	return n
}

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
		return ErrExist // 已经设置过口令，改用 UsePassword
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

// UsePassword 用口令派生密钥并打开该 Share 的加密；口令不对返回 ErrPermission。
// 未启用加密的 Share 无需提供口令，直接成功。
//
// 这是**挂载实例级**的密钥，只影响这一个 ShareFS。要让整个进程都解锁该 Share
// （LUKS 式，所有协议共用同一份密钥），用 UnlockShare。
func (fs *ShareFS) UsePassword(password string) error {
	if fs.share.Encryption == EncryptionNone {
		return nil
	}
	key, err := verifyPassword(fs.share, password)
	if err != nil {
		return err
	}
	fs.key = key
	return nil
}

// ClearKey 清除挂载实例里的密钥（不动进程级密钥表，那要用 LockShare）。
func (fs *ShareFS) ClearKey() {
	for i := range fs.key {
		fs.key[i] = 0
	}
	fs.key = nil
}

// HasKey 报告当前是否有可用密钥。
func (fs *ShareFS) HasKey() bool {
	return len(fs.keyBytes()) == keyLen
}

// keyBytes 返回当前可用的解密密钥。
//
// 优先级：挂载实例自己的（UsePassword / SetPassword 设的）→ 进程级密钥表里已解锁的
// （UnlockShare 设的）。后者正是"管理员解锁一次，所有协议都能用"的落点。
func (fs *ShareFS) keyBytes() []byte {
	if len(fs.key) == keyLen {
		return fs.key
	}
	if key, ok := fs.pm.Keys.Get(fs.share.Id); ok && len(key) == keyLen {
		return key
	}
	return nil
}

// deriveKey 用 PBKDF2-HMAC-SHA256 从口令派生 AES-256 密钥。
func deriveKey(password string, salt []byte) []byte {
	return pbkdf2.Key([]byte(password), salt, pbkdf2Iter, keyLen, sha256.New)
}

// aead 返回绑定当前密钥的 AES-256-GCM。
func (fs *ShareFS) aead() (cipher.AEAD, error) {
	key := fs.keyBytes()
	if len(key) != keyLen {
		return nil, ErrEncrypted
	}
	block, err := aes.NewCipher(key)
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
// 算法要认识，启用加密时要已持有密钥。
func (fs *ShareFS) requireCodec() error {
	switch fs.share.Compression {
	case CompressionNone, CompressionZstd:
	default:
		return ErrNotSupported
	}
	switch fs.share.Encryption {
	case EncryptionNone:
	case EncryptionAESGCM:
		if !fs.HasKey() {
			return ErrEncrypted
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

// ---------------------------------------------------------------
// 进程级解锁（LUKS 式 open / close）
// ---------------------------------------------------------------

// UnlockShare 用口令解锁一个启用了加密的 Share：校验通过后把派生密钥放进进程级
// 密钥表（pm.Keys），此后所有协议（HTTP API / WebDAV / SMB / SFTP）都能读写它，
// 直到 LockShare 或进程退出——密钥只在内存里，不落库，重启后要重新解锁。
//
// 口令不对返回 ErrPermission；Share 没启用加密、或还没设置过口令返回 ErrInvalid。
func UnlockShare(pm *pool.PoolManager, share db.Share, password string) error {
	if share.Encryption == EncryptionNone {
		return ErrInvalid
	}
	key, err := verifyPassword(share, password)
	if err != nil {
		return err
	}
	pm.Keys.Set(share.Id, key)
	return nil
}

// LockShare 丢弃某个 Share 在内存里的密钥（并把字节清零），类似 LUKS close。
//
// 已经在读写的句柄会随即失败，所以调用方要保证此刻没有正在进行的操作。
func LockShare(pm *pool.PoolManager, shareID int64) {
	pm.Keys.Delete(shareID)
}

// ShareUnlocked 报告某个 Share 的密钥当前是否在内存里。
func ShareUnlocked(pm *pool.PoolManager, shareID int64) bool {
	key, ok := pm.Keys.Get(shareID)
	return ok && len(key) == keyLen
}

// verifyPassword 校验口令并返回派生密钥。
// 口令不对返回 ErrPermission；校验值缺失或长度不对返回 ErrInvalid（还没设置过口令）。
func verifyPassword(share db.Share, password string) ([]byte, error) {
	blob := share.EncryptionKeyHash
	if len(blob) != saltLen+verifyLen {
		return nil, ErrInvalid
	}
	salt, want := blob[:saltLen], blob[saltLen:]
	key := deriveKey(password, salt)
	if !hash.Equal(key, want) {
		return nil, ErrPermission
	}
	return key, nil
}
