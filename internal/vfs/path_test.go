package vfs

import (
	"strings"
	"testing"
)

func TestCleanPath(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		want  string
		valid bool
	}{
		{"根", "/", "/", true},
		{"普通路径", "/a/b", "/a/b", true},
		{"折叠重复斜杠并去掉末尾", "//a//b//", "/a/b", true},
		{"去掉当前目录", "/a/./b", "/a/b", true},
		{"就解析上级目录", "/a/../b", "/b", true},
		{"不越过根 1", "/..", "/", true},
		{"不越过根 2", "/../..", "/", true},
		{"末段是上级目录", "/a/b/..", "/a", true},
		{"空路径非法", "", "", false},
		{"相对路径非法", "relative", "", false},
		{"缺少前导斜杠", "a/b", "", false},
	}
	for _, tt := range tests {
		got, err := cleanPath(tt.in)
		if tt.valid && err != nil {
			t.Errorf("%s: cleanPath(%q) 报错 %v", tt.name, tt.in, err)
			continue
		}
		if !tt.valid {
			if err == nil {
				t.Errorf("%s: cleanPath(%q) = %q, 期望报错", tt.name, tt.in, got)
			}
			continue
		}
		if got != tt.want {
			t.Errorf("%s: cleanPath(%q) = %q, want %q", tt.name, tt.in, got, tt.want)
		}
	}
}

func TestPathSegments(t *testing.T) {
	tests := []struct {
		in   string
		want int
		last string
	}{
		{"/", 0, ""},
		{"/a", 1, "a"},
		{"/a/b/c", 3, "c"},
		{"/a b/中文名", 2, "中文名"},
	}
	for _, tt := range tests {
		got := pathSegments(tt.in)
		if len(got) != tt.want {
			t.Errorf("pathSegments(%q) = %v, want %d 段", tt.in, got, tt.want)
			continue
		}
		if tt.want > 0 && got[len(got)-1] != tt.last {
			t.Errorf("pathSegments(%q) 末段 = %q, want %q", tt.in, got[len(got)-1], tt.last)
		}
	}
}

func TestParentPathAndBaseName(t *testing.T) {
	tests := []struct {
		in         string
		wantParent string
		wantBase   string
	}{
		{"/", "/", "/"},
		{"/a", "/", "a"},
		{"/a/b", "/a", "b"},
		{"/a/b/c", "/a/b", "c"},
	}
	for _, tt := range tests {
		if got := parentPath(tt.in); got != tt.wantParent {
			t.Errorf("parentPath(%q) = %q, want %q", tt.in, got, tt.wantParent)
		}
		if got := baseName(tt.in); got != tt.wantBase {
			t.Errorf("baseName(%q) = %q, want %q", tt.in, got, tt.wantBase)
		}
	}
}

func TestJoinPath(t *testing.T) {
	tests := []struct {
		dir, name, want string
	}{
		{"/", "a", "/a"},
		{"/a", "b", "/a/b"},
	}
	for _, tt := range tests {
		if got := joinPath(tt.dir, tt.name); got != tt.want {
			t.Errorf("joinPath(%q, %q) = %q, want %q", tt.dir, tt.name, got, tt.want)
		}
	}
}

func TestSplitMount(t *testing.T) {
	tests := []struct {
		in       string
		wantName string
		wantRest string
	}{
		{"/", "", "/"},
		{"/work", "work", "/"},
		{"/work/a", "work", "/a"},
		{"/work/a/b", "work", "/a/b"},
	}
	for _, tt := range tests {
		name, rest := splitMount(tt.in)
		if name != tt.wantName || rest != tt.wantRest {
			t.Errorf("splitMount(%q) = (%q, %q), want (%q, %q)", tt.in, name, rest, tt.wantName, tt.wantRest)
		}
	}
}

func TestCheckName(t *testing.T) {
	tests := []struct {
		name string
		ok   bool
	}{
		{"a.txt", true},
		{"中文名", true},
		{strings.Repeat("x", maxNameLen), true},
		{"", false},
		{".", false},
		{"..", false},
		{"a/b", false},
		{"a\x00b", false},
		{strings.Repeat("x", maxNameLen+1), false},
	}
	for _, tt := range tests {
		err := checkName(tt.name)
		if (err == nil) != tt.ok {
			t.Errorf("checkName(%q) = %v, want ok=%v", tt.name, err, tt.ok)
		}
	}
}
