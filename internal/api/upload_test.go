package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzc53/zenofs/internal/db"
)

// createShareForTest 建一个共享并返回 id。
func createShareForTest(t *testing.T, srv *httptest.Server, name string, poolID int64) int64 {
	t.Helper()
	body, err := json.Marshal(map[string]any{"name": name, "pool_id": poolID})
	if err != nil {
		t.Fatal(err)
	}
	resp := do(t, http.MethodPost, srv.URL+"/api/shares", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("建共享 = %d, want 201", resp.StatusCode)
	}
	return int64(decode(t, resp)["id"].(float64))
}

// listNames 列出目录里的文件名。
func listNames(t *testing.T, srv *httptest.Server, shareID int64) []string {
	t.Helper()
	resp := do(t, http.MethodGet, fmt.Sprintf("%s/api/shares/%d/list?path=/", srv.URL, shareID), nil)
	body := decode(t, resp)
	raw, _ := body["entries"].([]any)
	names := make([]string, 0, len(raw))
	for _, e := range raw {
		if m, ok := e.(map[string]any); ok {
			if name, ok := m["name"].(string); ok {
				names = append(names, name)
			}
		}
	}
	return names
}

// 上传被掐断（客户端掉线、连接中断）时，不该在列表里留下一个"看着正常、
// 其实不完整"的文件。这里用裸 TCP 精确构造"chunked 请求体只发一半就断开"，
// 也就是用户传大文件时中途失败的那种情形。
func TestUploadTruncatedBodyLeavesNoFile(t *testing.T) {
	env, srv := newTestServer(t)
	poolID := newPoolWithDisks(t, env, srv)
	shareID := createShareForTest(t, srv, "docs", poolID)

	addr := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	head := fmt.Sprintf("PUT /api/shares/%d/files?path=/broken.bin HTTP/1.1\r\n"+
		"Host: %s\r\nAuthorization: Bearer %s\r\nTransfer-Encoding: chunked\r\n\r\n",
		shareID, addr, testAdminToken)
	if _, err := conn.Write([]byte(head)); err != nil {
		t.Fatalf("write head: %v", err)
	}
	// 声明 1024 字节的块，实际只发 512 字节就断开
	if _, err := conn.Write([]byte("400\r\n" + strings.Repeat("x", 512))); err != nil {
		t.Logf("write body（服务器可能已关连接，可忽略）: %v", err)
	}
	conn.Close()

	// 服务器处理完后，文件不该留在目录里
	deadline := time.Now().Add(3 * time.Second)
	for {
		names := listNames(t, srv, shareID)
		if len(names) == 0 {
			return // ✓ 半成品被清掉了
		}
		if time.Now().After(deadline) {
			t.Fatalf("被掐断的上传留下了文件: %v", names)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// 正常上传（完整 body）当然要留下文件——确认上面的清理逻辑不是"什么都不存"。
func TestUploadCompleteKeepsFile(t *testing.T) {
	env, srv := newTestServer(t)
	poolID := newPoolWithDisks(t, env, srv)
	shareID := createShareForTest(t, srv, "docs", poolID)

	payload := bytes.Repeat([]byte("payload"), 100)
	url := fmt.Sprintf("%s/api/shares/%d/files?path=/ok.bin", srv.URL, shareID)
	if resp := do(t, http.MethodPut, url, payload); resp.StatusCode != http.StatusCreated {
		t.Fatalf("正常上传 = %d, want 201", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if got := readAll(t, do(t, http.MethodGet, url, nil)); !bytes.Equal(got, payload) {
		t.Fatalf("读回内容不一致（%d 字节）", len(got))
	}
}

// 上传空文件是合法的：不该被当成"中断"。
func TestUploadEmptyFileIsAllowed(t *testing.T) {
	env, srv := newTestServer(t)
	poolID := newPoolWithDisks(t, env, srv)
	shareID := createShareForTest(t, srv, "docs", poolID)

	url := fmt.Sprintf("%s/api/shares/%d/files?path=/empty.bin", srv.URL, shareID)
	if resp := do(t, http.MethodPut, url, []byte{}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("空文件上传 = %d, want 201", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if names := listNames(t, srv, shareID); len(names) != 1 || names[0] != "empty.bin" {
		t.Fatalf("空文件应当留在目录里: %v", names)
	}
}

// countFiles 递归统计目录里的文件数（缓存盘目录用）。
func countFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 目录不存在等：当 0 个文件
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 %s: %v", dir, err)
	}
	return n
}

// 缓存盘可以删除：盘上的缓存文件与 read_caches 记录要一起清掉；数据盘不允许删。
func TestDeleteCacheDiskEndpoint(t *testing.T) {
	env, srv := newTestServer(t)
	resp := do(t, http.MethodPost, srv.URL+"/api/pools", []byte(`{"name":"p","chunk_size_kb":256}`))
	poolID := int64(decode(t, resp)["Id"].(float64))
	addURL := fmt.Sprintf("%s/api/pools/%d/disks", srv.URL, poolID)

	addDisk := func(body map[string]any) map[string]any {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		resp := do(t, http.MethodPost, addURL, raw)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("加盘 = %d, want 201", resp.StatusCode)
		}
		return decode(t, resp)
	}
	deleteDisk := func(id int64) *http.Response {
		return do(t, http.MethodDelete, fmt.Sprintf("%s/api/disks/%d", srv.URL, id), nil)
	}

	dataDisk := addDisk(map[string]any{"path": env.Path("d0")})
	addDisk(map[string]any{"path": env.Path("d1"), "add_parity": true})
	cacheDir := env.Path("cache0")
	cacheDisk := addDisk(map[string]any{"path": cacheDir, "type": "cache"})
	cacheID := int64(cacheDisk["Id"].(float64))

	// 反复读同一个文件，把 chunk 推上缓存盘（阈值是访问 5 次之后落盘）
	shareID := createShareForTest(t, srv, "docs", poolID)
	payload := bytes.Repeat([]byte("cache me please"), 4096)
	fileURL := fmt.Sprintf("%s/api/shares/%d/files?path=/hot.bin", srv.URL, shareID)
	if resp := do(t, http.MethodPut, fileURL, payload); resp.StatusCode != http.StatusCreated {
		t.Fatalf("上传 = %d, want 201", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	for i := 0; i < 8; i++ {
		resp := do(t, http.MethodGet, fileURL, nil)
		resp.Body.Close()
	}

	var cacheRows int64
	if err := env.DB.DB.Model(&db.ReadCache{}).Where("disk_id = ?", cacheID).Count(&cacheRows).Error; err != nil {
		t.Fatal(err)
	}
	var cachedRows int64
	if err := env.DB.DB.Model(&db.ReadCache{}).Where("disk_id = ? AND status = ?", cacheID, db.Cached).Count(&cachedRows).Error; err != nil {
		t.Fatal(err)
	}
	if cachedRows == 0 {
		t.Fatalf("反复读之后 chunk 应当落到缓存盘上（read_caches 里没有 Cached 记录）")
	}
	filesBefore := countFiles(t, cacheDir)
	if filesBefore == 0 {
		t.Fatalf("缓存盘目录里应当有缓存文件")
	}

	// 删除缓存盘
	resp = deleteDisk(cacheID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("删缓存盘 = %d, want 200", resp.StatusCode)
	}
	body := decode(t, resp)
	if removed, _ := body["removed_caches"].(float64); int(removed) != int(cacheRows) {
		t.Fatalf("removed_caches = %v，期望 %d", body["removed_caches"], cacheRows)
	}

	// 记录、盘、文件都该没了
	var left int64
	env.DB.DB.Model(&db.ReadCache{}).Where("disk_id = ?", cacheID).Count(&left)
	if left != 0 {
		t.Fatalf("read_caches 里还剩 %d 条记录", left)
	}
	env.DB.DB.Model(&db.Disk{}).Where("id = ?", cacheID).Count(&left)
	if left != 0 {
		t.Fatalf("disks 里还剩这条盘记录")
	}
	if files := countFiles(t, cacheDir); files != 0 {
		t.Fatalf("缓存盘目录里还剩 %d 个文件", files)
	}

	// 重复删除 → 404（盘已经不在了）
	if resp := deleteDisk(cacheID); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("重复删除 = %d, want 404", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// 数据盘不允许删（唯一数据副本）
	if resp := deleteDisk(int64(dataDisk["Id"].(float64))); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("删数据盘 = %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// 缓存盘没了之后，读写照常（只是不再缓存）
	if got := readAll(t, do(t, http.MethodGet, fileURL, nil)); !bytes.Equal(got, payload) {
		t.Fatalf("删掉缓存盘后读回内容不一致")
	}
}

// 并发上传：多个请求同时写 chunk 时结果要正确（都成功、内容都对）。
//
// 注意：这个测试**不能**用来守 database is locked（它修复前后都会通过，
// 因为 HTTP 这一层没把事务卡到同一个瞬间）。真正钉住那个 bug 的是
// pool 包的 TestConcurrentAddChunksNoLockError。
func TestConcurrentUploadsNoLockFailure(t *testing.T) {
	env, srv := newTestServer(t)
	resp := do(t, http.MethodPost, srv.URL+"/api/pools", []byte(`{"name":"p","chunk_size_kb":256}`))
	poolID := int64(decode(t, resp)["Id"].(float64))
	addURL := fmt.Sprintf("%s/api/pools/%d/disks", srv.URL, poolID)

	for i, body := range []string{
		fmt.Sprintf(`{"path":%q}`, env.Path("d0")),
		fmt.Sprintf(`{"path":%q,"add_parity":true}`, env.Path("d1")),
	} {
		resp := do(t, http.MethodPost, addURL, []byte(body))
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("加第 %d 块盘 = %d, want 201", i+1, resp.StatusCode)
		}
		resp.Body.Close()
	}
	shareID := createShareForTest(t, srv, "docs", poolID)

	const n = 8
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			payload := bytes.Repeat([]byte{byte('a' + i)}, 64*1024)
			resp := do(t, http.MethodPut,
				fmt.Sprintf("%s/api/shares/%d/files?path=/f%d.bin", srv.URL, shareID, i), payload)
			codes[i] = resp.StatusCode
			resp.Body.Close()
		}(i)
	}
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusCreated {
			t.Fatalf("并发上传第 %d 个 = %d, want 201（多半是 database is locked）", i, code)
		}
	}
	// 并发写完之后，所有文件都该读得回来
	for i := 0; i < n; i++ {
		want := bytes.Repeat([]byte{byte('a' + i)}, 64*1024)
		got := readAll(t, do(t, http.MethodGet,
			fmt.Sprintf("%s/api/shares/%d/files?path=/f%d.bin", srv.URL, shareID, i), nil))
		if !bytes.Equal(got, want) {
			t.Fatalf("第 %d 个文件读回内容不一致（%d 字节 vs %d 字节）", i, len(got), len(want))
		}
	}
}
