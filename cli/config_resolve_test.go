package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/valentinkolb/filegate/infra/runtimecfg"
)

func newTestManager(t *testing.T, yaml string) *ConfigManager {
	t.Helper()

	dir := t.TempDir()
	base := filepath.Join(dir, "data")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	configFile := filepath.Join(dir, "conf.yaml")
	body := "auth:\n  bearer_token: file-token\nstorage:\n  base_paths:\n    - " + base + "\n" + yaml
	if err := os.WriteFile(configFile, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	store, err := runtimecfg.Open(filepath.Join(dir, "runtime"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	resolved, err := resolveConfig(configFile, store.Overrides())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return newConfigManager(configFile, store, resolved)
}

func TestRuntimeChangeIsVisibleInTheSnapshot(t *testing.T) {
	m := newTestManager(t, "upload:\n  max_upload_bytes: 1000\n")

	if got := m.Holder().Get().Upload.MaxUploadBytes; got != 1000 {
		t.Fatalf("boot value = %d, want 1000 from the file", got)
	}

	restarts, err := m.Apply(map[string]any{"upload.max_upload_bytes": 2000})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(restarts) != 0 {
		t.Errorf("runtime key reported as restart-required: %+v", restarts)
	}
	if got := m.Holder().Get().Upload.MaxUploadBytes; got != 2000 {
		t.Errorf("live value = %d, want 2000 without a restart", got)
	}
}

// The failure a generic config API falls into: accept a static key, answer
// success, change nothing, say nothing.
func TestStaticChangeIsReportedAsRestartRequired(t *testing.T) {
	m := newTestManager(t, "")

	restarts, err := m.Apply(map[string]any{"server.listen": ":9999"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(restarts) != 1 || restarts[0].Path != "server.listen" {
		t.Fatalf("restarts = %+v, want exactly server.listen", restarts)
	}
	if restarts[0].Running == restarts[0].Desired {
		t.Errorf("restart entry does not show the difference: %+v", restarts[0])
	}
	// The process keeps serving on the old address until it restarts.
	if got := m.Holder().Get().Server.Listen; got != ":9999" {
		t.Logf("snapshot carries the desired value %q; the listener still uses the booted one", got)
	}
}

func TestInvalidChangeIsRejectedAndLeavesTheSnapshotIntact(t *testing.T) {
	m := newTestManager(t, "")
	before := m.Holder().Get().Detection.Backend

	if _, err := m.Apply(map[string]any{"detection.backend": "telepathy"}); err == nil {
		t.Fatal("invalid backend was accepted")
	}
	if got := m.Holder().Get().Detection.Backend; got != before {
		t.Errorf("snapshot changed to %q despite the rejection", got)
	}

	if _, err := m.Apply(map[string]any{"nonsense.key": 1}); err == nil {
		t.Error("unknown key was accepted")
	}
}

func TestOverridesOutrankTheFileAndSurviveReload(t *testing.T) {
	m := newTestManager(t, "upload:\n  max_upload_bytes: 1000\n")

	if _, err := m.Apply(map[string]any{"upload.max_upload_bytes": 3000}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := m.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := m.Holder().Get().Upload.MaxUploadBytes; got != 3000 {
		t.Errorf("after reload = %d, want the override to still outrank the file", got)
	}

	// Clearing falls back to the file, not to the built-in default.
	if _, err := m.Apply(map[string]any{"upload.max_upload_bytes": nil}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := m.Holder().Get().Upload.MaxUploadBytes; got != 1000 {
		t.Errorf("after clearing = %d, want the file value 1000", got)
	}
}

func TestSourcesDistinguishDefaultFileEnvAndRuntime(t *testing.T) {
	t.Setenv("FILEGATE_UPLOAD_EXPIRY", "2h")
	m := newTestManager(t, "upload:\n  max_upload_bytes: 1000\n")

	sources := m.Sources()
	if got := sources["upload.max_upload_bytes"]; got != SourceFile {
		t.Errorf("file-set key reported as %q, want file", got)
	}
	if got := sources["upload.expiry"]; got != SourceEnv {
		t.Errorf("env-set key reported as %q, want env", got)
	}
	if got := sources["upload.min_free_bytes"]; got != SourceDefault {
		t.Errorf("untouched key reported as %q, want default", got)
	}

	if _, err := m.Apply(map[string]any{"upload.min_free_bytes": 999}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := m.Sources()["upload.min_free_bytes"]; got != SourceRuntime {
		t.Errorf("after an override the source is %q, want runtime", got)
	}
}

func TestValidateDoesNotApply(t *testing.T) {
	m := newTestManager(t, "")
	before := m.Holder().Get().Upload.MaxUploadBytes

	if err := m.Validate(map[string]any{"upload.max_upload_bytes": 4242}); err != nil {
		t.Fatalf("validate rejected a valid change: %v", err)
	}
	if got := m.Holder().Get().Upload.MaxUploadBytes; got != before {
		t.Errorf("validate applied the change: %d", got)
	}
	if err := m.Validate(map[string]any{"detection.backend": "telepathy"}); err == nil {
		t.Error("validate accepted an invalid backend")
	}
}

func TestSecretsNeverRenderTheirValue(t *testing.T) {
	m := newTestManager(t, "")
	cfg := m.Holder().Get()

	for _, spec := range allConfigFlagSpecs() {
		if !spec.Secret {
			continue
		}
		rendered := configValueForAPI(&cfg, spec)
		shaped, ok := rendered.(map[string]any)
		if !ok {
			t.Errorf("%s rendered as %T, want a presence flag", spec.Path, rendered)
			continue
		}
		if _, present := shaped["configured"]; !present {
			t.Errorf("%s rendered without a configured flag: %v", spec.Path, shaped)
		}
	}

	// The token really is set in the fixture, so this proves the flag reflects
	// reality rather than always reporting false.
	tokenSpec, _ := specByPath("auth.bearer_token")
	if got := configValueForAPI(&cfg, tokenSpec).(map[string]any)["configured"]; got != true {
		t.Errorf("bearer token reported as not configured despite being set")
	}
}
