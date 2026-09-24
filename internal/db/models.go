// Package db 提供数据库模型定义和 GORM 连接管理。
//
// 分为两层：
//   - Storage Layer: Pool, Disk, Stripe, Chunk 等分布式存储核心模型
//   - Share Layer: User, Share, Inode, Version 等文件系统模型
package db

import (
	"database/sql"

	"gorm.io/datatypes"
)

// DiskPoolStatus 磁盘或存储池的运行状态。
type DiskPoolStatus int8

const (
	Online  DiskPoolStatus = iota // 0 — 在线，可正常读写
	Offline                       // 1 — 离线，暂停所有操作
	Repair                        // 2 — 修复中，数据正在重建
)

// DiskBackend 磁盘后端类型（存储介质）。
type DiskBackend int8

const (
	LocalBackend DiskBackend = iota // 0 — 本地文件系统
	S3Backend                       // 1 — S3 兼容对象存储（预留）
)

// DiskType 磁盘在存储池中的角色。
type DiskType int8

const (
	DataDisk  DiskType = iota // 0 — 数据盘，参与 EC 条带化
	CacheDisk                 // 1 — 缓存盘，仅用作读缓存
)

// Disk 表示存储池中的一块物理/逻辑盘。
type Disk struct {
	Id      int64          `gorm:"primaryKey"`
	Path    string         `gorm:"uniqueIndex"`                                // 盘路径（本地目录 / S3 bucket）
	PoolId  int64          `gorm:"index:idx_disk_pool_status_type,priority:1"` // 所属存储池
	Backend DiskBackend    // 后端类型
	Type    DiskType       `gorm:"index:idx_disk_pool_status_type,priority:3"` // 磁盘角色
	Status  DiskPoolStatus `gorm:"index:idx_disk_pool_status_type,priority:2"` // 运行状态
}

// Pool 是一个 Reed-Solomon 纠删码存储池。
// 每个 Pool 有独立的 DataShards/ParityShards 配置和 ChunkSize。
type Pool struct {
	Id           int64  `gorm:"primaryKey"`
	Name         string `gorm:"uniqueIndex"`
	DataShards   int64  // RS 数据分片数
	ParityShards int64  // RS 校验分片数
	ChunkSize    int64  // 单 chunk 上限（KB）
	Status       DiskPoolStatus
}

// Stripe 是一个 RS 条带，包含若干 data chunk 和 parity chunk。
type Stripe struct {
	Id     int64 `gorm:"primaryKey"`
	PoolId int64 `gorm:"index"`
}

// ChunkStatus chunk 的生命周期状态。
type ChunkStatus int8

const (
	ChunkReserved  ChunkStatus = iota // 0 — 预分配槽位，尚未写入
	ChunkAllocated                    // 1 — 数据块已分配
)

// ChunkType chunk 在条带中的角色。
type ChunkType int8

const (
	DataChunk   ChunkType = iota // 0 — 数据分片
	ParityChunk                  // 1 — RS 校验分片
)

// Chunk 是条带中的一个分片，存储在 Disk 上。
type Chunk struct {
	Id        int64       `gorm:"primaryKey"`
	Status    ChunkStatus `gorm:"index"`
	Path      string      // 相对路径（在 Disk.Path 下的位置）
	Size      int64       // 数据实际大小（字节）
	Hash      []byte      // BLAKE3 哈希，用于数据完整性校验
	DiskId    int64       `gorm:"index"` // 所在磁盘
	StripeId  int64       `gorm:"index"` // 所属条带
	PoolId    int64       `gorm:"index"` // 所属存储池
	Type      ChunkType
	Index     int64 // 在条带中的序号（data=0..D-1, parity=0..P-1）
	CreatedAt int64 `gorm:"autoCreateTime"`
}

// WriteQueue 是 write 计算的任务队列。
// 当 data chunk 写入或更新时入队，parity worker 出队后计算 RS parity。
type WriteQueue struct {
	Id        int64      `gorm:"primaryKey"`
	ChunkId   int64      `gorm:"index"`
	StripeId  int64      `gorm:"index"`
	Status    TaskStatus `gorm:"index"`
	CreatedAt int64      `gorm:"autoCreateTime;index"`
}

