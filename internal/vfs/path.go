package vfs

import (
	"path"
	"strings"
)

// ---------------------------------------------------------------
// 路径处理
// ---------------------------------------------------------------
//
// VFS 内一律使用 '/' 分隔的绝对路径，"/" 表示挂载根（即 Share 根目录）。
// 各协议的分隔符差异（SMB 的 '\'、WebDAV 的 URL 转义）由协议层转换。
//
// 路径解析是纯词法的：折叠重复的 '/'、去掉 '.'、就地解析 '..' 且不会越过根。
// 不跟随符号链接展开 '..'（与 POSIX 有细微差别，但语义更可预测）。

// cleanPath 规范化路径；相对路径或空路径返回 ErrInvalid。
func cleanPath(p string) (string, error) {
	if p == "" || !strings.HasPrefix(p, "/") {
		return "", ErrInvalid
	}
	return path.Clean(p), nil
}

// pathSegments 把已规范化的绝对路径拆成各级名称；根目录返回空切片。
func pathSegments(p string) []string {
	if p == "/" {
		return nil
	}
	return strings.Split(strings.TrimPrefix(p, "/"), "/")
}

// parentPath 返回父目录路径；根目录的父目录仍是根目录。
func parentPath(p string) string {
	if dir := path.Dir(p); dir != "." {
		return dir
	}
	return "/"
}

// baseName 返回路径最后一段；根目录返回 "/"。
func baseName(p string) string {
	if p == "/" {
		return "/"
	}
	return path.Base(p)
}

// joinPath 把父目录与本级名称拼成绝对路径。
func joinPath(dir, name string) string {
	if dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
}
