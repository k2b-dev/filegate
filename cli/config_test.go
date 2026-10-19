package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStaticConfigValidation(t *testing.T) {
	base := t.TempDir()
	data := filepath.Join(base, "data")
	os.Mkdir(data, 0700)
	token := filepath.Join(base, "token")
	os.WriteFile(token, []byte(strings.Repeat("t", 32)), 0600)
	source := fmt.Sprintf("server:\n  public_url: http://localhost:8080\nauth:\n  token_file: %s\nstate_dir: %s\nroots:\n  - name: cloud\n    path: %s\n    index: true\n    versioning:\n      enabled: true\n", token, filepath.Join(base, "state"), data)
	file := filepath.Join(base, "config.yaml")
	os.WriteFile(file, []byte(source), 0600)
	cfg, e := LoadConfig(file)
	if e != nil {
		t.Fatal(e)
	}
	if cfg.Roots[0].Versioning.Keep.Last != 10 || cfg.Roots[0].Versioning.Keep.Daily != 30 {
		t.Fatal("missing default retention")
	}
	if cfg.Roots[0].Versioning.Cooldown != "1m" {
		t.Fatal("wrong default")
	}
	for _, bad := range []string{strings.Replace(source, "index: true", "index: false", 1), source + "s3: {}\n", strings.Replace(source, filepath.Join(base, "state"), filepath.Join(data, "state"), 1), source + "---\nroots: []\n"} {
		os.WriteFile(file, []byte(bad), 0600)
		if _, e = LoadConfig(file); e == nil {
			t.Fatal("invalid config accepted", bad)
		}
	}
}

func TestSearchTopKConfig(t *testing.T) {
	base := t.TempDir()
	data := filepath.Join(base, "data")
	if err := os.Mkdir(data, 0700); err != nil {
		t.Fatal(err)
	}
	source := fmt.Sprintf("server:\n  public_url: http://localhost:8080\nauth:\n  token_file: %s\nstate_dir: %s\nroots:\n  - name: docs\n    path: %s\n", filepath.Join(base, "token"), filepath.Join(base, "state"), data)
	file := filepath.Join(base, "config.yaml")
	for _, tc := range []struct {
		yaml    string
		want    int
		invalid bool
	}{
		{"", 1000, false}, {"search:\n  topK: 7\n", 7, false}, {"search:\n  topK: 100000\n", 100000, false},
		{"search:\n  topK: 0\n", 0, true}, {"search:\n  topK: -1\n", 0, true}, {"search:\n  topK: 100001\n", 0, true}, {"search:\n  topK: many\n", 0, true},
	} {
		if err := os.WriteFile(file, []byte(source+tc.yaml), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(file)
		if (err != nil) != tc.invalid || !tc.invalid && cfg.Search.TopK != tc.want {
			t.Fatalf("%q: topK=%d err=%v", tc.yaml, cfg.Search.TopK, err)
		}
	}
}
