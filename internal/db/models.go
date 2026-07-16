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
	Id      int64  `gorm:"primaryKey"`
	Path    string `gorm:"uniqueIndex"`                                           // 盘路径（本地目录 / S3 bucket）
	PoolId  int64  `gorm:"index:idx_disk_pool_status_type,priority:1"`            // 所属存储池
	Backend DiskBackend                                                           // 后端类型
	Type    DiskType       `gorm:"index:idx_disk_pool_status_type,priority:3"`    // 磁盘角色
	Status  DiskPoolStatus `gorm:"index:idx_disk_pool_status_type,priority:2"`    // 运行状态
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
	ChunkReserved ChunkStatus = iota // 0 — 预分配槽位，尚未写入
	ChunkPending                     // 1 — 数据已写入磁盘，待计算 parity
	ChunkDirty                       // 2 — 数据已更新，parity 需重新计算
	ChunkActive                      // 3 — 数据与 parity 均已就绪
	ChunkError                       // 4 — 数据损坏或磁盘故障
)

// ChunkType chunk 在条带中的角色。
type ChunkType int8

const (
	DataChunk  ChunkType = iota // 0 — 数据分片
	ParityChunk                 // 1 — RS 校验分片
)

// Chunk 是条带中的一个分片，存储在 Disk 上。
type Chunk struct {
	Id        int64       `gorm:"primaryKey"`
	Status    ChunkStatus `gorm:"index"`
	Path      string      // 相对路径（在 Disk.Path 下的位置）
	Size      int64       // 数据实际大小（字节）
	Checksum  []byte      // BLAKE3 哈希，用于数据完整性校验
	DiskId    int64       `gorm:"index"`  // 所在磁盘
	StripeId  int64       `gorm:"index"`  // 所属条带
	Type      ChunkType
	Index     int64       // 在条带中的序号（data=0..D-1, parity=0..P-1）
	CreatedAt int64       `gorm:"autoCreateTime"`
}

// ChunkOp WriteQueue 中记录的操作类型。
type ChunkOp int8

const (
	ChunkWrite ChunkOp = iota // 0 — 写入（新建或更新）
	ChunkDelete               // 1 — 删除（预留）
)

// WriteQueueStatus WriteQueue 条目的处理状态。
type WriteQueueStatus int8

const (
	QueuePending    WriteQueueStatus = iota // 0 — 等待处理
	QueueProcessing                        // 1 — 正在处理
)

// WriteQueue 是 parity 计算的任务队列。
// 当 data chunk 写入或更新时入队，parity worker 出队后计算 RS parity。
type WriteQueue struct {
	Id        int64            `gorm:"primaryKey"`
	ChunkId   int64            `gorm:"index"`
	StripeId  int64            `gorm:"index"`
	Op        ChunkOp          `gorm:"index"`
	Status    WriteQueueStatus `gorm:"index;default:0"`
	CreatedAt int64            `gorm:"autoCreateTime;index"`
}

// ReadCache 记录 chunk 在缓存盘上的副本。
// 同一 chunk 最多有一条未过期的缓存记录。
type ReadCache struct {
	Id        int64 `gorm:"primaryKey"`
	ChunkId   int64 `gorm:"uniqueIndex"`               // 缓存哪个 chunk
	Path      string                                    // 缓存文件相对路径
	DiskId    int64 `gorm:"index"`                      // 缓存所在磁盘
	CreatedAt int64 `gorm:"autoCreateTime;index"`
	UpdatedAt int64 `gorm:"autoUpdateTime;index"`
	ExpiredAt int64 `gorm:"index"`                      // 过期时间戳，超时后清理
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
	TaskPending  TaskStatus = iota // 0 — 等待执行
	TaskRunning                    // 1 — 正在运行
	TaskFinished                   // 2 — 已完成
)

