package pool

import (
	"os"
	"path/filepath"

	"github.com/zzc53/zenofs/internal/db"
)

// ChunkWriter 抽象了 chunk 数据的持久化方式。
// 根据 disk.Backend 匹配对应的 Writer。
type ChunkWriter interface {
	WriterType() db.DiskBackend
	Write(disk db.Disk, relPath string, data []byte) error
}

type LocalChunkWriter struct{}

func NewLocalChunkWriter() *LocalChunkWriter {
	return &LocalChunkWriter{}
}

func (w *LocalChunkWriter) WriterType() db.DiskBackend {
	return db.LocalBackend
}

func (w *LocalChunkWriter) Write(disk db.Disk, relPath string, data []byte) error {
	absPath := filepath.Join(disk.Path, relPath)
	if err := os.MkdirAll(filepath.Dir(absPath), 0755); err != nil {
		return err
	}
	return os.WriteFile(absPath, data, 0644)
}
