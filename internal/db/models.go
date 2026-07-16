package db

type DiskPoolStatus int8

const (
	Online DiskPoolStatus = iota
	Offline
	Repair
)

type DiskBackend int8

const (
	LocalBackend DiskBackend = iota
	S3Backend
)

type DiskType int8

const (
	DataDisk DiskType = iota
	CacheDisk
)

type Disk struct {
	Id      int64  `gorm:"primaryKey"`
	Path    string `gorm:"uniqueIndex"`
	PoolId  int64  `gorm:"index:idx_disk_pool_status_type,priority:1"`
	Backend DiskBackend
	Type    DiskType       `gorm:"index:idx_disk_pool_status_type,priority:3"`
	Status  DiskPoolStatus `gorm:"index:idx_disk_pool_status_type,priority:2"`
}

type Pool struct {
	Id           int64  `gorm:"primaryKey"`
	Name         string `gorm:"uniqueIndex"`
	DataShards   int64
	ParityShards int64
	ChunkSize    int64
	Status       DiskPoolStatus
}

type Stripe struct {
	Id     int64 `gorm:"primaryKey"`
	PoolId int64 `gorm:"index"`
}

type ChunkStatus int8

const (
	ChunkReserved ChunkStatus = iota // 0 — 预分配 slot
	ChunkPending                     // 1 — 数据已写入
	ChunkDirty                       // 2 — 数据已写入，parity 待计算
	ChunkActive                      // 3 — 数据已写入
	ChunkError                       // 4 - 数据损坏
)

type ChunkType int8

const (
	DataChunk ChunkType = iota
	ParityChunk
)

type Chunk struct {
	Id        int64       `gorm:"primaryKey"`
	Status    ChunkStatus `gorm:"index"`
	Path      string
	Size      int64
	Checksum  []byte
	DiskId    int64
	StripeId  int64 `gorm:"index"`
	Type      ChunkType
	Index     int64
	CreatedAt int64 `gorm:"autoCreateTime"`
}

type ChunkOp int8

const (
	ChunkWrite ChunkOp = iota
	ChunkDelete
)

type WriteQueueStatus int8

const (
	QueuePending WriteQueueStatus = iota
	QueueProcessing
)

type WriteQueue struct {
	Id        int64            `gorm:"primaryKey"`
	ChunkId   int64            `gorm:"index"`
	StripeId  int64            `gorm:"index"`
	Op        ChunkOp          `gorm:"index"`
	Status    WriteQueueStatus `gorm:"index;default:0"`
	CreatedAt int64            `gorm:"autoCreateTime;index"`
}

type ReadCache struct {
	Id        int64 `gorm:"primaryKey"`
	ChunkId   int64 `gorm:"index"`
	Path      string
	CreatedAt int64 `gorm:"autoCreateTime;index"`
	UpdatedAt int64 `gorm:"autoUpdateTime;index"`
	ExpiredAt int64 `gorm:"index"`
}

type Setting struct {
	Id        int64  `gorm:"primaryKey"`
	Name      string `gorm:"uniqueIndex"`
	Value     string
	CreatedAt int64 `gorm:"autoCreateTime;index"`
	UpdatedAt int64 `gorm:"autoUpdateTime;index"`
}

type TaskStatus int8

const (
	TaskPending TaskStatus = iota
	TaskRunning
	TaskFinished
)

// Task 记录历史任务，同名任务只能有一个 Pending 或 Running。
type Task struct {
	Id        int64      `gorm:"primaryKey"`
	Name      string     `gorm:"index"`
	Status    TaskStatus `gorm:"index"`
	Message   string
	PoolId    int64
	DiskId    int64
	CreatedAt int64 `gorm:"autoCreateTime;index"`
	UpdatedAt int64 `gorm:"autoUpdateTime"`
}

// ── Share Layer (User/Share/Inode/Version) ──

type UserRole int8

const (
	UserNormal UserRole = iota
	UserAdmin
)

type SharePermission int8

