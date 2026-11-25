package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListenerConfig(t *testing.T) {
	p := runtimePaths{config: t.TempDir()}
	path := filepath.Join(p.config, "config.json")
	addr, err := p.mcpAddress()
	if err != nil || addr.String() != "127.0.0.1:9417" {
		t.Fatalf("first start: %v, %v", addr, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("configuration permissions: %v, %v", info, err)
	}
	for _, tc := range []struct{ contents, want string }{
		{`{"bind":"::","port":12345}`, "[::]:12345"},
		{`{"port":0}`, "127.0.0.1:0"},
		{`{"bind":"invalid"}`, ""},
		{`{"port":65536}`, ""},
		{`{"prot":12345}`, ""},
		{`null`, ""},
		{`{}` + `{}`, ""},
	} {
		if err := os.WriteFile(path, []byte(tc.contents), 0600); err != nil {
			t.Fatal(err)
		}
		addr, err := p.mcpAddress()
		if tc.want == "" {
			if err == nil || !strings.Contains(err.Error(), path) {
				t.Errorf("%s: expected error naming the file, got %v", tc.contents, err)
			}
		} else if err != nil || addr.String() != tc.want {
			t.Errorf("%s: got %v, %v", tc.contents, addr, err)
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != tc.contents {
			t.Fatalf("edited configuration changed: %q, %v", got, err)
		}
	}
}
