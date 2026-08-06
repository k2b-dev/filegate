package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLoadConfigVersioningRetentionDefaults pins the default versioning
// schedule. A regression that empties the default RetentionBuckets list
// would silently turn auto-on reflink deployments into unbounded storage
// growth — every write captures forever, the pruner has nothing to
// prune. The other defaults are pinned alongside so a "tidy the config
// loader" pass can't drift the operator-facing contract.
func TestLoadConfigVersioningRetentionDefaults(t *testing.T) {
	base := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	content := "auth:\n  bearer_token: test-token\nstorage:\n  base_paths:\n    - " + base + "\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.Versioning.Enabled != "auto" {
		t.Fatalf("versioning.enabled=%q, want auto", cfg.Versioning.Enabled)
	}
	if cfg.Detection.ReconcileInterval != 24*time.Hour {
		t.Fatalf("detection.reconcile_interval=%s, want 24h", cfg.Detection.ReconcileInterval)
	}
	if cfg.Versioning.Cooldown != 15*time.Minute {
		t.Fatalf("versioning.cooldown=%s, want 15m", cfg.Versioning.Cooldown)
	}
	if cfg.Versioning.PrunerInterval != 5*time.Minute {
		t.Fatalf("versioning.pruner_interval=%s, want 5m", cfg.Versioning.PrunerInterval)
	}
	if cfg.Versioning.PinnedGraceAfterDelete != 30*24*time.Hour {
		t.Fatalf("versioning.pinned_grace_after_delete=%s, want 720h", cfg.Versioning.PinnedGraceAfterDelete)
	}
	if cfg.Versioning.MaxPinnedPerFile != 100 {
		t.Fatalf("versioning.max_pinned_per_file=%d, want 100", cfg.Versioning.MaxPinnedPerFile)
	}
	if cfg.Versioning.MaxLabelBytes != 2048 {
		t.Fatalf("versioning.max_label_bytes=%d, want 2048", cfg.Versioning.MaxLabelBytes)
	}
	if cfg.Versioning.MinSizeForAutoV1 != 64*1024 {
		t.Fatalf("versioning.min_size_for_auto_v1=%d, want %d",
			cfg.Versioning.MinSizeForAutoV1, 64*1024)
	}

	// Bucketed exponential decay default. The storage-leak prevention
	// rests on this being non-empty.
	want := []struct {
		KeepFor  time.Duration
		MaxCount int
	}{
		{time.Hour, -1},
		{24 * time.Hour, 24},
		{30 * 24 * time.Hour, 30},
		{365 * 24 * time.Hour, 12},
	}
	if len(cfg.Versioning.RetentionBuckets) != len(want) {
		t.Fatalf("retention_buckets len=%d, want %d", len(cfg.Versioning.RetentionBuckets), len(want))
	}
	for i, b := range cfg.Versioning.RetentionBuckets {
		if b.KeepFor != want[i].KeepFor || b.MaxCount != want[i].MaxCount {
			t.Fatalf("bucket %d = {%s, %d}, want {%s, %d}",
				i, b.KeepFor, b.MaxCount, want[i].KeepFor, want[i].MaxCount)
		}
	}
}

func TestLoadConfigAllowsDisabledReconciliation(t *testing.T) {
	base := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	content := "storage:\n  base_paths:\n    - " + base + "\ndetection:\n  reconcile_interval: 0s\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Detection.ReconcileInterval != 0 {
		t.Fatalf("detection.reconcile_interval=%s, want disabled", cfg.Detection.ReconcileInterval)
	}
}

func TestLoadConfigRejectsNegativeReconciliation(t *testing.T) {
	base := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	content := "storage:\n  base_paths:\n    - " + base + "\ndetection:\n  reconcile_interval: -1s\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := loadConfig(cfgPath); err == nil {
		t.Fatal("expected load error for negative reconciliation interval")
	}
}

// TestLoadConfigRejectsNegativeMaxPinnedPerFile pins the operator-safety
// invariant that a typo (`max_pinned_per_file: -1`) is loud rather than
// silently disabling the cap. The previous behaviour normalized any
// negative to 0 (= no cap) — a footgun a careful operator's config
// could trigger.
func TestLoadConfigRejectsNegativeMaxPinnedPerFile(t *testing.T) {
	base := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	content := "auth:\n  bearer_token: test-token\nstorage:\n  base_paths:\n    - " + base +
		"\nversioning:\n  max_pinned_per_file: -3\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := loadConfig(cfgPath); err == nil {
		t.Fatalf("expected load error for negative max_pinned_per_file")
	}
}

// TestLoadConfigRejectsInvalidVersioningEnabled pins that
// versioning.enabled accepts only auto/on/off, surfacing typos at
// startup instead of silently misbehaving.
func TestLoadConfigRejectsInvalidVersioningEnabled(t *testing.T) {
	base := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	content := "auth:\n  bearer_token: test-token\nstorage:\n  base_paths:\n    - " + base +
		"\nversioning:\n  enabled: maybe\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := loadConfig(cfgPath); err == nil {
		t.Fatalf("expected load error for invalid enabled value")
	}
}
