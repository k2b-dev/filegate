package cli

import (
	"strings"
	"testing"
)

// TestCheckMountsHealthOrFailEmpty: zero base_paths is rejected
// fast — the daemon needs at least one mount to do anything.
func TestCheckMountsHealthOrFailEmpty(t *testing.T) {
	if _, err := checkMountsHealthOrFail(nil); err == nil {
		t.Errorf("empty paths slice should return an error")
	}
}

// TestCheckMountsHealthOrFailMissingMount: a configured mount
// that doesn't exist on disk hard-fails startup with a clear
// error message naming the path.
func TestCheckMountsHealthOrFailMissingMount(t *testing.T) {
	missing := "/nonexistent/filegate/test/mount"
	_, err := checkMountsHealthOrFail([]string{missing})
	if err == nil {
		t.Fatalf("missing mount should hard-fail")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error=%q, want path %q in message", err.Error(), missing)
	}
}

// TestCheckMountsHealthOrFailHealthy: a real tempdir passes —
// the function returns nil. Caller proceeds to open the index.
// (Skips when the test FS doesn't support xattrs; see the
// filesystem-package test for the same skip rationale.)
func TestCheckMountsHealthOrFailHealthy(t *testing.T) {
	tmp := t.TempDir()
	results, err := checkMountsHealthOrFail([]string{tmp})
	if err != nil {
		// xattr-not-supported on the test FS shows up as the
		// only error path here — accept it as a skip rather
		// than a fail.
		if strings.Contains(err.Error(), "xattr") {
			t.Skipf("test FS doesn't support xattrs: %v", err)
		}
		t.Errorf("healthy mount probe returned error: %v", err)
	}
	if len(results) != 1 || results[0].Path != tmp {
		t.Fatalf("health results = %+v, want one result for %q", results, tmp)
	}
}
