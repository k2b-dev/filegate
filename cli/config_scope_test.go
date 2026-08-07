package cli

import (
	"strings"
	"testing"

	"github.com/k2b-dev/filegate/v3/domain"
)

// Every key must carry a scope decision. Without this guard a newly added key
// silently defaults to static, which is the safe direction but hides the fact
// that nobody thought about it.
func TestEveryConfigKeyIsClassified(t *testing.T) {
	specs := allConfigFlagSpecs()
	if len(specs) == 0 {
		t.Fatal("no config specs")
	}

	seen := make(map[string]bool, len(specs))
	for _, spec := range specs {
		if seen[spec.Path] {
			t.Errorf("%s is declared twice", spec.Path)
		}
		seen[spec.Path] = true

		// A static key without a reason is an unreviewed default rather than
		// a decision, so require the justification alongside the scope.
		if spec.Scope == scopeStatic && strings.TrimSpace(spec.Reason) == "" {
			t.Errorf("%s is static but gives no reason why it cannot change at runtime", spec.Path)
		}
		if spec.Scope == scopeRuntime && strings.TrimSpace(spec.Reason) != "" {
			t.Errorf("%s is runtime but carries a static-only reason: %q", spec.Path, spec.Reason)
		}
	}
}

// The config endpoints expose every key, so secrecy is a deny list. This test
// is the thing that keeps a newly added credential from leaking by default.
func TestSecretLookingKeysAreMarkedSecret(t *testing.T) {
	markers := []string{"token", "secret", "password", "key"}
	// s3.keys holds secrets and is marked; these hold none despite matching.
	allowed := map[string]bool{
		"s3.access_key":            true,
		"cache.path_cache_size":    true,
		"s3.max_concurrent_writes": true,
	}

	for _, spec := range allConfigFlagSpecs() {
		if spec.Secret {
			continue
		}
		lower := strings.ToLower(spec.Path)
		for _, marker := range markers {
			if !strings.Contains(lower, marker) || allowed[spec.Path] {
				continue
			}
			t.Errorf("%s looks like a credential (%q) but is not marked Secret; mark it or add it to the reviewed exceptions", spec.Path, marker)
		}
	}
}

func TestRuntimeScopeMatchesLiveSnapshotConsumers(t *testing.T) {
	expectedRuntime := map[string]bool{
		"server.public_url":               true,
		"server.trusted_proxies":          true,
		"server.cors.allowed_origins":     true,
		"server.cors.allowed_methods":     true,
		"server.cors.allowed_headers":     true,
		"server.cors.exposed_headers":     true,
		"server.cors.max_age":             true,
		"server.cors.allow_credentials":   true,
		"server.access_log_enabled":       true,
		"upload.max_chunk_bytes":          true,
		"upload.max_upload_bytes":         true,
		"upload.max_session_upload_bytes": true,
		"upload.min_free_bytes":           true,
	}
	var static, runtime int
	for _, spec := range allConfigFlagSpecs() {
		if spec.Scope == scopeStatic {
			static++
			if expectedRuntime[spec.Path] {
				t.Errorf("%s is consumed from the live snapshot but marked static", spec.Path)
			}
			continue
		}
		runtime++
		if !expectedRuntime[spec.Path] {
			t.Errorf("%s is marked runtime without a live snapshot consumer", spec.Path)
		}
		delete(expectedRuntime, spec.Path)
	}
	t.Logf("config keys: %d static, %d runtime, %d total", static, runtime, static+runtime)
	for path := range expectedRuntime {
		t.Errorf("%s has a live snapshot consumer but no runtime schema entry", path)
	}
}

// The runtime store must never sit inside the index directory: index rebuilds
// remove that directory, which would take stored credentials with it.
func TestRuntimeConfigPathMustBeOutsideTheIndex(t *testing.T) {
	base := domain.StorageConfig{IndexPath: "/var/lib/filegate/index"}

	for _, nested := range []string{
		"/var/lib/filegate/index",
		"/var/lib/filegate/index/config",
		"/var/lib/filegate/index/nested/deeper",
	} {
		storage := base
		storage.RuntimeConfigPath = nested
		if err := validateRuntimeConfigPath(storage); err == nil {
			t.Errorf("%q accepted although an index rebuild would delete it", nested)
		}
	}

	for _, ok := range []string{
		"/var/lib/filegate/config",
		"/var/lib/filegate/index-config",
		"/etc/filegate/runtime",
	} {
		storage := base
		storage.RuntimeConfigPath = ok
		if err := validateRuntimeConfigPath(storage); err != nil {
			t.Errorf("%q rejected although it is outside the index: %v", ok, err)
		}
	}

	storage := base
	storage.RuntimeConfigPath = ""
	if err := validateRuntimeConfigPath(storage); err == nil {
		t.Error("empty runtime config path accepted")
	}
}

// Numeric keys must say what they count. Three of them are named "size" while
// holding an entry count, and rendering those as bytes would claim a 100,000
// entry cache occupies 97 KiB.
func TestNumericKeysDeclareTheirUnit(t *testing.T) {
	for _, spec := range allConfigFlagSpecs() {
		if spec.Kind != configFlagInt && spec.Kind != configFlagInt64 {
			continue
		}
		if strings.TrimSpace(spec.Unit) == "" {
			t.Errorf("%s is numeric but declares no unit; a client cannot tell bytes from a count", spec.Path)
		}
	}
}

// A key named "size" is not evidence of bytes.
func TestSizeNamedCountsAreNotMarkedAsBytes(t *testing.T) {
	counts := map[string]string{
		"cache.path_cache_size":     "entries",
		"thumbnail.lru_cache_size":  "entries",
		"activity.ring_buffer_size": "events",
		"jobs.queue_size":           "jobs",
	}
	for _, spec := range allConfigFlagSpecs() {
		want, tracked := counts[spec.Path]
		if !tracked {
			continue
		}
		if spec.Unit != want {
			t.Errorf("%s unit = %q, want %q; it holds a count, not bytes", spec.Path, spec.Unit, want)
		}
	}
}
