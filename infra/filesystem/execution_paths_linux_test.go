//go:build linux

package filesystem

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRawPathExecutionRequest(t *testing.T) {
	base := t.TempDir()
	f, err := Open(base)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, p := range []string{"\xff.pdf", "mid-\xfe.pdf", "�.pdf", "mid-�.pdf"} {
		if err := os.WriteFile(filepath.Join(base, p), []byte(p), 0600); err != nil {
			t.Fatal(err)
		}
		request := executionRequest{Op: "rename", Path: p, Target: "moved-" + p}
		b, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		var decoded executionRequest
		if err := json.Unmarshal(b, &decoded); err != nil {
			t.Fatal(err)
		}
		_, output, err := executeRequest(f, decoded, nil)
		if output != nil {
			output.Close()
		}
		if err != nil {
			t.Errorf("rename %q: %v", p, err)
			continue
		}
		data, err := os.ReadFile(filepath.Join(base, "moved-"+p))
		if err != nil || string(data) != p {
			t.Errorf("worker changed raw path %q: %q %v", p, data, err)
		}
	}
}
