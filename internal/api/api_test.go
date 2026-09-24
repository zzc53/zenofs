package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/pool"
	"github.com/zzc53/zenofs/internal/testutil"
	"github.com/zzc53/zenofs/internal/token"
)

// newTestServer 起一个真实的 chi 路由 + 临时库 + 本地盘 handler。
func newTestServer(t *testing.T) (*testutil.Env, *httptest.Server) {
	t.Helper()
	env := testutil.New(t)
	pm := pool.New(env.DB, []pool.ChunkHandler{pool.NewLocalChunkHandler()})
	srv := httptest.NewServer(NewRouter(pm, token.NewManager(env.DB), RouterOptions{}))
	t.Cleanup(srv.Close)
	return env, srv
}

// do 发一个请求，body 为 nil 时不带请求体。
func do(t *testing.T, method, url string, body []byte) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

// decode 把响应体解析成 map；同时关闭 body。
func decode(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("解析响应 JSON: %v", err)
	}
	return out
}

// readAll 读走响应体；同时关闭 body。
func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestCreateAndGetPool(t *testing.T) {
	_, srv := newTestServer(t)

	resp := do(t, http.MethodPost, srv.URL+"/api/pools",
		[]byte(`{"name":"p1","chunk_size_kb":1024}`))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("创建池状态码 = %d, want 201", resp.StatusCode)
	}
	created := decode(t, resp)
	id, ok := created["Id"].(float64)
	if !ok || id == 0 {
		t.Fatalf("创建池响应 = %v", created)
	}
	if created["Name"] != "p1" || created["ChunkSize"] != float64(1024) {
		t.Fatalf("创建池响应 = %v", created)
	}

	// 查询
	resp = do(t, http.MethodGet, srv.URL+"/api/pools/1", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("查询池状态码 = %d, want 200", resp.StatusCode)
	}
	got := decode(t, resp)
	if got["Name"] != "p1" {
		t.Fatalf("查询池响应 = %v", got)
	}

	// 不存在的池 → 400 + 结构化错误码
	resp = do(t, http.MethodGet, srv.URL+"/api/pools/9999", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("不存在的池状态码 = %d, want 400", resp.StatusCode)
	}
	if body := decode(t, resp); body["str_code"] == nil {
		t.Fatalf("错误响应缺少 str_code: %v", body)
	}

	// 非法路径参数 / 非法 JSON / 非法 chunk size
	if resp = do(t, http.MethodGet, srv.URL+"/api/pools/abc", nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法 id 状态码 = %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp = do(t, http.MethodPost, srv.URL+"/api/pools", []byte("{not json")); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法 JSON 状态码 = %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp = do(t, http.MethodPost, srv.URL+"/api/pools",
		[]byte(`{"name":"bad","chunk_size_kb":0}`)); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法 chunk size 状态码 = %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

func TestOfflinePoolEndpoint(t *testing.T) {
	env, srv := newTestServer(t)

	resp := do(t, http.MethodPost, srv.URL+"/api/pools", []byte(`{"name":"p","chunk_size_kb":16}`))
	created := decode(t, resp)
	id := int64(created["Id"].(float64))

	resp = do(t, http.MethodPut, srv.URL+"/api/pools/1/offline", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("下线状态码 = %d, want 200", resp.StatusCode)
	}
	if body := decode(t, resp); body["status"] != "offline" {
		t.Fatalf("下线响应 = %v", body)
	}

	var row db.Pool
	if err := env.DB.DB.First(&row, id).Error; err != nil {
		t.Fatal(err)
	}
	if row.Status != db.Offline {
		t.Fatalf("库里的池状态 = %d, want Offline", row.Status)
	}
}

func TestAddDiskAndSwapEndpoints(t *testing.T) {
	env, srv := newTestServer(t)

	resp := do(t, http.MethodPost, srv.URL+"/api/pools", []byte(`{"name":"p","chunk_size_kb":16}`))
	id := int64(decode(t, resp)["Id"].(float64))

	// 加一块数据盘
	body, err := json.Marshal(map[string]any{"path": env.Path("disk0"), "add_parity": false})
	if err != nil {
		t.Fatal(err)
	}
	resp = do(t, http.MethodPost, srv.URL+"/api/pools/1/disks", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("加盘状态码 = %d, want 201", resp.StatusCode)
	}
	disk := decode(t, resp)
	diskId := int64(disk["Id"].(float64))
	if disk["Path"] != env.Path("disk0") {
		t.Fatalf("加盘响应 = %v", disk)
	}

	var row db.Pool
	if err := env.DB.DB.First(&row, id).Error; err != nil {
		t.Fatal(err)
	}
	if row.DataShards != 1 || row.ParityShards != 0 {
		t.Fatalf("分片数 = %d+%d, want 1+0", row.DataShards, row.ParityShards)
	}

	// 换盘：盘转 Repair、池下线
	body, err = json.Marshal(map[string]any{"path": env.Path("disk0-new")})
	if err != nil {
		t.Fatal(err)
	}
	resp = do(t, http.MethodPut, srv.URL+"/api/disks/1/swap", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("换盘状态码 = %d, want 200", resp.StatusCode)
	}
	if body := decode(t, resp); body["status"] != "swapped" {
		t.Fatalf("换盘响应 = %v", body)
	}
	var diskRow db.Disk
	if err := env.DB.DB.First(&diskRow, diskId).Error; err != nil {
		t.Fatal(err)
	}
	if diskRow.Status != db.Repair || diskRow.Path != env.Path("disk0-new") {
		t.Fatalf("换盘后的盘 = %+v", diskRow)
	}
	if err := env.DB.DB.First(&row, id).Error; err != nil {
		t.Fatal(err)
	}
	if row.Status != db.Offline {
		t.Fatalf("换盘后池状态 = %d, want Offline", row.Status)
	}

	// 参数错误
	if resp = do(t, http.MethodPost, srv.URL+"/api/pools/abc/disks", body); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法 poolId 状态码 = %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp = do(t, http.MethodPut, srv.URL+"/api/disks/1/swap", []byte("{bad")); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法 JSON 状态码 = %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

func TestRebuildEndpoint(t *testing.T) {
	_, srv := newTestServer(t)

	resp := do(t, http.MethodPost, srv.URL+"/api/pools", []byte(`{"name":"p","chunk_size_kb":16}`))
	decode(t, resp) // 只关心库里有这个池，id 在后面用固定路径参数

	// 没有 Repair 盘时投递 0 个子任务，但仍然成功
	resp = do(t, http.MethodPost, srv.URL+"/api/pools/1/rebuild", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("重建状态码 = %d, want 200", resp.StatusCode)
	}
	if body := decode(t, resp); body["queued"] != float64(0) {
		t.Fatalf("重建响应 = %v, want queued=0", body)
	}
}

func TestChunkUploadDownloadOverwrite(t *testing.T) {
	env, srv := newTestServer(t)

	// 建池：1 个数据盘 + 1 个校验盘，才能分配条带槽位
	resp := do(t, http.MethodPost, srv.URL+"/api/pools", []byte(`{"name":"p","chunk_size_kb":16}`))
	poolId := int64(decode(t, resp)["Id"].(float64))

	diskBody, err := json.Marshal(map[string]any{"path": env.Path("d0")})
	if err != nil {
		t.Fatal(err)
	}
	if resp = do(t, http.MethodPost, srv.URL+"/api/pools/1/disks", diskBody); resp.StatusCode != http.StatusCreated {
		t.Fatalf("加数据盘状态码 = %d", resp.StatusCode)
	}
	resp.Body.Close()

	parityBody, err := json.Marshal(map[string]any{"path": env.Path("d1"), "add_parity": true})
	if err != nil {
		t.Fatal(err)
	}
	if resp = do(t, http.MethodPost, srv.URL+"/api/pools/1/disks", parityBody); resp.StatusCode != http.StatusCreated {
		t.Fatalf("加校验盘状态码 = %d", resp.StatusCode)
	}
	resp.Body.Close()

	var row db.Pool
	if err := env.DB.DB.First(&row, poolId).Error; err != nil {
		t.Fatal(err)
	}
	if row.DataShards != 1 || row.ParityShards != 1 {
		t.Fatalf("分片数 = %d+%d, want 1+1", row.DataShards, row.ParityShards)
	}

	// 上传
	payload := []byte("hello zenofs chunk")
	resp = do(t, http.MethodPost, srv.URL+"/api/pools/1/chunks", payload)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("上传状态码 = %d, want 201", resp.StatusCode)
	}
	uploaded := decode(t, resp)
	chunkId := int64(uploaded["Id"].(float64))
	if chunkId == 0 || uploaded["Size"] != float64(len(payload)) {
		t.Fatalf("上传响应 = %v", uploaded)
	}

	// 下载原样返回字节
	resp = do(t, http.MethodGet, srv.URL+"/api/chunks/1", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("下载状态码 = %d, want 200", resp.StatusCode)
	}
	if got := readAll(t, resp); !bytes.Equal(got, payload) {
		t.Fatalf("下载内容 = %q", got)
	}

	// 覆写
	replaced := []byte("overwritten")
	resp = do(t, http.MethodPut, srv.URL+"/api/chunks/1", replaced)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("覆写状态码 = %d, want 200", resp.StatusCode)
	}
	if body := decode(t, resp); body["Size"] != float64(len(replaced)) {
		t.Fatalf("覆写响应 = %v", body)
	}
	resp = do(t, http.MethodGet, srv.URL+"/api/chunks/1", nil)
	if got := readAll(t, resp); !bytes.Equal(got, replaced) {
		t.Fatalf("覆写后下载内容 = %q", got)
	}

	// 错误分支：空 body、不存在的 chunk
	if resp = do(t, http.MethodPost, srv.URL+"/api/pools/1/chunks", nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("空 body 上传状态码 = %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp = do(t, http.MethodGet, srv.URL+"/api/chunks/9999", nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("不存在 chunk 状态码 = %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}
