package pool

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/testutil"
)

func TestGenerateChunkPaths(t *testing.T) {
	const n = 200
	paths, err := generateChunkPaths(n)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != n {
		t.Fatalf("返回 %d 条路径, want %d", len(paths), n)
	}
	seen := make(map[string]bool, n)
	for _, p := range paths {
		if p == "" || filepath.IsAbs(p) {
			t.Fatalf("路径应该是非空的相对路径: %q", p)
		}
		// 形如 YYYY/MM/DD/HH/MM/<random>，共 6 段
		if got := len(bytes.Split([]byte(p), []byte("/"))); got != 6 {
			t.Fatalf("路径 %q 的段数 = %d, want 6", p, got)
		}
		if seen[p] {
			t.Fatalf("路径重复: %q", p)
		}
		seen[p] = true
	}

	// 0 条也返回空切片，不报错
	if empty, err := generateChunkPaths(0); err != nil || len(empty) != 0 {
		t.Fatalf("generateChunkPaths(0) = (%v, %v)", empty, err)
	}
}

func TestGenerateSecureRandomString(t *testing.T) {
	s, err := generateSecureRandomString(8)
	if err != nil {
		t.Fatal(err)
	}
	if len(s) != 8 {
		t.Fatalf("长度 = %d, want 8", len(s))
	}
	other, err := generateSecureRandomString(8)
	if err != nil {
		t.Fatal(err)
	}
	if s == other {
		t.Fatal("两次生成的随机串相同")
	}
}

func TestLocalChunkHandler(t *testing.T) {
	env := testutil.New(t)
	disk := env.NewDisk(1, "local", db.DataDisk)
	h := NewLocalChunkHandler()

	if h.Type() != db.LocalBackend {
		t.Fatalf("Type = %d, want LocalBackend", h.Type())
	}

	data := testutil.RandBytes(3, 1024)
	rel := filepath.Join("2026", "01", "02", "03", "04", "abcdefgh")

	// 写入会自动创建多级目录
	if err := h.Write(disk, rel, data); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := h.Read(disk, rel)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("读回的内容不一致")
	}

	// 覆盖式写入
	replaced := []byte("replaced")
	if err := h.Write(disk, rel, replaced); err != nil {
		t.Fatal(err)
	}
	got, err = h.Read(disk, rel)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, replaced) {
		t.Fatal("覆盖写入没有生效")
	}

	if err := h.Delete(disk, rel); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(disk.Path, rel)); !os.IsNotExist(err) {
		t.Fatalf("文件没有被删除: %v", err)
	}
	// 文件本来就不存在时删除算成功（幂等）
	if err := h.Delete(disk, rel); err != nil {
		t.Fatalf("重复删除报错: %v", err)
	}
	// 读取不存在的文件要报错
	if _, err := h.Read(disk, rel); err == nil {
		t.Fatal("读不存在的文件应该报错")
	}

	// 未注册的后端没有 handler
	if h := (&PoolManager{}).handlerFor(db.LocalBackend); h != nil {
		t.Fatalf("没有 handler 时应该返回 nil: %v", h)
	}
}
