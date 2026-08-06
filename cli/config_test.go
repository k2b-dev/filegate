package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/valentinkolb/filegate/domain"
	"github.com/valentinkolb/filegate/infra/runtimecfg"
)

func TestLoadConfigJobDefaults(t *testing.T) {
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
	if cfg.Jobs.Workers <= 0 {
		t.Fatalf("jobs.workers must be > 0")
	}
	if cfg.Jobs.QueueSize != 8192 {
		t.Fatalf("jobs.queue_size=%d, want 8192", cfg.Jobs.QueueSize)
	}
	if cfg.Upload.MaxSessionUploadBytes != int64(50*1024*1024*1024) {
		t.Fatalf("upload.max_session_upload_bytes=%d, want %d",
			cfg.Upload.MaxSessionUploadBytes, int64(50*1024*1024*1024))
	}
	if cfg.Upload.MaxConcurrentSegmentWrites <= 0 {
		t.Fatalf("upload.max_concurrent_segment_writes=%d, want > 0", cfg.Upload.MaxConcurrentSegmentWrites)
	}
	if cfg.Upload.MinFreeBytes != int64(64*1024*1024) {
		t.Fatalf("upload.min_free_bytes=%d, want %d", cfg.Upload.MinFreeBytes, int64(64*1024*1024))
	}
	if cfg.S3.MaxConcurrentWrites <= 0 {
		t.Fatalf("s3.max_concurrent_writes=%d, want > 0", cfg.S3.MaxConcurrentWrites)
	}
	if cfg.Activity.RingBufferSize != 500 {
		t.Fatalf("activity.ring_buffer_size=%d, want 500", cfg.Activity.RingBufferSize)
	}
	if cfg.Thumbnail.MaxSourceBytes != int64(64*1024*1024) {
		t.Fatalf("thumbnail.max_source_bytes=%d, want %d",
			cfg.Thumbnail.MaxSourceBytes, int64(64*1024*1024))
	}
	if cfg.Thumbnail.MaxPixels != int64(40*1024*1024) {
		t.Fatalf("thumbnail.max_pixels=%d, want %d",
			cfg.Thumbnail.MaxPixels, int64(40*1024*1024))
	}
}

func TestLoadConfigJobOverrides(t *testing.T) {
	base := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	content := "" +
		"auth:\n" +
		"  bearer_token: test-token\n" +
		"storage:\n" +
		"  base_paths:\n" +
		"    - " + base + "\n" +
		"jobs:\n" +
		"  workers: 40\n" +
		"  queue_size: 12000\n" +
		"  thumbnail_workers: 90\n" +
		"  thumbnail_queue_size: 32000\n" +
		"upload:\n" +
		"  max_concurrent_segment_writes: 77\n" +
		"  min_free_bytes: 123456\n" +
		"s3:\n" +
		"  max_concurrent_writes: 9\n" +
		"activity:\n" +
		"  ring_buffer_size: 42\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Jobs.Workers != 40 || cfg.Jobs.QueueSize != 12000 {
		t.Fatalf("global jobs mismatch: workers=%d queue=%d", cfg.Jobs.Workers, cfg.Jobs.QueueSize)
	}
	if cfg.Jobs.ThumbnailWorkers != 90 || cfg.Jobs.ThumbnailQueueSize != 32000 {
		t.Fatalf("thumbnail jobs mismatch: workers=%d queue=%d", cfg.Jobs.ThumbnailWorkers, cfg.Jobs.ThumbnailQueueSize)
	}
	if cfg.Upload.MaxConcurrentSegmentWrites != 77 {
		t.Fatalf("upload.max_concurrent_segment_writes=%d, want 77", cfg.Upload.MaxConcurrentSegmentWrites)
	}
	if cfg.Upload.MinFreeBytes != 123456 {
		t.Fatalf("upload.min_free_bytes=%d, want 123456", cfg.Upload.MinFreeBytes)
	}
	if cfg.S3.MaxConcurrentWrites != 9 {
		t.Fatalf("s3.max_concurrent_writes=%d, want 9", cfg.S3.MaxConcurrentWrites)
	}
	if cfg.Activity.RingBufferSize != 42 {
		t.Fatalf("activity.ring_buffer_size=%d, want 42", cfg.Activity.RingBufferSize)
	}
}

