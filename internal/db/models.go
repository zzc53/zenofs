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
	ChunkUpdated                     // 2 — 数据已更新
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
	CreatedAt int64      `gorm:"autoCreateTime;index"`
	UpdatedAt int64      `gorm:"autoUpdateTime"`
}
