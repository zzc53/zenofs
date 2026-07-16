package pool

import (
	"io"
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
	// ReadAt 从 offset 偏移处读取最多 length 字节。
	ReadAt(disk db.Disk, relPath string, offset int64, length int64) ([]byte, error)
	// WriteAt 从 offset 偏移处写入 data（不覆盖文件其他部分）。
	WriteAt(disk db.Disk, relPath string, offset int64, data []byte) error
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

// ReadAt 打开文件，从 offset 偏移处读取最多 length 字节。
// 返回实际读取的字节数（文件末尾不足 length 时不报错）。
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

// WriteAt 打开文件（不存在则创建），从 offset 处写入 data。
// 不会截断文件，仅覆盖指定偏移区域。
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
