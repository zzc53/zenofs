package config

import "testing"

func TestLoadConfigDefaults(t *testing.T) {
	t.Setenv("ZENOFS_DSN", "")
	cfg := LoadConfig([]string{"zenofs"})
	if cfg.DatabaseUrl != Default().DatabaseUrl {
		t.Fatalf("默认 DSN = %q, want %q", cfg.DatabaseUrl, Default().DatabaseUrl)
	}
	if Default().DatabaseUrl != "sqlite://zenofs.db" {
		t.Fatalf("默认 DSN 变了: %q", Default().DatabaseUrl)
	}
}

func TestLoadConfigPrecedence(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  string
		want string
	}{
		{"命令行参数", []string{"zenofs", "postgres://localhost/z"}, "mysql://ignored", "postgres://localhost/z"},
		{"环境变量", []string{"zenofs"}, "mysql://localhost/z", "mysql://localhost/z"},
		{"参数为空串时回退环境变量", []string{"zenofs", ""}, "mysql://localhost/z", "mysql://localhost/z"},
		{"两者都没有用默认值", []string{"zenofs"}, "", "sqlite://zenofs.db"},
		{"多出来的参数被忽略", []string{"zenofs", "sqlite://a.db", "--verbose"}, "x", "sqlite://a.db"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ZENOFS_DSN", tt.env)
			if got := LoadConfig(tt.args).DatabaseUrl; got != tt.want {
				t.Fatalf("DatabaseUrl = %q, want %q", got, tt.want)
			}
		})
	}
}
