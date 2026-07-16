package pool

import (
	"io"
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
	ReadAt(disk db.Disk, relPath string, offset int64, length int64) ([]byte, error)
	WriteAt(disk db.Disk, relPath string, offset int64, data []byte) error
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

func (w *LocalChunkHandler) ReadAt(disk db.Disk, relPath string, offset int64, length int64) ([]byte, error) {
	f, err := os.Open(filepath.Join(disk.Path, relPath))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	buf := make([]byte, length)
	n, err := f.ReadAt(buf, offset)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf[:n], nil
}

func (w *LocalChunkHandler) WriteAt(disk db.Disk, relPath string, offset int64, data []byte) error {
	absPath := filepath.Join(disk.Path, relPath)
	if err := os.MkdirAll(filepath.Dir(absPath), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(absPath, os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteAt(data, offset)
	return err
}
