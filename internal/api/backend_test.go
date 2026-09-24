package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zzc53/zenofs/internal/auth"
	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/pool"
	"github.com/zzc53/zenofs/internal/testutil"
	"github.com/zzc53/zenofs/internal/token"
)

func TestStatusEndpoint(t *testing.T) {
	// 还没有用户的裸服务：前端据此走首启引导
	_, srv, _ := newBareServer(t)
	resp := doNoAuth(t, http.MethodGet, srv.URL+"/api/status", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/status = %d, want 200（应当免认证）", resp.StatusCode)
	}
	got := decode(t, resp)
	if got["bootstrap_needed"] != true {
		t.Fatalf("bootstrap_needed = %v, want true", got["bootstrap_needed"])
	}
	if got["version"] == nil {
		t.Fatalf("响应缺少 version: %v", got)
	}

	// 已经有用户之后应当变成 false
	_, srv2 := newTestServer(t)
	got = decode(t, doNoAuth(t, http.MethodGet, srv2.URL+"/api/status", nil))
	if got["bootstrap_needed"] != false {
		t.Fatalf("bootstrap_needed = %v, want false", got["bootstrap_needed"])
	}
}

func TestAccessTokenQueryParam(t *testing.T) {
	env, srv := newTestServer(t)
	poolID := newPoolWithDisks(t, env, srv)

	// 用 ?access_token= 代替 Authorization 头（浏览器直接打开链接的场景）
	req := newRequest(t, http.MethodGet, srv.URL+"/api/auth/me?access_token="+testAdminToken, nil)
	resp := send(t, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("?access_token= 访问 = %d, want 200", resp.StatusCode)
	}
	if me := decode(t, resp); me["username"] != "root" {
		t.Fatalf("/api/auth/me = %v", me)
	}

	// 坏的 query token → 401
	req = newRequest(t, http.MethodGet, srv.URL+"/api/auth/me?access_token=bogus", nil)
	if resp := send(t, req); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("坏 token = %d, want 401", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// 没有 token → 401
	if resp := doNoAuth(t, http.MethodGet, srv.URL+"/api/auth/me", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无 token = %d, want 401", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// 头里的 token 优先于 query 里的坏 token
	req = newRequest(t, http.MethodGet, srv.URL+"/api/auth/me?access_token=bogus", nil)
	req.Header.Set("Authorization", "Bearer "+testAdminToken)
	if resp := send(t, req); resp.StatusCode != http.StatusOK {
		t.Fatalf("头优先 = %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// 下载场景：带 query token 能直接取到文件
	shareBody, err := json.Marshal(map[string]any{"name": "docs", "pool_id": poolID})
	if err != nil {
		t.Fatal(err)
	}
	resp = do(t, http.MethodPost, srv.URL+"/api/shares", shareBody)
	shareID := int64(decode(t, resp)["id"].(float64))
	putURL := fmt.Sprintf("%s/api/shares/%d/files?path=/a.txt", srv.URL, shareID)
	resp = do(t, http.MethodPut, putURL, []byte("hello"))
	resp.Body.Close()

	getURL := fmt.Sprintf("%s/api/shares/%d/files?path=/a.txt&access_token=%s", srv.URL, shareID, testAdminToken)
	if resp := send(t, newRequest(t, http.MethodGet, getURL, nil)); string(readAll(t, resp)) != "hello" {
		t.Fatalf("带 query token 的下载内容不对")
	}
}

func TestUsageAndTasksEndpoints(t *testing.T) {
	env, srv := newTestServer(t)
	poolID := newPoolWithDisks(t, env, srv)

	shareBody, err := json.Marshal(map[string]any{"name": "docs", "pool_id": poolID, "quota_mb": 100})
	if err != nil {
		t.Fatal(err)
	}
	resp := do(t, http.MethodPost, srv.URL+"/api/shares", shareBody)
	shareID := int64(decode(t, resp)["id"].(float64))

	// usage：配额 100MB，已用 0
	usageURL := fmt.Sprintf("%s/api/shares/%d/usage", srv.URL, shareID)
	usage := decode(t, do(t, http.MethodGet, usageURL, nil))
	if usage["quota_mb"] != float64(100) {
		t.Fatalf("usage = %v", usage)
	}
	if usage["total_bytes"] != float64(100*1024*1024) {
		t.Fatalf("total_bytes = %v（配额 100MB 应当换算成字节）", usage["total_bytes"])
	}
	if usage["used_bytes"] != float64(0) {
		t.Fatalf("used_bytes = %v", usage["used_bytes"])
	}

	// 上传后已用应当增长
	putURL := fmt.Sprintf("%s/api/shares/%d/files?path=/a.txt", srv.URL, shareID)
	resp = do(t, http.MethodPut, putURL, []byte("0123456789"))
	resp.Body.Close()
	usage = decode(t, do(t, http.MethodGet, usageURL, nil))
	if used, _ := usage["used_bytes"].(float64); used < 10 {
		t.Fatalf("上传后 used_bytes = %v，应当 >= 10", usage["used_bytes"])
	}

	// tasks：管理员能看到（空列表也算成功）
	resp = do(t, http.MethodGet, srv.URL+"/api/tasks", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/tasks = %d, want 200", resp.StatusCode)
	}
	if tasks := decodeArray(t, resp); tasks == nil {
		t.Fatalf("tasks 应当是数组")
	}

	// 普通用户看不了
	authMgr, err := auth.NewManager(env.DB)
	if err != nil {
		t.Fatal(err)
	}
	_, bobJWT := newUserAndLogin(t, env, authMgr, "bob")
	if resp := doAs(t, bobJWT, http.MethodGet, srv.URL+"/api/tasks", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("普通用户 /api/tasks = %d, want 403", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// 未授权的 Share 取 usage → 404
	if resp := doAs(t, bobJWT, http.MethodGet, usageURL, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("未授权 usage = %d, want 404", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

// 关掉前端时根路径不返回页面（接口-only 部署）。
func TestWebUIDisabled(t *testing.T) {
	env := testutil.New(t)
	pm := pool.New(env.DB, []pool.ChunkHandler{pool.NewLocalChunkHandler()})
	authMgr, err := auth.NewManager(env.DB)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewRouter(Options{
		PoolManager:  pm,
		Tokens:       token.NewManager(env.DB),
		Auth:         authMgr,
		DisableWebUI: true,
	}))
	t.Cleanup(srv.Close)

	if resp := doNoAuth(t, http.MethodGet, srv.URL+"/", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("关闭前端后 / = %d, want 404", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// /api 不受影响
	if resp := doNoAuth(t, http.MethodGet, srv.URL+"/api/auth/me", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/api/auth/me = %d, want 401", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

// ?access_token= 要被摘出来、放进 context，同时请求行里不能再留 token（否则日志会记录它）。
func TestStripAccessToken(t *testing.T) {
	var seen *http.Request
	h := stripAccessToken(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = r
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/shares/1/files?path=/a.txt&access_token=SECRET", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if seen == nil {
		t.Fatal("handler 没有被调用")
	}
	if strings.Contains(seen.RequestURI, "SECRET") {
		t.Fatalf("RequestURI 里还留着 token: %q", seen.RequestURI)
	}
	if strings.Contains(seen.URL.RawQuery, "SECRET") {
		t.Fatalf("RawQuery 里还留着 token: %q", seen.URL.RawQuery)
	}
	if got := seen.URL.Query().Get("path"); got != "/a.txt" {
		t.Fatalf("其它查询参数被弄丢了: path=%q", got)
	}
	tok, _ := seen.Context().Value(queryTokenKey{}).(string)
	if tok != "SECRET" {
		t.Fatalf("context 里的 token = %q，期望 SECRET", tok)
	}

	// 没有 access_token 的请求应当原样透传
	req = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if seen.RequestURI != "/api/status" {
		t.Fatalf("普通请求被改动了: %q", seen.RequestURI)
	}
}

// 加盘的规则：先选数据盘还是缓存盘；数据盘的 parity、缓存盘的前置条件都有服务端校验。
func TestAddDiskTypeAndParityRules(t *testing.T) {
	env, srv := newTestServer(t)
	resp := do(t, http.MethodPost, srv.URL+"/api/pools", []byte(`{"name":"p","chunk_size_kb":64}`))
	poolID := int64(decode(t, resp)["Id"].(float64))
	addURL := fmt.Sprintf("%s/api/pools/%d/disks", srv.URL, poolID)

	addDisk := func(body map[string]any) (int, map[string]any) {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		resp := do(t, http.MethodPost, addURL, raw)
		if resp.StatusCode >= 300 {
			resp.Body.Close()
			return resp.StatusCode, nil
		}
		return resp.StatusCode, decode(t, resp)
	}
	shards := func() (int64, int64) {
		view := decode(t, do(t, http.MethodGet, fmt.Sprintf("%s/api/pools/%d", srv.URL, poolID), nil))
		return int64(view["DataShards"].(float64)), int64(view["ParityShards"].(float64))
	}

	// 池里一个数据分片都没有时：既不能加 parity，也不能先加缓存盘
	if status, _ := addDisk(map[string]any{"path": env.Path("p0"), "add_parity": true}); status != http.StatusBadRequest {
		t.Fatalf("第一块盘就勾 parity = %d, want 400", status)
	}
	if status, _ := addDisk(map[string]any{"path": env.Path("cache-early"), "type": "cache"}); status != http.StatusBadRequest {
		t.Fatalf("没有数据盘时加缓存盘 = %d, want 400", status)
	}

	// 数据盘（type 省略时默认）→ DataShards=1
	status, view := addDisk(map[string]any{"path": env.Path("d0")})
	if status != http.StatusCreated {
		t.Fatalf("加数据盘 = %d, want 201", status)
	}
	if view["Type"] != float64(db.DataDisk) {
		t.Fatalf("数据盘 Type = %v，期望 %d", view["Type"], db.DataDisk)
	}
	if d, p := shards(); d != 1 || p != 0 {
		t.Fatalf("加数据盘后 data=%d parity=%d，期望 1/0", d, p)
	}

	// 有数据分片之后才能加 parity 分片
	if status, _ := addDisk(map[string]any{"path": env.Path("d1"), "add_parity": true}); status != http.StatusCreated {
		t.Fatalf("加校验分片 = %d, want 201", status)
	}
	if d, p := shards(); d != 1 || p != 1 {
		t.Fatalf("加校验分片后 data=%d parity=%d，期望 1/1", d, p)
	}

	// 缓存盘：不参与条带，也不改变分片计数
	status, view = addDisk(map[string]any{"path": env.Path("cache0"), "type": "cache"})
	if status != http.StatusCreated {
		t.Fatalf("加缓存盘 = %d, want 201", status)
	}
	if view["Type"] != float64(db.CacheDisk) {
		t.Fatalf("缓存盘 Type = %v，期望 %d（CacheDisk）", view["Type"], db.CacheDisk)
	}
	if d, p := shards(); d != 1 || p != 1 {
		t.Fatalf("缓存盘不该改变分片计数: data=%d parity=%d", d, p)
	}

	// 缓存盘不能计入校验分片（明确拒绝，而不是悄悄忽略）
	if status, _ := addDisk(map[string]any{"path": env.Path("cache1"), "type": "cache", "add_parity": true}); status != http.StatusBadRequest {
		t.Fatalf("缓存盘 + parity = %d, want 400", status)
	}
	// 未知用途
	if status, _ := addDisk(map[string]any{"path": env.Path("x"), "type": "nvme"}); status != http.StatusBadRequest {
		t.Fatalf("非法 type = %d, want 400", status)
	}
}

// 加盘的路径规则：空路径、相对路径、重复路径都要给出可读的错误原因，
// 而不是把底层的唯一约束错误兜底成一句没头没脑的 400。
func TestAddDiskPathRules(t *testing.T) {
	env, srv := newTestServer(t)
	resp := do(t, http.MethodPost, srv.URL+"/api/pools", []byte(`{"name":"p","chunk_size_kb":64}`))
	poolID := int64(decode(t, resp)["Id"].(float64))
	addURL := fmt.Sprintf("%s/api/pools/%d/disks", srv.URL, poolID)

	post := func(body map[string]any) (int, map[string]any) {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		resp := do(t, http.MethodPost, addURL, raw)
		return resp.StatusCode, decode(t, resp)
	}
	expectErr := func(desc string, status int, view map[string]any, wantStatus int, wantStrCode string) {
		t.Helper()
		if status != wantStatus {
			t.Fatalf("%s = %d, want %d", desc, status, wantStatus)
		}
		if got := view["str_code"]; got != wantStrCode {
			t.Fatalf("%s 的 str_code = %v, want %s", desc, got, wantStrCode)
		}
		// 前端就是靠这个 message 告诉用户哪里不对，空字符串等于没说
		if msg, _ := view["message"].(string); msg == "" {
			t.Fatalf("%s 的错误 message 是空的", desc)
		}
	}

	// 空路径：不能建出一块"看起来加上了、其实不知道往哪写"的盘
	status, view := post(map[string]any{"path": "", "type": "data"})
	expectErr("空路径加盘", status, view, http.StatusBadRequest, "DISK_BAD_PATH")

	// 相对路径：会随进程工作目录漂移
	status, view = post(map[string]any{"path": "disk0", "type": "data"})
	expectErr("相对路径加盘", status, view, http.StatusBadRequest, "DISK_BAD_PATH")

	// 正常路径可以加
	diskPath := env.Path("d0")
	if status, view = post(map[string]any{"path": diskPath}); status != http.StatusCreated {
		t.Fatalf("加数据盘 = %d, want 201（%v）", status, view)
	}

	// 同一个路径再加一次：409，并说清是路径被占用了
	status, view = post(map[string]any{"path": diskPath})
	expectErr("重复路径加盘", status, view, http.StatusConflict, "DISK_EXIST")

	// 被拒绝的这几次不该在池里留下分片
	pool := decode(t, do(t, http.MethodGet, fmt.Sprintf("%s/api/pools/%d", srv.URL, poolID), nil))
	if pool["DataShards"] != float64(1) || pool["ParityShards"] != float64(0) {
		t.Fatalf("池分片数 = %v/%v，期望 1/0", pool["DataShards"], pool["ParityShards"])
	}

	// 缓存盘也要守同样的路径规则
	cachePath := env.Path("cache0")
	if status, view = post(map[string]any{"path": cachePath, "type": "cache"}); status != http.StatusCreated {
		t.Fatalf("加缓存盘 = %d, want 201（%v）", status, view)
	}
	status, view = post(map[string]any{"path": cachePath, "type": "cache"})
	expectErr("重复路径加缓存盘", status, view, http.StatusConflict, "DISK_EXIST")
}
