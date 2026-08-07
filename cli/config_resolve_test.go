package cli

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	apiv1 "github.com/k2b-dev/filegate/v3/api/v1"
	"github.com/k2b-dev/filegate/v3/infra/runtimecfg"
)

type testManager struct {
	manager    *ConfigManager
	store      *runtimecfg.Store
	configFile string
}

func newTestManager(t *testing.T, extraYAML string) testManager {
	t.Helper()

	dir := t.TempDir()
	base := filepath.Join(dir, "data")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	configFile := filepath.Join(dir, "conf.yaml")
	body := "auth:\n  bearer_token: file-token\nstorage:\n  base_paths:\n    - " + base + "\n" + extraYAML
	if err := os.WriteFile(configFile, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	store, err := runtimecfg.Open(filepath.Join(dir, "runtime"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	resolved, err := resolveConfig(configFile, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return testManager{
		manager:    newConfigManager(configFile, store, resolved, resolved, nil),
		store:      store,
		configFile: configFile,
	}
}

func TestRuntimeManifestChangeIsPublishedImmediately(t *testing.T) {
	fixture := newTestManager(t, "upload:\n  max_upload_bytes: 1000\n")
	m := fixture.manager

	applied, err := m.ApplyManifest(map[string]any{"upload.max_upload_bytes": 2000}, "", "alice")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(applied.RestartRequired) != 0 {
		t.Errorf("runtime key reported as restart-required: %+v", applied.RestartRequired)
	}
	if got := m.Holder().Get().Upload.MaxUploadBytes; got != 2000 {
		t.Errorf("effective value = %d, want 2000", got)
	}
	if applied.Manifest.AppliedBy != "alice" || applied.Manifest.Revision == "" {
		t.Errorf("manifest metadata = %+v", applied.Manifest)
	}
}

func TestStaticManifestChangeStaysDesiredUntilRestart(t *testing.T) {
	fixture := newTestManager(t, "")
	m := fixture.manager
	running := m.Holder().Get().Server.Listen

	applied, err := m.ApplyManifest(map[string]any{"server.listen": ":9999"}, "", "alice")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(applied.RestartRequired) != 1 || applied.RestartRequired[0].Path != "server.listen" {
		t.Fatalf("restarts = %+v, want server.listen", applied.RestartRequired)
	}
	if got := m.Holder().Get().Server.Listen; got != running {
		t.Errorf("effective static value = %q, want running %q", got, running)
	}
	values := m.Values()
	entry := valueByPath(t, values, "server.listen")
	if entry.Effective != running || entry.Desired != ":9999" {
		t.Errorf("value = %+v", entry)
	}
}

func TestAppliedStaticManifestBecomesEffectiveAtNextStartup(t *testing.T) {
	fixture := newTestManager(t, "")
	applied, err := fixture.manager.ApplyManifest(map[string]any{"server.listen": ":9999"}, "", "alice")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	stored, exists, err := fixture.store.Manifest()
	if err != nil || !exists {
		t.Fatalf("stored manifest: exists=%v err=%v", exists, err)
	}
	resolved, err := resolveConfig(fixture.configFile, stored.Values)
	if err != nil {
		t.Fatalf("resolve startup: %v", err)
	}
	baseline, err := resolveConfig(fixture.configFile, nil)
	if err != nil {
		t.Fatalf("resolve baseline: %v", err)
	}
	restarted := newConfigManager(fixture.configFile, fixture.store, baseline, resolved, &stored)
	if got := restarted.Holder().Get().Server.Listen; got != ":9999" {
		t.Errorf("after restart = %q, want :9999", got)
	}
	if restarted.Values().Manifest.Revision != applied.Manifest.Revision {
		t.Errorf("startup lost manifest metadata")
	}
	if len(restarted.Values().RestartRequired) != 0 {
		t.Errorf("startup still reports restart required: %+v", restarted.Values().RestartRequired)
	}
}

func TestManifestResolutionKeepsGeneratedBootstrapToken(t *testing.T) {
	fixture := newTestManager(t, "")
	fixture.manager.baseline.Config.Auth.BearerToken = "generated-token"
	fixture.manager.desired.Auth.BearerToken = "generated-token"
	effective := fixture.manager.Holder().Get()
	effective.Auth.BearerToken = "generated-token"
	fixture.manager.Holder().Set(effective)

	applied, err := fixture.manager.ApplyManifest(map[string]any{"server.access_log_enabled": true}, "", "alice")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(applied.RestartRequired) != 0 {
		t.Errorf("bootstrap token became a pending manifest change: %+v", applied.RestartRequired)
	}
	if got := fixture.manager.Values(); valueByPath(t, got, "auth.bearer_token").Desired.(map[string]any)["configured"] != true {
		t.Error("generated bootstrap token disappeared from desired config")
	}
}

func TestManifestReplacementRemovesManagedPathAndFallsBack(t *testing.T) {
	fixture := newTestManager(t, "upload:\n  max_upload_bytes: 1000\n")
	m := fixture.manager

	first, err := m.ApplyManifest(map[string]any{"upload.max_upload_bytes": 3000}, "", "alice")
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	second, err := m.ApplyManifest(map[string]any{"server.access_log_enabled": true}, first.Manifest.Revision, "alice")
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if got := m.Holder().Get().Upload.MaxUploadBytes; got != 1000 {
		t.Errorf("removed value = %d, want file fallback 1000", got)
	}
	if len(second.Changes) != 2 || second.Changes[0].Operation != "add" || second.Changes[1].Operation != "remove" {
		t.Errorf("replacement changes = %+v", second.Changes)
	}
	stored, _, _ := fixture.store.Manifest()
	if _, exists := stored.Values["upload.max_upload_bytes"]; exists {
		t.Error("removed path is still persisted")
	}
}

func TestInvalidManifestDoesNotPersistOrPublish(t *testing.T) {
	fixture := newTestManager(t, "")
	before := fixture.manager.Holder().Get()

	for name, values := range map[string]map[string]any{
		"unknown":   {"nonsense.key": 1},
		"invalid":   {"detection.backend": "telepathy"},
		"null":      {"upload.expiry": nil},
		"bootstrap": {"storage.runtime_config_path": "/tmp/elsewhere"},
		"secret":    {"auth.bearer_token": "do-not-store"},
		"resource":  {"s3.keys": []any{}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := fixture.manager.ApplyManifest(values, "", "alice"); err == nil {
				t.Fatal("invalid manifest was accepted")
			}
		})
	}
	if got := fixture.manager.Holder().Get(); got.Detection.Backend != before.Detection.Backend {
		t.Errorf("effective config changed despite rejection")
	}
	if _, exists, err := fixture.store.Manifest(); err != nil || exists {
		t.Errorf("invalid manifest persisted: exists=%v err=%v", exists, err)
	}
}

func TestApplyRejectsStalePlanRevision(t *testing.T) {
	fixture := newTestManager(t, "")
	first, err := fixture.manager.ApplyManifest(map[string]any{"server.access_log_enabled": true}, "", "alice")
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if _, err := fixture.manager.ApplyManifest(map[string]any{"server.access_log_enabled": false}, "", "bob"); err == nil {
		t.Fatal("stale empty revision was accepted")
	}
	if got := fixture.manager.Values().Manifest.Revision; got != first.Manifest.Revision {
		t.Errorf("revision changed after conflict: %q", got)
	}
}

func TestPlanIsDeterministicAndDoesNotApply(t *testing.T) {
	fixture := newTestManager(t, "")
	before := fixture.manager.Holder().Get().Upload.MaxUploadBytes
	values := map[string]any{"upload.max_upload_bytes": 4242, "server.access_log_enabled": true}

	first, err := fixture.manager.PlanManifest(values)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	second, err := fixture.manager.PlanManifest(map[string]any{"server.access_log_enabled": true, "upload.max_upload_bytes": 4242})
	if err != nil {
		t.Fatalf("second plan: %v", err)
	}
	if first.ProposedRevision != second.ProposedRevision {
		t.Errorf("revision depends on map order: %q != %q", first.ProposedRevision, second.ProposedRevision)
	}
	if got := fixture.manager.Holder().Get().Upload.MaxUploadBytes; got != before {
		t.Errorf("plan published value %d", got)
	}
	if _, exists, _ := fixture.store.Manifest(); exists {
		t.Error("plan persisted a manifest")
	}
}

func TestSourcesDistinguishDefaultFileEnvAndManifest(t *testing.T) {
	t.Setenv("FILEGATE_UPLOAD_EXPIRY", "2h")
	fixture := newTestManager(t, "upload:\n  max_upload_bytes: 1000\n")
	m := fixture.manager

	if got := m.Sources()["upload.max_upload_bytes"]; got != SourceFile {
		t.Errorf("file source = %q", got)
	}
	if got := m.Sources()["upload.expiry"]; got != SourceEnv {
		t.Errorf("env source = %q", got)
	}
	if got := m.Sources()["upload.min_free_bytes"]; got != SourceDefault {
		t.Errorf("default source = %q", got)
	}
	if _, err := m.ApplyManifest(map[string]any{"upload.min_free_bytes": 999}, "", "alice"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := m.Sources()["upload.min_free_bytes"]; got != SourceManifest {
		t.Errorf("manifest source = %q", got)
	}
}

func TestSecretsNeverRenderTheirValue(t *testing.T) {
	fixture := newTestManager(t, "")
	values := fixture.manager.Values()
	for _, spec := range allConfigFlagSpecs() {
		if !spec.Secret {
			continue
		}
		entry := valueByPath(t, values, spec.Path)
		for label, rendered := range map[string]any{"effective": entry.Effective, "desired": entry.Desired} {
			shaped, ok := rendered.(map[string]any)
			if !ok {
				t.Errorf("%s %s rendered as %T", spec.Path, label, rendered)
				continue
			}
			if _, present := shaped["configured"]; !present {
				t.Errorf("%s %s missing configured flag", spec.Path, label)
			}
		}
	}
}

func TestSchemaPublishesChoicesAndManagementBoundary(t *testing.T) {
	choicesByPath := map[string][]string{
		"detection.backend":  {"auto", "poll", "btrfs"},
		"versioning.enabled": {"auto", "on", "off"},
	}
	managedByPath := map[string]string{
		"upload.expiry":               apiv1.ConfigManagedByManifest,
		"storage.runtime_config_path": apiv1.ConfigManagedByBootstrap,
		"auth.bearer_token":           apiv1.ConfigManagedByBootstrap,
		"s3.keys":                     apiv1.ConfigManagedByResource,
	}

	for _, key := range configSchema() {
		if want, ok := choicesByPath[key.Path]; ok {
			if !slices.Equal(key.Choices, want) {
				t.Errorf("%s choices = %v, want %v", key.Path, key.Choices, want)
			}
			delete(choicesByPath, key.Path)
		}
		if want, ok := managedByPath[key.Path]; ok {
			if key.ManagedBy != want {
				t.Errorf("%s managedBy = %q, want %q", key.Path, key.ManagedBy, want)
			}
			delete(managedByPath, key.Path)
		}
	}
	if len(choicesByPath) != 0 || len(managedByPath) != 0 {
		t.Errorf("schema missing keys: choices=%v management=%v", choicesByPath, managedByPath)
	}
}

func valueByPath(t *testing.T, response apiv1.ConfigValuesResponse, path string) apiv1.ConfigValue {
	t.Helper()
	for _, value := range response.Values {
		if value.Path == path {
			return value
		}
	}
	t.Fatalf("value %q not found", path)
	return apiv1.ConfigValue{}
}