type StripeQueueType int8

const (
	StripeQueueParity = iota
	StripeQueueRebuild
)

// StripeQueue 是条带级任务队列（parity 计算 / 故障重建）。
// TaskId 指向发起该任务的后台作业（Task）；parity 任务是写入流程的副产品，
// 没有父作业，TaskId 为 0。
type StripeQueue struct {
	Id        int64           `gorm:"primaryKey"`
	StripeId  int64           `gorm:"index"`
	Type      StripeQueueType `gorm:"index"`
	TaskId    int64           `gorm:"index"` // 所属重建作业（Task.Id），0 表示无父作业
	Status    TaskStatus      `gorm:"index;default:0"`
	CreatedAt int64           `gorm:"autoCreateTime;index"`
}

type CacheStatus int8

const (
	NotCached = iota
	Cached
)

// ReadCache 记录 chunk 在缓存盘上的副本。
// 同一 chunk 最多有一条未过期的缓存记录。
type ReadCache struct {
	Id          int64       `gorm:"primaryKey"`
	ChunkId     int64       `gorm:"uniqueIndex"` // 缓存哪个 chunk
	Path        string      // 缓存文件相对路径
	DiskId      int64       `gorm:"index"` // 缓存所在磁盘
	AccessCount int64       `gorm:"index"`
	Status      CacheStatus `gorm:"index"`
	CreatedAt   int64       `gorm:"autoCreateTime;index"`
	UpdatedAt   int64       `gorm:"autoUpdateTime;index"`
}

// Setting 存储全局 KV 配置（如 HTTP_PORT）。
type Setting struct {
	Id        int64  `gorm:"primaryKey"`
	Name      string `gorm:"uniqueIndex"`
	Value     string
	CreatedAt int64 `gorm:"autoCreateTime;index"`
	UpdatedAt int64 `gorm:"autoUpdateTime;index"`
}

// TaskStatus 后台任务的执行状态。
type TaskStatus int8

const (
	TaskPending TaskStatus = iota // 0 — 等待执行
	TaskRunning                   // 1 — 正在运行
	TaskSuccess                   // 2 — 成功
	TaskFail                      // 3 - 失败
)

// Task 记录后台作业（一次重建 = 一条）。
// 同一个 pool 同时只允许有一个进行中的作业：作业名带上 poolId，投递前按名字查重。
type Task struct {
	Id        int64          `gorm:"primaryKey"`
	Name      string         `gorm:"index"`
	Status    TaskStatus     `gorm:"index"`
	Message   string         // 任务描述
	Metadata  datatypes.JSON `gorm:"type:json"` // 上下文元数据（JSON）
	CreatedAt int64          `gorm:"autoCreateTime;index"`
	UpdatedAt int64          `gorm:"autoUpdateTime"`
}

// ── Share Layer (User/Share/Inode/Version) ──

// UserRole 用户角色。
type UserRole int8

const (
	UserNormal UserRole = iota // 0 — 普通用户
	UserAdmin                  // 1 — 管理员
)

// SharePermission 用户在 Share 中的访问权限。
type SharePermission int8

const (
	ShareRead  SharePermission = iota // 0 — 只读
	ShareWrite                        // 1 — 读写
	ShareAdmin                        // 2 — 管理（可修改权限）
)

// InodeKind inode 的类型。
type InodeKind int8

const (
	InodeFile InodeKind = iota // 0 — 普通文件
	InodeDir                   // 1 — 目录
	InodeLink                  // 2 — 链接
)

// InodeEventType inode 历史事件类型。
type InodeEventType int8

const (
	InodeCreated  InodeEventType = iota // 0 — 创建
	InodeRenamed                        // 1 — 改名
	InodeMoved                          // 2 — 移动
	InodeDeleted                        // 3 — 删除（进回收站）
	InodeRestored                       // 4 — 从回收站恢复
)

