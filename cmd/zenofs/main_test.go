package main

import (
	"net/http"
	"testing"
)

// sftpAddr 决定 SFTP 起不起、起在哪个端口：默认 2222，可显式关闭。
func TestSFTPAddr(t *testing.T) {
	dft := ":2222"
	if got := sftpAddr(""); got != dft {
		t.Errorf("默认端口 = %q，期望 %q", got, dft)
	}
	for _, c := range []struct{ in, want string }{
		{"2222", ":2222"},
		{"2223", ":2223"},
		{" 2224 ", ":2224"}, // 容忍空白
		{"off", ""},
		{"-", ""},
		{"0", ""},
		{"OFF", ":OFF"}, // 大小写敏感：只认小写关闭词
	} {
		if got := sftpAddr(c.in); got != c.want {
			t.Errorf("sftpAddr(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// 默认端口必须是非特权端口（22 需要 root，普通用户起不来）。
func TestSFTPDefaultPortIsUnprivileged(t *testing.T) {
	got := sftpAddr("")
	if got != ":2222" {
		t.Fatalf("SFTP 默认监听地址 = %q，期望 \":2222\"", got)
	}
}

// 大文件传输不能被固定时间掐断：ReadTimeout / WriteTimeout 必须留空
// （它们会从"读完请求头"开始计时，覆盖整个文件传输），
// 但要有 ReadHeaderTimeout 與 IdleTimeout 兜住慢速/闲置连接。
func TestHTTPServerAllowsLongTransfers(t *testing.T) {
	srv := newHTTPServer(":0", http.NewServeMux())
	if srv.ReadTimeout != 0 {
		t.Fatalf("ReadTimeout = %v，会把大文件上传掐断", srv.ReadTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout = %v，会把大文件下载掐断", srv.WriteTimeout)
	}
	if srv.ReadHeaderTimeout == 0 {
		t.Fatalf("应当设置 ReadHeaderTimeout 防慢速请求头")
	}
	if srv.IdleTimeout == 0 {
		t.Fatalf("应当设置 IdleTimeout 回收闲置连接")
	}
}