func TestLoadConfigActivityEnvOverride(t *testing.T) {
	t.Setenv("FILEGATE_STORAGE_BASE_PATHS", t.TempDir())
	t.Setenv("FILEGATE_AUTH_BEARER_TOKEN", "test-token")
	t.Setenv("FILEGATE_ACTIVITY_RING_BUFFER_SIZE", "123")

	cfg, err := loadConfig("")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Activity.RingBufferSize != 123 {
		t.Fatalf("activity.ring_buffer_size=%d, want 123", cfg.Activity.RingBufferSize)
	}
}

func TestLoadConfigRejectsNegativeActivityRingBufferSize(t *testing.T) {
	base := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	content := "" +
		"auth:\n" +
		"  bearer_token: test-token\n" +
		"storage:\n" +
		"  base_paths:\n" +
		"    - " + base + "\n" +
		"activity:\n" +
		"  ring_buffer_size: -1\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := loadConfig(cfgPath)
	if err == nil {
		t.Fatalf("expected config validation error")
	}
	if want := "activity.ring_buffer_size must be >= 0"; err.Error() != want {
		t.Fatalf("error=%q, want %q", err.Error(), want)
	}
}

func TestLoadConfigRejectsSessionUploadLimitBelowSegmentSize(t *testing.T) {
	base := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	content := "" +
		"auth:\n" +
		"  bearer_token: test-token\n" +
		"storage:\n" +
		"  base_paths:\n" +
		"    - " + base + "\n" +
		"upload:\n" +
		"  max_chunk_bytes: 10485760\n" +
		"  max_session_upload_bytes: 1048576\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := loadConfig(cfgPath)
	if err == nil {
		t.Fatalf("expected config validation error")
	}
	if want := "upload.max_session_upload_bytes must be >= upload.max_chunk_bytes"; err.Error() != want {
		t.Fatalf("error=%q, want %q", err.Error(), want)
	}
}

func TestLoadConfigCORSDefaultsDisabled(t *testing.T) {
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
	if len(cfg.Server.CORS.AllowedOrigins) != 0 {
		t.Fatalf("allowed origins=%v, want empty", cfg.Server.CORS.AllowedOrigins)
	}
	if cfg.Server.CORS.AllowCredentials {
		t.Fatalf("allow credentials default should be false")
	}
}

func TestLoadConfigRejectsWildcardCORSCredentials(t *testing.T) {
	base := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	content := "" +
		"auth:\n" +
		"  bearer_token: test-token\n" +
		"storage:\n" +
		"  base_paths:\n" +
		"    - " + base + "\n" +
		"server:\n" +
		"  cors:\n" +
		"    allowed_origins: [\"*\"]\n" +
		"    allow_credentials: true\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := loadConfig(cfgPath)
	if err == nil {
		t.Fatalf("expected config validation error")
	}
	if want := "server.cors.allow_credentials cannot be true when allowed_origins contains *"; err.Error() != want {
		t.Fatalf("error=%q, want %q", err.Error(), want)
	}
}

