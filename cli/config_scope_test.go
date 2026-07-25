package cli

import (
	"strings"
	"testing"
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

func TestScopeCountsAreDeliberate(t *testing.T) {
	var static, runtime int
	for _, spec := range allConfigFlagSpecs() {
		if spec.Scope == scopeStatic {
			static++
			continue
		}
		runtime++
	}
	t.Logf("config keys: %d static, %d runtime, %d total", static, runtime, static+runtime)

	// The whole point of the split is that most settings are adjustable. If
	// this ratio inverts, something was classified without thinking.
	if runtime < static {
		t.Errorf("more static (%d) than runtime (%d) keys; the split is meant to favour runtime", static, runtime)
	}
}
