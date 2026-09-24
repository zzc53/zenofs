package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zzc53/zenofs/internal/auth"
	"github.com/zzc53/zenofs/internal/testutil"
)

// newPoolWithDisks 通过 API 建一个"1 data + 1 parity"的池，返回池 id。
func newPoolWithDisks(t *testing.T, env *testutil.Env, srv *httptest.Server) int64 {
	t.Helper()
	resp := do(t, http.MethodPost, srv.URL+"/api/pools", []byte(`{"name":"p","chunk_size_kb":64}`))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("建池 = %d, want 201", resp.StatusCode)
	}
	poolID := int64(decode(t, resp)["Id"].(float64))
	for _, d := range []struct {
		path   string
		parity bool
	}{
		{env.Path("d0"), false},
		{env.Path("d1"), true},
	} {
		body, err := json.Marshal(map[string]any{"path": d.path, "add_parity": d.parity})
		if err != nil {
			t.Fatal(err)
		}
		resp := do(t, http.MethodPost, fmt.Sprintf("%s/api/pools/%d/disks", srv.URL, poolID), body)
		resp.Body.Close()
	}
	return poolID
}

// 加密 Share 的完整生命周期：创建即加密并解锁 → 读写 → lock 后 423 → 错误口令 403 →
// 正确口令重新解锁后数据还在。密钥只在进程内存里（LUKS 语义）。
func TestEncryptedShareUnlockLock(t *testing.T) {
	env, srv := newTestServer(t)
	poolID := newPoolWithDisks(t, env, srv)

	// 创建加密 Share：给 password 就启用加密并立刻解锁
	createBody, err := json.Marshal(map[string]any{
		"name": "vault", "pool_id": poolID, "password": "share-pw-123",
	})
	if err != nil {
		t.Fatal(err)
	}
	resp := do(t, http.MethodPost, srv.URL+"/api/shares", createBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("建加密 Share = %d, want 201", resp.StatusCode)
	}
	created := decode(t, resp)
	shareID := int64(created["id"].(float64))
	if created["encrypted"] != true || created["unlocked"] != true {
		t.Fatalf("建加密 Share 响应 = %v（期望 encrypted/unlocked 都为 true）", created)
	}

	// 已解锁：能写能读
	payload := []byte("top secret payload")
	fileURL := fmt.Sprintf("%s/api/shares/%d/files?path=/secret.txt", srv.URL, shareID)
	if resp := do(t, http.MethodPut, fileURL, payload); resp.StatusCode != http.StatusCreated {
		t.Fatalf("加密 Share 上传 = %d, want 201", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := do(t, http.MethodGet, fileURL, nil); string(readAll(t, resp)) != string(payload) {
		t.Fatalf("加密 Share 下载内容不对")
	}

	// lock：密钥从内存消失，读写都 423 Locked
	if resp := do(t, http.MethodPost, fmt.Sprintf("%s/api/shares/%d/lock", srv.URL, shareID), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("lock = %d, want 200", resp.StatusCode)
	} else if view := decode(t, resp); view["unlocked"] != false || view["encrypted"] != true {
		t.Fatalf("lock 响应 = %v", view)
	}
	if resp := do(t, http.MethodGet, fileURL, nil); resp.StatusCode != http.StatusLocked {
		t.Fatalf("上锁后下载 = %d, want 423", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := do(t, http.MethodPut, fileURL, []byte("x")); resp.StatusCode != http.StatusLocked {
		t.Fatalf("上锁后上传 = %d, want 423", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// 口令错 → 403；未启用加密的 Share 去 unlock → 400
	if resp := do(t, http.MethodPost, fmt.Sprintf("%s/api/shares/%d/unlock", srv.URL, shareID),
		[]byte(`{"password":"wrong-pw"}`)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("错误口令 unlock = %d, want 403", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// 正确口令 → 重新解锁，数据完好
	if resp := do(t, http.MethodPost, fmt.Sprintf("%s/api/shares/%d/unlock", srv.URL, shareID),
		[]byte(`{"password":"share-pw-123"}`)); resp.StatusCode != http.StatusOK {
		t.Fatalf("unlock = %d, want 200", resp.StatusCode)
	} else if view := decode(t, resp); view["unlocked"] != true {
		t.Fatalf("unlock 响应 = %v", view)
	}
	if resp := do(t, http.MethodGet, fileURL, nil); string(readAll(t, resp)) != string(payload) {
		t.Fatalf("重新解锁后内容不对")
	}

	// 建 Share 时只给 encryption 不给 password → 400（避免建出谁都读不了的 Share）
	badBody, err := json.Marshal(map[string]any{
		"name": "nopass", "pool_id": poolID, "encryption": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp := do(t, http.MethodPost, srv.URL+"/api/shares", badBody); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("只给 encryption = %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// 口令太短也拒绝
	shortBody, err := json.Marshal(map[string]any{"name": "short", "pool_id": poolID, "password": "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if resp := do(t, http.MethodPost, srv.URL+"/api/shares", shortBody); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("短口令 = %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// 未加密的 Share 调 unlock → 400
	plainBody, err := json.Marshal(map[string]any{"name": "plain", "pool_id": poolID})
	if err != nil {
		t.Fatal(err)
	}
	resp = do(t, http.MethodPost, srv.URL+"/api/shares", plainBody)
	plainView := decode(t, resp)
	plainID := int64(plainView["id"].(float64))
	if plainView["encrypted"] != false || plainView["unlocked"] != false {
		t.Fatalf("未加密 Share 视图 = %v", plainView)
	}
	if resp := do(t, http.MethodPost, fmt.Sprintf("%s/api/shares/%d/unlock", srv.URL, plainID),
		[]byte(`{"password":"whatever"}`)); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("未加密 Share unlock = %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

// 解锁/上锁只允许管理员；普通用户即使有该 Share 的 admin 授权也不行。
func TestUnlockRequiresAdmin(t *testing.T) {
	env, srv := newTestServer(t)
	authMgr, err := auth.NewManager(env.DB)
	if err != nil {
		t.Fatal(err)
	}
	poolID := newPoolWithDisks(t, env, srv)

	createBody, err := json.Marshal(map[string]any{
		"name": "vault2", "pool_id": poolID, "password": "share-pw-123",
	})
	if err != nil {
		t.Fatal(err)
	}
	resp := do(t, http.MethodPost, srv.URL+"/api/shares", createBody)
	shareID := int64(decode(t, resp)["id"].(float64))

	// 普通用户：给到 admin 授权，但仍然不能 unlock / lock
	bobID, bobJWT := newUserAndLogin(t, env, authMgr, "bob")
	grantBody, err := json.Marshal(map[string]any{"user_id": bobID, "permission": "admin"})
	if err != nil {
		t.Fatal(err)
	}
	resp = do(t, http.MethodPost, fmt.Sprintf("%s/api/shares/%d/users", srv.URL, shareID), grantBody)
	resp.Body.Close()

	if resp := doAs(t, bobJWT, http.MethodPost, fmt.Sprintf("%s/api/shares/%d/unlock", srv.URL, shareID),
		[]byte(`{"password":"share-pw-123"}`)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("普通用户 unlock = %d, want 403", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := doAs(t, bobJWT, http.MethodPost, fmt.Sprintf("%s/api/shares/%d/lock", srv.URL, shareID), nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("普通用户 lock = %d, want 403", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// 但解锁之后（管理员已经开着），有 write 授权的人可以正常读写
	lockResp := do(t, http.MethodPost, fmt.Sprintf("%s/api/shares/%d/lock", srv.URL, shareID), nil)
	lockResp.Body.Close()
	unlockResp := do(t, http.MethodPost, fmt.Sprintf("%s/api/shares/%d/unlock", srv.URL, shareID),
		[]byte(`{"password":"share-pw-123"}`))
	unlockResp.Body.Close()
	if resp := doAs(t, bobJWT, http.MethodGet, fmt.Sprintf("%s/api/shares/%d/list?path=/", srv.URL, shareID), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("解锁后普通用户列目录 = %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}