// TestLoadConfigS3CleanupEnvOverrides pins that the
// FILEGATE_S3_CLEANUP_* env vars actually reach the resolved
// config. Without explicit SetDefault calls in loadConfig, viper's
// AutomaticEnv silently misses them — operators who set
// FILEGATE_S3_CLEANUP_INTERVAL=-1s to disable the loop would
// then quietly fall back to the 1h default. Codex flagged this
// as a P2 on the M5-1 commit.
func TestLoadConfigS3CleanupEnvOverrides(t *testing.T) {
	t.Setenv("FILEGATE_STORAGE_BASE_PATHS", t.TempDir())
	t.Setenv("FILEGATE_AUTH_BEARER_TOKEN", "test-token")
	t.Setenv("FILEGATE_S3_CLEANUP_DONE_RETENTION", "5m")
	t.Setenv("FILEGATE_S3_CLEANUP_ABORTED_RETENTION", "10m")
	t.Setenv("FILEGATE_S3_CLEANUP_STUCK_UPLOAD_MAX_AGE", "30m")
	t.Setenv("FILEGATE_S3_CLEANUP_INTERVAL", "-1s") // disable

	cfg, err := loadConfig("")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.S3.Cleanup.DoneRetention != 5*time.Minute {
		t.Errorf("DoneRetention=%s, want 5m", cfg.S3.Cleanup.DoneRetention)
	}
	if cfg.S3.Cleanup.AbortedRetention != 10*time.Minute {
		t.Errorf("AbortedRetention=%s, want 10m", cfg.S3.Cleanup.AbortedRetention)
	}
	if cfg.S3.Cleanup.StuckUploadMaxAge != 30*time.Minute {
		t.Errorf("StuckUploadMaxAge=%s, want 30m", cfg.S3.Cleanup.StuckUploadMaxAge)
	}
	if cfg.S3.Cleanup.Interval != -1*time.Second {
		t.Errorf("Interval=%s, want -1s (disable signal)", cfg.S3.Cleanup.Interval)
	}
}

// TestLoadConfigMetricsEnvOverrides pins that FILEGATE_METRICS_* env
// vars reach the resolved config (same SetDefault-needed-for-env
// precedent as the s3.cleanup.* knobs).
func TestLoadConfigMetricsEnvOverrides(t *testing.T) {
	t.Setenv("FILEGATE_STORAGE_BASE_PATHS", t.TempDir())
	t.Setenv("FILEGATE_AUTH_BEARER_TOKEN", "test-token")
	t.Setenv("FILEGATE_METRICS_ENABLED", "true")
	t.Setenv("FILEGATE_METRICS_PATH", "/internal/metrics")
	t.Setenv("FILEGATE_METRICS_TOKEN", "scrape-secret")

	cfg, err := loadConfig("")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if !cfg.Metrics.Enabled {
		t.Errorf("Metrics.Enabled=false, want true")
	}
	if cfg.Metrics.Path != "/internal/metrics" {
		t.Errorf("Metrics.Path=%q, want /internal/metrics", cfg.Metrics.Path)
	}
	if cfg.Metrics.Token != "scrape-secret" {
		t.Errorf("Metrics.Token=%q, want scrape-secret", cfg.Metrics.Token)
	}
}

// TestLoadConfigMetricsDefaults pins the off-by-default contract.
func TestLoadConfigMetricsDefaults(t *testing.T) {
	t.Setenv("FILEGATE_STORAGE_BASE_PATHS", t.TempDir())
	t.Setenv("FILEGATE_AUTH_BEARER_TOKEN", "test-token")
	cfg, err := loadConfig("")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Metrics.Enabled {
		t.Errorf("Metrics.Enabled defaults to true, want false")
	}
	if cfg.Metrics.Path != "/metrics" {
		t.Errorf("Metrics.Path default=%q, want /metrics", cfg.Metrics.Path)
	}
}

// TestLoadConfigBearerIsOptional pins the current contract: an empty
// auth.bearer_token loads fine, with or without S3.
//
// This replaces an earlier rule that required a token unless s3.enabled was
// set. Serve now generates and stores one on first boot, so a fresh install
// needs no configuration at all. Rejecting an empty value here would make that
// impossible. The REST auth middleware still fails closed while no token
// exists, so loading is not the same as opening the API.
func TestLoadConfigBearerIsOptional(t *testing.T) {
	for name, s3Enabled := range map[string]string{"s3 enabled": "true", "s3 disabled": "false"} {
		t.Run("empty bearer + "+name, func(t *testing.T) {
			t.Setenv("FILEGATE_STORAGE_BASE_PATHS", t.TempDir())
			t.Setenv("FILEGATE_AUTH_BEARER_TOKEN", "")
			t.Setenv("FILEGATE_S3_ENABLED", s3Enabled)
			if _, err := loadConfig(""); err != nil {
				t.Errorf("empty bearer should load, got %v", err)
			}
		})
	}
}