const (
	ShareRead SharePermission = iota
	ShareWrite
	ShareAdmin
)

type InodeKind int8

const (
	InodeFile InodeKind = iota
	InodeDir
	InodeLink
)

type InodeEventType int8

const (
	InodeCreated InodeEventType = iota
	InodeRenamed
	InodeMoved
	InodeDeleted
)

type User struct {
	Id           int64    `gorm:"primaryKey"`
	Username     string   `gorm:"uniqueIndex;not null"`
	PasswordHash string   `gorm:"not null"`
	Role         UserRole `gorm:"default:0"`
	CreatedAt    int64    `gorm:"autoCreateTime"`
}

type Share struct {
	Id               int64   `gorm:"primaryKey"`
	Name             string  `gorm:"uniqueIndex;not null"`
	PoolId           int64   `gorm:"index;not null"`
	Compression      int8    `gorm:"default:0"`
	Encryption       int8    `gorm:"default:0"`
	EncryptionKeyHash []byte  `gorm:"default:null"`
	CreatedBy        int64   `gorm:"index"`
	CreatedAt        int64   `gorm:"autoCreateTime"`
}

// ShareUser 每个用户在每个 share 中的权限。
type ShareUser struct {
	ShareId    int64           `gorm:"primaryKey"`
	UserId     int64           `gorm:"primaryKey"`
	Permission SharePermission `gorm:"default:0"`
}

// Inode 文件/目录/链接的元数据节点。
// parent_id = NULL 时为 share 根目录。
// kind=link 时 LinkId 指向目标 inode。
type Inode struct {
	Id        int64     `gorm:"primaryKey;autoIncrement"`
	ParentId  *int64    `gorm:"index:idx_inode_parent_name,priority:1"`
	Name      string    `gorm:"index:idx_inode_parent_name,priority:2"`
	Kind      InodeKind `gorm:"default:0"`
	Uid       int64     `gorm:"default:0"`
	Gid       int64     `gorm:"default:0"`
	Mtime     int64     `gorm:"autoUpdateTime"`
	Ctime     int64     `gorm:"autoCreateTime"`
	ShareId   int64     `gorm:"index"`
	VersionId *int64    `gorm:"index"`  // 当前文件版本（目录/链接为 NULL）
	LinkId    *int64    `gorm:"index"`  // 链接目标 inode（仅 kind=link）
	Deleted   int8      `gorm:"default:0;index"`
}

// Version 文件的一个版本。
type Version struct {
	Id         int64  `gorm:"primaryKey;autoIncrement"`
	InodeId    int64  `gorm:"uniqueIndex:idx_ver_inode_num,priority:1;not null"`
	VersionNum int64  `gorm:"uniqueIndex:idx_ver_inode_num,priority:2;not null"`
	Size       int64  `gorm:"default:0"`
	Checksum   string `gorm:"default:''"`
	CreatedAt  int64  `gorm:"autoCreateTime"`
}

// VersionChunk 文件版本到存储 chunk 的映射。
// 按 idx 有序排列，读取时 SELECT ... ORDER BY idx 拼接。
type VersionChunk struct {
	VersionId   int64  `gorm:"primaryKey"`
	Idx         int64  `gorm:"primaryKey"`
	ChunkId     int64  `gorm:"index;not null"`
	Size        int64  `gorm:"not null"`
	Checksum    []byte `gorm:"not null"`
	IsEncrypted int8   `gorm:"default:0"`
	Compressed  int8   `gorm:"default:0"`
}

// InodeHistory 记录 inode 的创建、改名、移动、删除事件。
type InodeHistory struct {
	Id           int64          `gorm:"primaryKey;autoIncrement"`
	InodeId      int64          `gorm:"index;not null"`
	EventType    InodeEventType `gorm:"not null"`
	OldName      *string
	NewName      *string
	OldParentId  *int64
	NewParentId  *int64
	CreatedAt    int64          `gorm:"autoCreateTime;index"`
}