// User 系统用户。
//
// 登录要过两关：bcrypt 校验密码，再用 TOTP（SHA-1 / 30 秒 / 6 位）校验验证码。
// OTPSecret 是 base32 密钥，只在创建/重置时明文出现一次，之后只用于算码。
type User struct {
	Id           int64    `gorm:"primaryKey"`
	Username     string   `gorm:"uniqueIndex;not null"`
	PasswordHash string   `gorm:"not null"`
	OTPSecret    string   `gorm:"not null;default:''"` // TOTP 密钥（base32）；空表示该用户没有二次验证
	Role         UserRole `gorm:"default:0"`
	CreatedAt    int64    `gorm:"autoCreateTime"`
}

// Share 是用户可见的存储空间，绑定一个存储池。
// 支持按需配置压缩、加密和空间配额。
//
// 文件切片大小**不在 Share 上**：它取自所属 Pool 的 ChunkSize（见 vfs 的 sliceSize）。
// 切片大小决定文件怎么切成 chunk，读写偏移都依赖它，挂在池上才能保证一个池里
// 所有共享用的是同一个值。
type Share struct {
	Id                int64  `gorm:"primaryKey"`
	Name              string `gorm:"uniqueIndex;not null"`
	PoolId            int64  `gorm:"index;not null"` // 绑定到哪个存储池
	Quota             int64  `gorm:"default:0"`      // 空间配额上限（MB），0 表示不限制
	Compression       int8   `gorm:"default:0"`      // 压缩算法（取值见 vfs.Compression*：0=无、1=zstd）
	Encryption        int8   `gorm:"default:0"`      // 加密算法（取值见 vfs.Encryption*：0=无、1=AES-256-GCM）
	EncryptionKeyHash []byte `gorm:"default:null"`   // 加密密钥校验值 / PBKDF2 salt（启用加密时非空）
	CreatedBy         int64  `gorm:"index"`          // 创建者用户 ID
	CreatedAt         int64  `gorm:"autoCreateTime"`
}

// ShareUser 记录用户对 Share 的访问权限。
type ShareUser struct {
	ShareId    int64           `gorm:"primaryKey"`
	UserId     int64           `gorm:"primaryKey"`
	Permission SharePermission `gorm:"default:0"`
}

// Inode 是文件/目录/链接的元数据节点（类似 POSIX inode）。
// parent_id 为 NULL 时表示 Share 根目录。
// kind=link 时 LinkId 指向目标 inode。
// 软删除通过 Deleted 标志实现，保留历史记录。
//
// 对外（internal/vfs）的属性映射：
//
//	uid   → CreatedBy（属主用户）
//	gid   → ShareId（所属 Share）
//	atime / ctime / mtime → UpdatedAt
//	mode  → 只区分"是否可执行"（Executable 字段）；读写权限由
//	        ShareUser.Permission 在挂载层控制，不落在 inode 上
type Inode struct {
	Id         int64         `gorm:"primaryKey;autoIncrement"`
	ParentId   sql.NullInt64 `gorm:"index:idx_inode_parent_name,priority:1"`
	Name       string        `gorm:"index:idx_inode_parent_name,priority:2"`
	Kind       InodeKind     `gorm:"default:0"`
	ShareId    int64         `gorm:"index"`
	VersionId  sql.NullInt64 `gorm:"index"`     // 当前文件版本（目录/链接为 NULL）
	LinkId     sql.NullInt64 `gorm:"index"`     // 链接目标 inode（仅 kind=link）
	Executable int8          `gorm:"default:0"` // 1 表示可执行（POSIX 的 x 位）
	CreatedBy  int64         `gorm:"index"`     // 创建用户ID
	CreatedAt  int64         `gorm:"autoCreateTime"`
	UpdatedBy  int64         `gorm:"index"`
	UpdatedAt  int64         `gorm:"autoUpdateTime"`
	Deleted    int8          `gorm:"default:0;index"` // 软删除标记
}

// Version 是文件的一个快照版本。
// 同一文件的版本号递增，支持回滚和版本管理。
type Version struct {
	Id          int64         `gorm:"primaryKey;autoIncrement"`
	InodeId     int64         `gorm:"index;not null"` // 同一 inode 可有多个版本，当前版本由 Inode.VersionId 指定
	ParentId    sql.NullInt64 `gorm:"index"`
	Size        int64         `gorm:"default:0"` // 文件总大小
	Hash        string        // 文件级哈希（所有 chunk 拼接后）
	Encryption  int8          `gorm:"default:0"`
	Compression int8          `gorm:"default:0"`
	CreatedBy   int64         `gorm:"index"` // 创建用户ID
	CreatedAt   int64         `gorm:"autoCreateTime"`
}

