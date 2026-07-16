package pool

import (
	"os"
	"path/filepath"

	"github.com/zzc53/zenofs/internal/db"
)

// ChunkHandler 抽象了 chunk 数据的读写方式。
// 根据 disk.Backend 匹配对应的实现。
type ChunkHandler interface {
	Type() db.DiskBackend
	Write(disk db.Disk, relPath string, data []byte) error
	Read(disk db.Disk, relPath string) ([]byte, error)
}

type LocalChunkHandler struct{}

func NewLocalChunkHandler() *LocalChunkHandler {
	return &LocalChunkHandler{}
}

func (w *LocalChunkHandler) Type() db.DiskBackend {
	return db.LocalBackend
}

func (w *LocalChunkHandler) Write(disk db.Disk, relPath string, data []byte) error {
	absPath := filepath.Join(disk.Path, relPath)
	if err := os.MkdirAll(filepath.Dir(absPath), 0755); err != nil {
		return err
	}
	return os.WriteFile(absPath, data, 0644)
}

func (w *LocalChunkHandler) Read(disk db.Disk, relPath string) ([]byte, error) {
	return os.ReadFile(filepath.Join(disk.Path, relPath))
}
