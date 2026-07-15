package db

type DiskPoolStatus int8

const (
	Online  DiskPoolStatus = 0
	Offline DiskPoolStatus = 1
	Repair  DiskPoolStatus = 2
)

type DiskBackend int8

const (
	LocalBackend DiskBackend = 0
	S3Backend    DiskBackend = 1
)

type DiskType int8

const (
	DataDisk  DiskType = 0
	CacheDisk DiskType = 1
)

type Disk struct {
	Id      int64          `gorm:"primaryKey"`
	Path    string         `gorm:"uniqueIndex"`
	PoolId  int64          `gorm:"index"`
	Backend DiskBackend    `gorm:"index"`
	Type    DiskType       `gorm:"index"`
	Status  DiskPoolStatus `gorm:"index"`
}

type Pool struct {
	Id           int64          `gorm:"primaryKey"`
	Name         string         `gorm:"uniqueIndex"`
	DataShards   int64          `gorm:"index"`
	ParityShards int64          `gorm:"index"`
	ChunkSize    int64          `gorm:"index"`
	Status       DiskPoolStatus `gorm:"index"`
}

type Stripe struct {
	Id           int64 `gorm:"primaryKey"`
	PoolId       int64 `gorm:"index"`
	DataShards   int64 `gorm:"index"`
	ParityShards int64 `gorm:"index"`
}

type ChunkType int8

const (
	DataChunk   ChunkType = 0
	ParityChunk ChunkType = 1
)

type Chunk struct {
	Id        int64 `gorm:"primaryKey"`
	Path      string
	Size      int64
	Checksum  []byte
	DiskId    int64
	StripeId  int64
	Type      ChunkType
	Index     int64
	CreatedAt int64 `gorm:"autoCreateTime"`
}

type WriteQueue struct {
	Id        int64 `gorm:"primaryKey"`
	ChunkId   int64
	StripeId  int64
	CreatedAt int64 `gorm:"autoCreateTime"`
}

type ReadCache struct {
	Id        int64 `gorm:"primaryKey"`
	ChunkId   int64
	Path      string
	CreatedAt int64 `gorm:"autoCreateTime"`
	UpdatedAt int64 `gorm:"autoUpdateTime"`
	ExpiredAt int64
}