// Task 记录后台任务的执行历史。
// 通过 acquireLock/releaseLock 机制保证同一时间只有一个 Running 任务。
type Task struct {
	Id        int64           `gorm:"primaryKey"`
	Name      string          `gorm:"index"`
	Status    TaskStatus      `gorm:"index"`
	Message   string          // 任务描述
	Metadata  datatypes.JSON `gorm:"type:json"` // 上下文元数据（JSON）
	CreatedAt int64           `gorm:"autoCreateTime;index"`
	UpdatedAt int64           `gorm:"autoUpdateTime"`
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
	InodeCreated InodeEventType = iota // 0 — 创建
	InodeRenamed                       // 1 — 改名
	InodeMoved                         // 2 — 移动
	InodeDeleted                       // 3 — 删除
)

// User 系统用户。
type User struct {
	Id           int64    `gorm:"primaryKey"`
	Username     string   `gorm:"uniqueIndex;not null"`
	PasswordHash string   `gorm:"not null"`
	Role         UserRole `gorm:"default:0"`
	CreatedAt    int64    `gorm:"autoCreateTime"`
}

// Share 是用户可见的存储空间，绑定一个存储池。
// 支持按需配置压缩和加密。
type Share struct {
	Id                int64  `gorm:"primaryKey"`
	Name              string `gorm:"uniqueIndex;not null"`
	PoolId            int64  `gorm:"index;not null"`       // 绑定到哪个存储池
	Compression       int8   `gorm:"default:0"`            // 压缩算法（0=无）
	Encryption        int8   `gorm:"default:0"`            // 加密算法（0=无）
	EncryptionKeyHash []byte `gorm:"default:null"`         // 加密密钥哈希（启用加密时非空）
	CreatedBy         int64  `gorm:"index"`                // 创建者用户 ID
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
type Inode struct {
	Id        int64         `gorm:"primaryKey;autoIncrement"`
	ParentId  sql.NullInt64 `gorm:"index:idx_inode_parent_name,priority:1"`
	Name      string        `gorm:"index:idx_inode_parent_name,priority:2"`
	Kind      InodeKind     `gorm:"default:0"`
	ShareId   int64         `gorm:"index"`
	VersionId sql.NullInt64 `gorm:"index"` // 当前文件版本（目录/链接为 NULL）
	LinkId    sql.NullInt64 `gorm:"index"` // 链接目标 inode（仅 kind=link）
	CreatedAt int64         `gorm:"autoCreateTime"`
	UpdatedAt int64         `gorm:"autoUpdateTime"`
	Deleted   int8          `gorm:"default:0;index"` // 软删除标记
}

// Version 是文件的一个快照版本。
// 同一文件的版本号递增，支持回滚和版本管理。
type Version struct {
	Id         int64 `gorm:"primaryKey;autoIncrement"`
	InodeId    int64 `gorm:"uniqueIndex:idx_ver_inode_num,priority:1;not null"`
	VersionNum int64 `gorm:"uniqueIndex:idx_ver_inode_num,priority:2;not null"` // 版本号（从 1 递增）
	Size       int64 `gorm:"default:0"`                                          // 文件总大小
	Checksum   string                                                            // 文件级哈希（所有 chunk 拼接后）
	CreatedAt  int64 `gorm:"autoCreateTime"`
}

// VersionChunk 将文件版本的逻辑切片映射到存储层的 chunk。
// idx 表示切片在文件中的顺序，读取时按 idx 排序拼接。
type VersionChunk struct {
	VersionId   int64  `gorm:"primaryKey"`
	Idx         int64  `gorm:"primaryKey"`    // 切片序号
	ChunkId     int64  `gorm:"index;not null"` // 对应的存储层 chunk
	Size        int64  `gorm:"not null"`       // 切片大小
	Checksum    []byte `gorm:"not null"`       // 切片级哈希
	IsEncrypted int8   `gorm:"default:0"`
	Compressed  int8   `gorm:"default:0"`
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
