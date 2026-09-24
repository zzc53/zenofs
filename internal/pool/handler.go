package pool

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/zzc53/zenofs/internal/db"
)

// ChunkHandler 抽象了 chunk 数据的读写方式。
// 根据 disk.Backend 匹配对应的实现，支持全量和部分读写。
type ChunkHandler interface {
	Type() db.DiskBackend
	// Write 将 data 完整写入 relPath 文件。
	Write(disk db.Disk, relPath string, data []byte) error
	// Read 完整读取 relPath 文件的全部内容。
	Read(disk db.Disk, relPath string) ([]byte, error)
	// Delete 删除 relPath 对应的文件；文件本就不存在时返回 nil。
	Delete(disk db.Disk, relPath string) error
}

// LocalChunkHandler 基于本地文件系统实现 ChunkHandler。
type LocalChunkHandler struct{}

func NewLocalChunkHandler() *LocalChunkHandler {
	return &LocalChunkHandler{}
}

func (w *LocalChunkHandler) Type() db.DiskBackend {
	return db.LocalBackend
}

// Write 创建目录并将 data 写入文件（覆盖式写入）。
func (w *LocalChunkHandler) Write(disk db.Disk, relPath string, data []byte) error {
	absPath := filepath.Join(disk.Path, relPath)
	if err := os.MkdirAll(filepath.Dir(absPath), 0755); err != nil {
		return err
	}
	return os.WriteFile(absPath, data, 0644)
}

// Read 完整读取文件内容返回。
func (w *LocalChunkHandler) Read(disk db.Disk, relPath string) ([]byte, error) {
	return os.ReadFile(filepath.Join(disk.Path, relPath))
}

// Delete 删除文件；文件本就不存在时视为成功。
func (w *LocalChunkHandler) Delete(disk db.Disk, relPath string) error {
	err := os.Remove(filepath.Join(disk.Path, relPath))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