func TestLoadConfigRejectsPackagedBearerTokenSentinel(t *testing.T) {
	t.Setenv("FILEGATE_STORAGE_BASE_PATHS", t.TempDir())
	t.Setenv("FILEGATE_AUTH_BEARER_TOKEN", " CHANGE_ME ")

	_, err := loadConfig("")
	if err == nil {
		t.Fatal("CHANGE_ME bearer token loaded successfully")
	}
	if !strings.Contains(err.Error(), "leave it empty to generate one") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// A generated token must be stable: printing or regenerating it on every start
// would make it useless as a credential.
func TestBootstrapBearerTokenIsGeneratedOnceAndReused(t *testing.T) {
	store, err := runtimecfg.Open(filepath.Join(t.TempDir(), "runtime"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()

	first, err := bootstrapBearerToken(store, "")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if len(first) != 40 {
		t.Errorf("token length = %d, want 40", len(first))
	}

	second, err := bootstrapBearerToken(store, "")
	if err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	if second != first {
		t.Errorf("token regenerated on the second call: %q then %q", first, second)
	}

	// A configured token always wins; the stored one is never substituted.
	configured, err := bootstrapBearerToken(store, "operator-chosen")
	if err != nil {
		t.Fatalf("configured: %v", err)
	}
	if configured != "operator-chosen" {
		t.Errorf("configured token = %q, want the operator value", configured)
	}
}

func TestLoadConfigExplicitMissingFileReturnsError(t *testing.T) {
	_, err := loadConfig(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Fatalf("expected error for missing explicit config file")
	}
}

func TestLoadConfigServerTimeoutDefaultsAndOverrides(t *testing.T) {
	base := t.TempDir()

	defaultCfgPath := filepath.Join(t.TempDir(), "config-default.yaml")
	defaultContent := "auth:\n  bearer_token: test-token\nstorage:\n  base_paths:\n    - " + base + "\n"
	if err := os.WriteFile(defaultCfgPath, []byte(defaultContent), 0o644); err != nil {
		t.Fatalf("write default config: %v", err)
	}
	defaultCfg, err := loadConfig(defaultCfgPath)
	if err != nil {
		t.Fatalf("load default config: %v", err)
	}
	if defaultCfg.Server.WriteTimeout != 5*time.Minute {
		t.Fatalf("default write_timeout=%s, want 5m", defaultCfg.Server.WriteTimeout)
	}
	if defaultCfg.Server.ShutdownTimeout != 60*time.Second {
		t.Fatalf("default shutdown_timeout=%s, want 60s", defaultCfg.Server.ShutdownTimeout)
	}

	overrideCfgPath := filepath.Join(t.TempDir(), "config-override.yaml")
	overrideContent := "" +
		"server:\n" +
		"  write_timeout: 90s\n" +
		"  shutdown_timeout: 45s\n" +
		"auth:\n" +
		"  bearer_token: test-token\n" +
		"storage:\n" +
		"  base_paths:\n" +
		"    - " + base + "\n"
	if err := os.WriteFile(overrideCfgPath, []byte(overrideContent), 0o644); err != nil {
		t.Fatalf("write override config: %v", err)
	}
	overrideCfg, err := loadConfig(overrideCfgPath)
	if err != nil {
		t.Fatalf("load override config: %v", err)
	}
	if overrideCfg.Server.WriteTimeout != 90*time.Second {
		t.Fatalf("override write_timeout=%s, want 90s", overrideCfg.Server.WriteTimeout)
	}
	if overrideCfg.Server.ShutdownTimeout != 45*time.Second {
		t.Fatalf("override shutdown_timeout=%s, want 45s", overrideCfg.Server.ShutdownTimeout)
	}

	t.Setenv("FILEGATE_STORAGE_BASE_PATHS", base)
	t.Setenv("FILEGATE_AUTH_BEARER_TOKEN", "test-token")
	t.Setenv("FILEGATE_SERVER_WRITE_TIMEOUT", "75s")
	t.Setenv("FILEGATE_SERVER_SHUTDOWN_TIMEOUT", "35s")
	envCfg, err := loadConfig("")
	if err != nil {
		t.Fatalf("load env config: %v", err)
	}
	if envCfg.Server.WriteTimeout != 75*time.Second {
		t.Fatalf("env write_timeout=%s, want 75s", envCfg.Server.WriteTimeout)
	}
	if envCfg.Server.ShutdownTimeout != 35*time.Second {
		t.Fatalf("env shutdown_timeout=%s, want 35s", envCfg.Server.ShutdownTimeout)
	}
}

func TestLoadConfigPackagedExamplesParse(t *testing.T) {
	cases := []struct {
		name string
		path string
		want func(t *testing.T, cfg domain.Config)
	}{
		{
			name: "package config",
			path: filepath.Join("..", "packaging", "config", "conf.yaml"),
			want: func(t *testing.T, cfg domain.Config) {
				if cfg.Server.ShutdownTimeout != 60*time.Second {
					t.Fatalf("shutdown_timeout=%s, want 60s", cfg.Server.ShutdownTimeout)
				}
				if cfg.Upload.MaxSessionUploadBytes < cfg.Upload.MaxChunkBytes {
					t.Fatalf("max_session_upload_bytes=%d below max_chunk_bytes=%d",
						cfg.Upload.MaxSessionUploadBytes, cfg.Upload.MaxChunkBytes)
				}
			},
		},
		{
			name: "s3 example config",
			path: filepath.Join("..", "filegate.s3.example.yaml"),
			want: func(t *testing.T, cfg domain.Config) {
				if !cfg.S3.Enabled {
					t.Fatalf("s3.enabled=false, want true")
				}
				if cfg.S3.Listen != ":9100" {
					t.Fatalf("s3.listen=%q, want :9100", cfg.S3.Listen)
				}
				if cfg.Server.ShutdownTimeout != 60*time.Second {
					t.Fatalf("shutdown_timeout=%s, want 60s", cfg.Server.ShutdownTimeout)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadConfig(tc.path)
			if err != nil {
				t.Fatalf("load %s: %v", tc.path, err)
			}
			tc.want(t, cfg)
		})
	}
}

func TestLoadConfigUsesFilegateConfigEnv(t *testing.T) {
	base := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "custom-conf.yaml")
	content := "" +
		"server:\n" +
		"  listen: \":9090\"\n" +
		"auth:\n" +
		"  bearer_token: test-token\n" +
		"storage:\n" +
		"  base_paths:\n" +
		"    - " + base + "\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	t.Setenv("FILEGATE_CONFIG", cfgPath)
	cfg, err := loadConfig("")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Server.Listen != ":9090" {
		t.Fatalf("listen=%q, want :9090", cfg.Server.Listen)
	}
}

func TestLoadConfigFindsConfYAMLInWorkingDir(t *testing.T) {
	base := t.TempDir()
	workDir := t.TempDir()
	confPath := filepath.Join(workDir, "conf.yaml")
	content := "" +
		"server:\n" +
		"  listen: \":9191\"\n" +
		"auth:\n" +
		"  bearer_token: test-token\n" +
		"storage:\n" +
		"  base_paths:\n" +
		"    - " + base + "\n"
	if err := os.WriteFile(confPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() { _ = os.Chdir(oldWD) }()

	cfg, err := loadConfig("")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Server.Listen != ":9191" {
		t.Fatalf("listen=%q, want :9191", cfg.Server.Listen)
	}
}
