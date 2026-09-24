package api

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/zzc53/zenofs/internal/auth"
	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/testutil"
)

// newPoolWithParity 建一个 2 data + 1 parity 的池，返回它的 id。
func newPoolWithParity(t *testing.T, env *testutil.Env, srv *httptest.Server) int64 {
	t.Helper()
	poolID := int64(decode(t, do(t, http.MethodPost, srv.URL+"/api/pools",
		[]byte(`{"name":"p","chunk_size_kb":64}`)))["Id"].(float64))

	addURL := fmt.Sprintf("%s/api/pools/%d/disks", srv.URL, poolID)
	for i, body := range []string{
		fmt.Sprintf(`{"path":%q}`, env.Path("d0")),
		fmt.Sprintf(`{"path":%q}`, env.Path("d1")),
		fmt.Sprintf(`{"path":%q,"add_parity":true}`, env.Path("d2")),
	} {
		resp := do(t, http.MethodPost, addURL, []byte(body))
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("加第 %d 块盘 = %d, want 201", i+1, resp.StatusCode)
		}
		resp.Body.Close()
	}
	return poolID
}

// dirEntries 列出一个目录下的条目。
func dirEntries(t *testing.T, srv *httptest.Server, shareID int64, dir string) []map[string]any {
	t.Helper()
	resp := do(t, http.MethodGet,
		fmt.Sprintf("%s/api/shares/%d/list?path=%s", srv.URL, shareID, url.QueryEscape(dir)), nil)
	raw, _ := decode(t, resp)["entries"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// inodeIDOf 从目录列表里找出某个路径的 inode id。
func inodeIDOf(t *testing.T, srv *httptest.Server, shareID int64, p string) int64 {
	t.Helper()
	dir, name := "/", p
	if i := strings.LastIndex(p, "/"); i >= 0 {
		if dir = p[:i]; dir == "" {
			dir = "/"
		}
		name = p[i+1:]
	}
	for _, e := range dirEntries(t, srv, shareID, dir) {
		if e["name"] == name {
			return int64(e["id"].(float64))
		}
	}
	t.Fatalf("目录 %s 里找不到 %s", dir, name)
	return 0
}

// historyEvents 取某个 inode 的变更事件名列表。
func historyEvents(t *testing.T, srv *httptest.Server, shareID, inodeID int64) []string {
	t.Helper()
	rows := decodeArray(t, do(t, http.MethodGet,
		fmt.Sprintf("%s/api/shares/%d/inodes/%d/history", srv.URL, shareID, inodeID), nil))
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		if name, ok := row["event"].(string); ok {
			out = append(out, name)
		}
	}
	return out
}

func hasEvent(events []string, want string) bool {
	for _, e := range events {
		if e == want {
			return true
		}
	}
	return false
}

// 版本历史：每次写入生成一个新版本；恢复到旧版本要把内容换回去。
func TestVersionsAndRestore(t *testing.T) {
	env, srv := newTestServer(t)
	poolID := newPoolWithParity(t, env, srv)
	shareID := createShareForTest(t, srv, "docs", poolID)

	fileURL := fmt.Sprintf("%s/api/shares/%d/files?path=/doc.txt", srv.URL, shareID)
	v1 := []byte("version one")
	v2 := []byte("version two, much longer than the first one")
	for _, payload := range [][]byte{v1, v2} {
		resp := do(t, http.MethodPut, fileURL, payload)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("上传 = %d, want 201", resp.StatusCode)
		}
		resp.Body.Close()
	}

	inodeID := inodeIDOf(t, srv, shareID, "/doc.txt")
	versionsURL := fmt.Sprintf("%s/api/shares/%d/inodes/%d/versions", srv.URL, shareID, inodeID)

	versions := decodeArray(t, do(t, http.MethodGet, versionsURL, nil))
	if len(versions) != 2 {
		t.Fatalf("版本数 = %d, want 2", len(versions))
	}
	// 新的在前，且只有最新的那个是 current
	if versions[0]["is_current"] != true || versions[1]["is_current"] != false {
		t.Fatalf("当前版本标记不对: %+v", versions)
	}
	if got := int64(versions[0]["size"].(float64)); got != int64(len(v2)) {
		t.Fatalf("最新版本 size = %d, want %d", got, len(v2))
	}

	// 恢复到最早那个版本
	oldest := int64(versions[len(versions)-1]["id"].(float64))
	resp := do(t, http.MethodPost,
		fmt.Sprintf("%s/api/shares/%d/versions/%d/restore", srv.URL, shareID, oldest), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("恢复版本 = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// 内容必须变回 v1
	if got := readAll(t, do(t, http.MethodGet, fileURL, nil)); !bytes.Equal(got, v1) {
		t.Fatalf("恢复后读到 %q, want %q", got, v1)
	}

	// 当前版本标记要跟着换
	for _, v := range decodeArray(t, do(t, http.MethodGet, versionsURL, nil)) {
		want := int64(v["id"].(float64)) == oldest
		if v["is_current"] != want {
			t.Fatalf("恢复后当前版本标记不对: %+v", v)
		}
	}

	// 不存在的版本 → 404
	if resp := do(t, http.MethodPost,
		fmt.Sprintf("%s/api/shares/%d/versions/999999/restore", srv.URL, shareID), nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("恢复不存在的版本 = %d, want 404", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

// 变更记录：改名 / 移动 / 删除 / 恢复都要留痕，而且回收站里的条目也能查到。
func TestInodeHistoryRecordsEvents(t *testing.T) {
	env, srv := newTestServer(t)
	poolID := newPoolWithParity(t, env, srv)
	shareID := createShareForTest(t, srv, "docs", poolID)

	if resp := do(t, http.MethodPut,
		fmt.Sprintf("%s/api/shares/%d/files?path=/note.txt", srv.URL, shareID), []byte("hello")); resp.StatusCode != http.StatusCreated {
		t.Fatalf("上传 = %d, want 201", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	inodeID := inodeIDOf(t, srv, shareID, "/note.txt")

	rename := func(from, to string) {
		t.Helper()
		resp := do(t, http.MethodPost, fmt.Sprintf("%s/api/shares/%d/rename", srv.URL, shareID),
			[]byte(fmt.Sprintf(`{"from":%q,"to":%q}`, from, to)))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("rename %s → %s = %d", from, to, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// 建个目录，先把文件改名、再把它移进去
	if resp := do(t, http.MethodPost, fmt.Sprintf("%s/api/shares/%d/folders", srv.URL, shareID),
		[]byte(`{"path":"/sub"}`)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("建目录 = %d, want 201", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	rename("/note.txt", "/renamed.txt")        // 改名
	rename("/renamed.txt", "/sub/renamed.txt") // 移动

	events := historyEvents(t, srv, shareID, inodeID)
	for _, want := range []string{"renamed", "moved"} {
		if !hasEvent(events, want) {
			t.Fatalf("历史里缺 %q: %v", want, events)
		}
	}

	// 删除 → 进回收站，历史仍然查得到，并多一条 deleted
	if resp := do(t, http.MethodDelete,
		fmt.Sprintf("%s/api/shares/%d/files?path=/sub/renamed.txt", srv.URL, shareID), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("删除 = %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if events = historyEvents(t, srv, shareID, inodeID); !hasEvent(events, "deleted") {
		t.Fatalf("删除后历史里缺 deleted: %v", events)
	}

	// 从回收站恢复 → 多一条 restored
	if resp := do(t, http.MethodPost,
		fmt.Sprintf("%s/api/shares/%d/recycle/%d/restore", srv.URL, shareID, inodeID), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("恢复 = %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if events = historyEvents(t, srv, shareID, inodeID); !hasEvent(events, "restored") {
		t.Fatalf("恢复后历史里缺 restored: %v", events)
	}
}

// 只读授权能看版本，但不能拿它恢复（写权限检查）。
func TestRestoreVersionNeedsWritePermission(t *testing.T) {
	env, srv := newTestServer(t)
	poolID := newPoolWithParity(t, env, srv)

	// 管理员的 Share 里放一个文件，好生成版本
	shareID := createShareForTest(t, srv, "docs", poolID)
	if resp := do(t, http.MethodPut,
		fmt.Sprintf("%s/api/shares/%d/files?path=/a.txt", srv.URL, shareID), []byte("x")); resp.StatusCode != http.StatusCreated {
		t.Fatalf("上传 = %d, want 201", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	inodeID := inodeIDOf(t, srv, shareID, "/a.txt")
	if v := decodeArray(t, do(t, http.MethodGet,
		fmt.Sprintf("%s/api/shares/%d/inodes/%d/versions", srv.URL, shareID, inodeID), nil)); len(v) == 0 {
		t.Fatal("没有版本")
	}
	// bob 拿到只读授权
	authMgr, err := auth.NewManager(env.DB)
	if err != nil {
		t.Fatal(err)
	}
	bobID, bobJWT := newUserAndLogin(t, env, authMgr, "bob")
	roShare := env.NewShare(poolID, testutil.ShareOpts{
		Name: "ro", UserID: bobID, Permission: db.ShareRead,
	})
	// 权限一律看 share_users（owner 没有特权），所以管理员也得显式授权才能写
	if err := env.DB.DB.Create(&db.ShareUser{
		ShareId: roShare.Id, UserId: 1, Permission: db.ShareWrite,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if resp := do(t, http.MethodPut,
		fmt.Sprintf("%s/api/shares/%d/files?path=/b.txt", srv.URL, roShare.Id), []byte("y")); resp.StatusCode != http.StatusCreated {
		t.Fatalf("往只读 Share 写 = %d, want 201（管理员不受限）", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	roInode := inodeIDOf(t, srv, roShare.Id, "/b.txt")
	roVersions := decodeArray(t, doAs(t, bobJWT, http.MethodGet,
		fmt.Sprintf("%s/api/shares/%d/inodes/%d/versions", srv.URL, roShare.Id, roInode), nil))
	if len(roVersions) == 0 {
		t.Fatal("bob 应当能看版本")
	}

	// 但恢复要被拒：403
	roVersion := int64(roVersions[0]["id"].(float64))
	resp := doAs(t, bobJWT, http.MethodPost,
		fmt.Sprintf("%s/api/shares/%d/versions/%d/restore", srv.URL, roShare.Id, roVersion), nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("只读用户恢复版本 = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()
}