// VersionChunk 将文件版本的逻辑切片映射到存储层的 chunk。
// idx 表示切片在文件中的顺序，读取时按 idx 排序拼接。
type VersionChunk struct {
	VersionId int64  `gorm:"primaryKey"`
	Idx       int64  `gorm:"primaryKey"`     // 切片序号
	ChunkId   int64  `gorm:"index;not null"` // 对应的存储层 chunk
	Size      int64  `gorm:"not null"`       // 切片大小
	Hash      []byte `gorm:"not null"`       // 切片级哈希
}

// InodeHistory 记录 inode 的元数据变更事件（创建/改名/移动/删除）。
// 用于审计和操作追溯。
type InodeHistory struct {
	Id          int64          `gorm:"primaryKey;autoIncrement"`
	InodeId     int64          `gorm:"index;not null"`
	EventType   InodeEventType `gorm:"not null"`
	OldName     sql.NullString // 改名前的文件名
	NewName     sql.NullString // 改名后的文件名
	OldParentId sql.NullInt64  // 移动前的父目录
	NewParentId sql.NullInt64  // 移动后的父目录
	CreatedAt   int64          `gorm:"autoCreateTime;index"`
}

// ── Access Layer (AccessToken) ──

// AccessTokenKind 访问凭证的形态。
type AccessTokenKind int8

const (
	TokenSecret    AccessTokenKind = iota // 0 — 随机 token：SMB / SFTP / WebDAV / HTTP 通用
	TokenPublicKey                        // 1 — SSH 公钥：仅 SFTP 公钥认证
)

// AccessToken 是一条协议访问凭证：要么是随机 token（可登所有协议），
// 要么是 SSH 公钥（仅 SFTP 公钥认证）。两者都绑定一个 users 记录。
//
// token 明文只在生成时返回一次，库里只留单向摘要：
//   - TokenHash：BLAKE3-256(token)，用于"客户端把 token 原样交上来比对"的协议
//     （SFTP 密码、WebDAV、HTTP API）；
//   - NTHash：MD4(UTF-16LE(token))，即 MS-NLMP 的 NTOWFv1。SMB 的 NTLMv2 校验
//     在数学上必须用它（HMAC-MD5 的密钥就是它），无法用别的摘要替代。
//
// 两者都不可逆：token 是 32 字节随机串，拿到摘要既不能还原、也无法离线爆破。
// 公钥不是秘密（本来就随私钥持有者公开），所以 PublicKey 明文存储，
// 另记 Fingerprint（SHA256:base64）供按 key 查表与展示。
//
// ExpiresAt 是过期时间（Unix 秒），0 表示永不过期；两种凭证都用它。
type AccessToken struct {
	Id          int64           `gorm:"primaryKey"`
	UserId      int64           `gorm:"index;not null"` // 归属用户（users.id）
	Name        string          // 备注，便于用户识别设备/用途
	Kind        AccessTokenKind // 凭证形态
	TokenHash   []byte          `gorm:"column:token_hash;index"` // 单向摘要：BLAKE3-256(token)；公钥凭证为 NULL
	NTHash      []byte          `gorm:"column:nt_hash"`          // 单向摘要：MD4(UTF-16LE(token))，仅 SMB 用；公钥凭证为 NULL
	PublicKey   string          `gorm:"type:text"`               // SSH 公钥（authorized_keys 单行）；token 凭证为空
	Fingerprint string          `gorm:"index"`                   // 公钥指纹 SHA256:base64；token 凭证为空
	ExpiresAt   int64           `gorm:"index"`                   // 过期时间（Unix 秒），0 = 永不过期
	CreatedAt   int64           `gorm:"autoCreateTime"`
	LastUsedAt  int64           // 最近一次成功认证的时间（Unix 秒），0 = 从未使用
}
