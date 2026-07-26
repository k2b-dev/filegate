package cli

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/valentinkolb/filegate/domain"
)

type configFlagKind int

const (
	configFlagString configFlagKind = iota
	configFlagBool
	configFlagInt
	configFlagInt64
	configFlagDuration
	configFlagStringArray
	configFlagS3Keys
	configFlagRetentionBuckets
)

// configScope says whether a value can change while the server runs.
//
// Getting this wrong is worse than refusing a change: a key wrongly marked
// runtime accepts an edit, answers 200, and quietly keeps using the old value.
// The scope therefore lives on the spec table rather than in a second list that
// could drift, and a test asserts every key carries one.
type configScope int

const (
	// scopeStatic values are consumed once during startup -- listener binds,
	// store paths, conditionally mounted routes -- and only a restart applies
	// a new value.
	scopeStatic configScope = iota
	// scopeRuntime values are read from the live snapshot, so a change takes
	// effect on the next request or the next loop iteration.
	scopeRuntime
)

func (s configScope) String() string {
	if s == scopeRuntime {
		return "runtime"
	}
	return "static"
}

type configFlagSpec struct {
	Name  string
	Path  string
	Kind  configFlagKind
	Usage string
	// Scope decides whether a change needs a restart. See configScope.
	Scope configScope
	// Secret marks values that must never be returned by an API. This is a
	// deny list because the config endpoints are meant to be complete; a test
	// fails when a secret-looking key is missing from it.
	Secret bool
	// Reason documents why a static key cannot move. Empty for runtime keys.
	Reason string
	// Unit says what a number counts, so a client can render 65536 as 64 KiB
	// and 100000 as "100,000 entries". The type alone cannot carry this: a
	// byte limit and a max-count are both ints, and several keys are named
	// "size" while holding a count.
	Unit string
}

func allConfigFlagSpecs() []configFlagSpec {
	return []configFlagSpec{
		{Name: "server-listen", Path: "server.listen", Kind: configFlagString, Usage: "REST listener address", Scope: scopeStatic, Reason: "REST listener bind address, fixed once the server accepts connections"},
		{Name: "server-public-url", Path: "server.public_url", Kind: configFlagString, Usage: "public REST base URL used when minting direct upload URLs", Scope: scopeRuntime},
		{Name: "server-trusted-proxies", Path: "server.trusted_proxies", Kind: configFlagStringArray, Usage: "proxy IP or CIDR whose X-Forwarded-For is honored; repeat for multiple; empty ignores forward headers", Scope: scopeRuntime},
		{Name: "server-cors-allowed-origins", Path: "server.cors.allowed_origins", Kind: configFlagStringArray, Usage: "CORS allowed origin; repeat for multiple origins; empty disables CORS", Scope: scopeRuntime},
		{Name: "server-cors-allowed-methods", Path: "server.cors.allowed_methods", Kind: configFlagStringArray, Usage: "CORS allowed method; repeat for multiple methods; empty uses REST defaults", Scope: scopeRuntime},
		{Name: "server-cors-allowed-headers", Path: "server.cors.allowed_headers", Kind: configFlagStringArray, Usage: "CORS allowed request header; repeat for multiple headers; empty uses REST defaults", Scope: scopeRuntime},
		{Name: "server-cors-exposed-headers", Path: "server.cors.exposed_headers", Kind: configFlagStringArray, Usage: "CORS response header exposed to browsers; repeat for multiple headers", Scope: scopeRuntime},
		{Name: "server-cors-max-age", Path: "server.cors.max_age", Kind: configFlagDuration, Usage: "CORS preflight cache duration", Scope: scopeRuntime},
		{Name: "server-cors-allow-credentials", Path: "server.cors.allow_credentials", Kind: configFlagBool, Usage: "allow credentials on CORS responses; cannot be used with wildcard origin", Scope: scopeRuntime},
		{Name: "server-write-timeout", Path: "server.write_timeout", Kind: configFlagDuration, Usage: "HTTP response write timeout", Scope: scopeStatic, Reason: "an http.Server field, no longer read after ListenAndServe"},
		{Name: "server-access-log-enabled", Path: "server.access_log_enabled", Kind: configFlagBool, Usage: "enable REST and S3 access logs", Scope: scopeRuntime},
		{Name: "server-shutdown-timeout", Path: "server.shutdown_timeout", Kind: configFlagDuration, Usage: "graceful shutdown timeout", Scope: scopeRuntime},
		{Name: "server-http2-cleartext", Path: "server.http2_cleartext", Kind: configFlagBool, Usage: "accept unencrypted HTTP/2 (h2c) on the REST listener, alongside HTTP/1.1 on the same port", Scope: scopeStatic, Reason: "the protocol set is fixed when the listener starts accepting connections"},
		{Name: "auth-bearer-token", Path: "auth.bearer_token", Kind: configFlagString, Usage: "REST bearer token", Scope: scopeStatic, Reason: "deliberately static: the break-glass credential must survive a damaged runtime store", Secret: true},
		{Name: "storage-base-paths", Path: "storage.base_paths", Kind: configFlagStringArray, Usage: "storage mount path; repeat for multiple mounts", Scope: scopeStatic, Reason: "mounts are bound into the service, seeded as index roots, and registered with the detector"},
		{Name: "storage-runtime-config-path", Path: "storage.runtime_config_path", Kind: configFlagString, Usage: "directory holding runtime config overrides and resources; must be outside the index", Scope: scopeStatic, Reason: "the runtime config store is opened at startup"},
		{Name: "storage-index-path", Path: "storage.index_path", Kind: configFlagString, Usage: "Pebble index directory", Scope: scopeStatic, Reason: "the Pebble index is opened at startup"},
		{Name: "detection-backend", Path: "detection.backend", Kind: configFlagString, Usage: "change detector backend: auto, poll, btrfs", Scope: scopeStatic, Reason: "selects a different detector implementation"},
		{Name: "detection-poll-interval", Path: "detection.poll_interval", Kind: configFlagDuration, Usage: "polling interval when poll detection is used", Scope: scopeRuntime},
		{Name: "cache-path-cache-size", Path: "cache.path_cache_size", Unit: "entries", Kind: configFlagInt, Usage: "maximum number of paths kept in the in-memory cache", Scope: scopeRuntime},
		{Name: "jobs-workers", Path: "jobs.workers", Unit: "workers", Kind: configFlagInt, Usage: "background worker count", Scope: scopeRuntime},
		{Name: "jobs-queue-size", Path: "jobs.queue_size", Unit: "jobs", Kind: configFlagInt, Usage: "maximum jobs queued before new ones are rejected", Scope: scopeRuntime},
		{Name: "jobs-thumbnail-workers", Path: "jobs.thumbnail_workers", Unit: "workers", Kind: configFlagInt, Usage: "thumbnail worker count", Scope: scopeRuntime},
		{Name: "jobs-thumbnail-queue-size", Path: "jobs.thumbnail_queue_size", Unit: "jobs", Kind: configFlagInt, Usage: "maximum thumbnail jobs queued before new ones are rejected", Scope: scopeRuntime},
		{Name: "upload-expiry", Path: "upload.expiry", Kind: configFlagDuration, Usage: "upload session expiry", Scope: scopeRuntime},
		{Name: "upload-cleanup-interval", Path: "upload.cleanup_interval", Kind: configFlagDuration, Usage: "upload session cleanup interval", Scope: scopeRuntime},
		{Name: "upload-max-chunk-bytes", Path: "upload.max_chunk_bytes", Unit: "bytes", Kind: configFlagInt64, Usage: "maximum single chunk size in bytes", Scope: scopeRuntime},
		{Name: "upload-max-upload-bytes", Path: "upload.max_upload_bytes", Unit: "bytes", Kind: configFlagInt64, Usage: "maximum one-shot upload size in bytes", Scope: scopeRuntime},
		{Name: "upload-max-session-upload-bytes", Path: "upload.max_session_upload_bytes", Unit: "bytes", Kind: configFlagInt64, Usage: "maximum upload-session size in bytes", Scope: scopeRuntime},
		{Name: "upload-max-concurrent-segment-writes", Path: "upload.max_concurrent_segment_writes", Unit: "writes", Kind: configFlagInt, Usage: "maximum concurrent segment writes", Scope: scopeRuntime},
		{Name: "upload-min-free-bytes", Path: "upload.min_free_bytes", Unit: "bytes", Kind: configFlagInt64, Usage: "minimum free bytes required before accepting uploads", Scope: scopeRuntime},
		{Name: "thumbnail-lru-cache-size", Path: "thumbnail.lru_cache_size", Unit: "entries", Kind: configFlagInt, Usage: "maximum number of thumbnails kept in memory", Scope: scopeRuntime},
		{Name: "thumbnail-max-source-bytes", Path: "thumbnail.max_source_bytes", Unit: "bytes", Kind: configFlagInt64, Usage: "maximum source file size for thumbnails", Scope: scopeRuntime},
		{Name: "thumbnail-max-pixels", Path: "thumbnail.max_pixels", Unit: "pixels", Kind: configFlagInt64, Usage: "maximum decoded pixels for thumbnails", Scope: scopeRuntime},
		{Name: "versioning-enabled", Path: "versioning.enabled", Kind: configFlagString, Usage: "versioning mode: auto, on, off", Scope: scopeRuntime},
		{Name: "versioning-cooldown", Path: "versioning.cooldown", Kind: configFlagDuration, Usage: "automatic version capture cooldown", Scope: scopeRuntime},
		{Name: "versioning-min-size-for-auto-v1", Path: "versioning.min_size_for_auto_v1", Unit: "bytes", Kind: configFlagInt64, Usage: "minimum size for automatic V1 capture", Scope: scopeRuntime},
		{Name: "versioning-retention-bucket", Path: "versioning.retention_buckets", Kind: configFlagRetentionBuckets, Usage: "retention bucket keep_for=<duration>,max_count=<n>; repeat for multiple buckets", Scope: scopeRuntime},
		{Name: "versioning-pruner-interval", Path: "versioning.pruner_interval", Kind: configFlagDuration, Usage: "versioning pruner interval", Scope: scopeRuntime},
		{Name: "versioning-max-pinned-per-file", Path: "versioning.max_pinned_per_file", Unit: "versions", Kind: configFlagInt, Usage: "maximum pinned versions per file; 0 disables cap", Scope: scopeRuntime},
		{Name: "versioning-pinned-grace-after-delete", Path: "versioning.pinned_grace_after_delete", Kind: configFlagDuration, Usage: "retention grace for pinned versions after live file delete", Scope: scopeRuntime},
		{Name: "versioning-max-label-bytes", Path: "versioning.max_label_bytes", Unit: "bytes", Kind: configFlagInt, Usage: "maximum version label bytes", Scope: scopeRuntime},
		{Name: "s3-enabled", Path: "s3.enabled", Kind: configFlagBool, Usage: "enable S3-compatible listener", Scope: scopeStatic, Reason: "controls whether the second listener exists"},
		{Name: "s3-listen", Path: "s3.listen", Kind: configFlagString, Usage: "S3 listener address", Scope: scopeStatic, Reason: "S3 listener bind address"},
		{Name: "s3-region", Path: "s3.region", Kind: configFlagString, Usage: "S3 SigV4 region", Scope: scopeRuntime},
		{Name: "s3-access-key", Path: "s3.access_key", Kind: configFlagString, Usage: "legacy single-tenant S3 access key", Scope: scopeRuntime, Secret: true},
		{Name: "s3-secret-key", Path: "s3.secret_key", Kind: configFlagString, Usage: "legacy single-tenant S3 secret key", Scope: scopeRuntime, Secret: true},
		{Name: "s3-max-concurrent-writes", Path: "s3.max_concurrent_writes", Unit: "writes", Kind: configFlagInt, Usage: "maximum concurrent S3 object and part writes", Scope: scopeRuntime},
		{Name: "s3-key", Path: "s3.keys", Kind: configFlagS3Keys, Usage: "S3 key access_key=<ak>,secret_key=<sk>,buckets=<a|b|*>,requests_per_second=<n>,burst=<n>; repeat for multiple keys", Scope: scopeRuntime, Secret: true},
		{Name: "s3-cleanup-done-retention", Path: "s3.cleanup.done_retention", Kind: configFlagDuration, Usage: "multipart done-manifest retention; zero uses adapter default", Scope: scopeRuntime},
		{Name: "s3-cleanup-aborted-retention", Path: "s3.cleanup.aborted_retention", Kind: configFlagDuration, Usage: "multipart aborted-manifest retention; zero uses adapter default", Scope: scopeRuntime},
		{Name: "s3-cleanup-stuck-upload-max-age", Path: "s3.cleanup.stuck_upload_max_age", Kind: configFlagDuration, Usage: "maximum age for stuck open multipart uploads; zero uses adapter default", Scope: scopeRuntime},
		{Name: "s3-cleanup-interval", Path: "s3.cleanup.interval", Kind: configFlagDuration, Usage: "multipart cleanup interval; negative disables", Scope: scopeRuntime},
		{Name: "metrics-enabled", Path: "metrics.enabled", Kind: configFlagBool, Usage: "enable Prometheus metrics endpoint", Scope: scopeStatic, Reason: "the metrics route is mounted conditionally during router construction"},
		{Name: "metrics-path", Path: "metrics.path", Kind: configFlagString, Usage: "Prometheus metrics path", Scope: scopeStatic, Reason: "the metrics route pattern is fixed at router construction"},
		{Name: "metrics-token", Path: "metrics.token", Kind: configFlagString, Usage: "optional Prometheus metrics bearer token", Scope: scopeRuntime, Secret: true},
		{Name: "activity-ring-buffer-size", Path: "activity.ring_buffer_size", Unit: "events", Kind: configFlagInt, Usage: "number of recent activity events kept in memory", Scope: scopeRuntime},
	}
}

func registerConfigFlags(flags *pflag.FlagSet) {
	for _, spec := range allConfigFlagSpecs() {
		switch spec.Kind {
		case configFlagString:
			flags.String(spec.Name, "", spec.Usage)
		case configFlagBool:
			flags.Bool(spec.Name, false, spec.Usage)
		case configFlagInt:
			flags.Int(spec.Name, 0, spec.Usage)
		case configFlagInt64:
			flags.Int64(spec.Name, 0, spec.Usage)
		case configFlagDuration:
			flags.Duration(spec.Name, 0, spec.Usage)
		case configFlagStringArray, configFlagS3Keys, configFlagRetentionBuckets:
			flags.StringArray(spec.Name, nil, spec.Usage)
		}
	}
}

func applyChangedConfigFlags(flags *pflag.FlagSet, cfg *domain.Config) error {
	for _, spec := range allConfigFlagSpecs() {
		if !flags.Changed(spec.Name) {
			continue
		}
		if err := applyChangedConfigFlag(flags, spec, cfg); err != nil {
			return err
		}
	}
	return validateResolvedConfig(*cfg)
}

func changedConfigFlagValues(flags *pflag.FlagSet) ([]configYAMLSet, error) {
	var sets []configYAMLSet
	for _, spec := range allConfigFlagSpecs() {
		if !flags.Changed(spec.Name) {
			continue
		}
		value, err := changedConfigFlagValue(flags, spec)
		if err != nil {
			return nil, err
		}
		sets = append(sets, configYAMLSet{Path: spec.Path, Value: value})
	}
	return sets, nil
}

func applyChangedConfigFlag(flags *pflag.FlagSet, spec configFlagSpec, cfg *domain.Config) error {
	switch spec.Path {
	case "server.listen":
		cfg.Server.Listen = getFlagString(flags, spec.Name)
	case "server.public_url":
		cfg.Server.PublicURL = strings.TrimRight(getFlagString(flags, spec.Name), "/")
	case "server.trusted_proxies":
		cfg.Server.TrustedProxies = cleanStringList(getFlagStringArray(flags, spec.Name))
	case "server.cors.allowed_origins":
		cfg.Server.CORS.AllowedOrigins = cleanStringList(getFlagStringArray(flags, spec.Name))
	case "server.cors.allowed_methods":
		cfg.Server.CORS.AllowedMethods = cleanStringList(getFlagStringArray(flags, spec.Name))
	case "server.cors.allowed_headers":
		cfg.Server.CORS.AllowedHeaders = cleanStringList(getFlagStringArray(flags, spec.Name))
	case "server.cors.exposed_headers":
		cfg.Server.CORS.ExposedHeaders = cleanStringList(getFlagStringArray(flags, spec.Name))
	case "server.cors.max_age":
		cfg.Server.CORS.MaxAge = getFlagDuration(flags, spec.Name)
	case "server.cors.allow_credentials":
		cfg.Server.CORS.AllowCredentials = getFlagBool(flags, spec.Name)
	case "server.write_timeout":
		cfg.Server.WriteTimeout = getFlagDuration(flags, spec.Name)
	case "server.access_log_enabled":
		cfg.Server.AccessLogEnabled = getFlagBool(flags, spec.Name)
	case "server.shutdown_timeout":
		cfg.Server.ShutdownTimeout = getFlagDuration(flags, spec.Name)
	case "auth.bearer_token":
		cfg.Auth.BearerToken = getFlagString(flags, spec.Name)
	case "storage.base_paths":
		cfg.Storage.BasePaths = getFlagStringArray(flags, spec.Name)
	case "storage.runtime_config_path":
		cfg.Storage.RuntimeConfigPath = strings.TrimSpace(getFlagString(flags, spec.Name))
	case "storage.index_path":
		cfg.Storage.IndexPath = getFlagString(flags, spec.Name)
	case "detection.backend":
		cfg.Detection.Backend = getFlagString(flags, spec.Name)
	case "detection.poll_interval":
		cfg.Detection.PollInterval = getFlagDuration(flags, spec.Name)
	case "cache.path_cache_size":
		cfg.Cache.PathCacheSize = getFlagInt(flags, spec.Name)
	case "jobs.workers":
		cfg.Jobs.Workers = getFlagInt(flags, spec.Name)
	case "jobs.queue_size":
		cfg.Jobs.QueueSize = getFlagInt(flags, spec.Name)
	case "jobs.thumbnail_workers":
		cfg.Jobs.ThumbnailWorkers = getFlagInt(flags, spec.Name)
	case "jobs.thumbnail_queue_size":
		cfg.Jobs.ThumbnailQueueSize = getFlagInt(flags, spec.Name)
	case "upload.expiry":
		cfg.Upload.Expiry = getFlagDuration(flags, spec.Name)
	case "upload.cleanup_interval":
		cfg.Upload.CleanupInterval = getFlagDuration(flags, spec.Name)
	case "upload.max_chunk_bytes":
		cfg.Upload.MaxChunkBytes = getFlagInt64(flags, spec.Name)
	case "upload.max_upload_bytes":
		cfg.Upload.MaxUploadBytes = getFlagInt64(flags, spec.Name)
	case "upload.max_session_upload_bytes":
		cfg.Upload.MaxSessionUploadBytes = getFlagInt64(flags, spec.Name)
	case "upload.max_concurrent_segment_writes":
		cfg.Upload.MaxConcurrentSegmentWrites = getFlagInt(flags, spec.Name)
	case "upload.min_free_bytes":
		cfg.Upload.MinFreeBytes = getFlagInt64(flags, spec.Name)
	case "thumbnail.lru_cache_size":
		cfg.Thumbnail.LRUCacheSize = getFlagInt(flags, spec.Name)
	case "thumbnail.max_source_bytes":
		cfg.Thumbnail.MaxSourceBytes = getFlagInt64(flags, spec.Name)
	case "thumbnail.max_pixels":
		cfg.Thumbnail.MaxPixels = getFlagInt64(flags, spec.Name)
	case "versioning.enabled":
		cfg.Versioning.Enabled = getFlagString(flags, spec.Name)
	case "versioning.cooldown":
		cfg.Versioning.Cooldown = getFlagDuration(flags, spec.Name)
	case "versioning.min_size_for_auto_v1":
		cfg.Versioning.MinSizeForAutoV1 = getFlagInt64(flags, spec.Name)
	case "versioning.retention_buckets":
		buckets, err := parseRetentionBucketFlags(getFlagStringArray(flags, spec.Name))
		if err != nil {
			return err
		}
		cfg.Versioning.RetentionBuckets = buckets
	case "versioning.pruner_interval":
		cfg.Versioning.PrunerInterval = getFlagDuration(flags, spec.Name)
	case "versioning.max_pinned_per_file":
		cfg.Versioning.MaxPinnedPerFile = getFlagInt(flags, spec.Name)
	case "versioning.pinned_grace_after_delete":
		cfg.Versioning.PinnedGraceAfterDelete = getFlagDuration(flags, spec.Name)
	case "versioning.max_label_bytes":
		cfg.Versioning.MaxLabelBytes = getFlagInt(flags, spec.Name)
	case "s3.enabled":
		cfg.S3.Enabled = getFlagBool(flags, spec.Name)
	case "s3.listen":
		cfg.S3.Listen = getFlagString(flags, spec.Name)
	case "s3.region":
		cfg.S3.Region = getFlagString(flags, spec.Name)
	case "s3.access_key":
		cfg.S3.AccessKey = getFlagString(flags, spec.Name)
	case "s3.secret_key":
		cfg.S3.SecretKey = getFlagString(flags, spec.Name)
	case "s3.max_concurrent_writes":
		cfg.S3.MaxConcurrentWrites = getFlagInt(flags, spec.Name)
	case "s3.keys":
		keys, err := parseS3KeyFlags(getFlagStringArray(flags, spec.Name))
		if err != nil {
			return err
		}
		cfg.S3.Keys = keys
	case "s3.cleanup.done_retention":
		cfg.S3.Cleanup.DoneRetention = getFlagDuration(flags, spec.Name)
	case "s3.cleanup.aborted_retention":
		cfg.S3.Cleanup.AbortedRetention = getFlagDuration(flags, spec.Name)
	case "s3.cleanup.stuck_upload_max_age":
		cfg.S3.Cleanup.StuckUploadMaxAge = getFlagDuration(flags, spec.Name)
	case "s3.cleanup.interval":
		cfg.S3.Cleanup.Interval = getFlagDuration(flags, spec.Name)
	case "metrics.enabled":
		cfg.Metrics.Enabled = getFlagBool(flags, spec.Name)
	case "metrics.path":
		cfg.Metrics.Path = getFlagString(flags, spec.Name)
	case "metrics.token":
		cfg.Metrics.Token = getFlagString(flags, spec.Name)
	case "activity.ring_buffer_size":
		cfg.Activity.RingBufferSize = getFlagInt(flags, spec.Name)
	default:
		return fmt.Errorf("unhandled config flag path %q", spec.Path)
	}
	return nil
}

func changedConfigFlagValue(flags *pflag.FlagSet, spec configFlagSpec) (any, error) {
	switch spec.Kind {
	case configFlagString:
		return getFlagString(flags, spec.Name), nil
	case configFlagBool:
		return getFlagBool(flags, spec.Name), nil
	case configFlagInt:
		return getFlagInt(flags, spec.Name), nil
	case configFlagInt64:
		return getFlagInt64(flags, spec.Name), nil
	case configFlagDuration:
		return getFlagDuration(flags, spec.Name).String(), nil
	case configFlagStringArray:
		return getFlagStringArray(flags, spec.Name), nil
	case configFlagS3Keys:
		return parseS3KeyFlags(getFlagStringArray(flags, spec.Name))
	case configFlagRetentionBuckets:
		return parseRetentionBucketFlags(getFlagStringArray(flags, spec.Name))
	default:
		return nil, fmt.Errorf("unhandled config flag kind for %s", spec.Name)
	}
}

func getFlagString(flags *pflag.FlagSet, name string) string {
	v, _ := flags.GetString(name)
	return v
}

func getFlagBool(flags *pflag.FlagSet, name string) bool {
	v, _ := flags.GetBool(name)
	return v
}

func getFlagInt(flags *pflag.FlagSet, name string) int {
	v, _ := flags.GetInt(name)
	return v
}

func getFlagInt64(flags *pflag.FlagSet, name string) int64 {
	v, _ := flags.GetInt64(name)
	return v
}

func getFlagDuration(flags *pflag.FlagSet, name string) time.Duration {
	v, _ := flags.GetDuration(name)
	return v
}

func getFlagStringArray(flags *pflag.FlagSet, name string) []string {
	v, _ := flags.GetStringArray(name)
	return v
}

func parseRetentionBucketFlags(raw []string) ([]domain.RetentionBucketConfig, error) {
	out := make([]domain.RetentionBucketConfig, 0, len(raw))
	for i, entry := range raw {
		kv := parseKVParts(entry)
		keepRaw := strings.TrimSpace(kv["keep_for"])
		if keepRaw == "" {
			keepRaw = strings.TrimSpace(kv["keep-for"])
		}
		if keepRaw == "" {
			return nil, fmt.Errorf("versioning-retention-bucket[%d]: keep_for is required", i)
		}
		keepFor, err := time.ParseDuration(keepRaw)
		if err != nil {
			return nil, fmt.Errorf("versioning-retention-bucket[%d]: keep_for: %w", i, err)
		}
		maxRaw := strings.TrimSpace(kv["max_count"])
		if maxRaw == "" {
			maxRaw = strings.TrimSpace(kv["max-count"])
		}
		if maxRaw == "" {
			return nil, fmt.Errorf("versioning-retention-bucket[%d]: max_count is required", i)
		}
		maxCount, err := strconv.Atoi(maxRaw)
		if err != nil {
			return nil, fmt.Errorf("versioning-retention-bucket[%d]: max_count: %w", i, err)
		}
		out = append(out, domain.RetentionBucketConfig{KeepFor: keepFor, MaxCount: maxCount})
	}
	return out, nil
}

func parseS3KeyFlags(raw []string) ([]domain.S3KeyConfig, error) {
	out := make([]domain.S3KeyConfig, 0, len(raw))
	for i, entry := range raw {
		kv := parseKVParts(entry)
		accessKey := firstNonEmpty(kv["access_key"], kv["access-key"], kv["access"])
		secretKey := firstNonEmpty(kv["secret_key"], kv["secret-key"], kv["secret"])
		if accessKey == "" || secretKey == "" {
			return nil, fmt.Errorf("s3-key[%d]: access_key and secret_key are required", i)
		}
		rps, err := parseOptionalNonNegativeInt(firstNonEmpty(kv["requests_per_second"], kv["requests-per-second"], kv["rps"]), "requests_per_second")
		if err != nil {
			return nil, fmt.Errorf("s3-key[%d]: %w", i, err)
		}
		burst, err := parseOptionalNonNegativeInt(kv["burst"], "burst")
		if err != nil {
			return nil, fmt.Errorf("s3-key[%d]: %w", i, err)
		}
		out = append(out, domain.S3KeyConfig{
			AccessKey:         accessKey,
			SecretKey:         secretKey,
			Buckets:           splitPipeList(kv["buckets"]),
			RequestsPerSecond: rps,
			Burst:             burst,
		})
	}
	return out, nil
}

func parseKVParts(raw string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			out[strings.ToLower(strings.TrimSpace(part))] = ""
			continue
		}
		out[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func splitPipeList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, "|")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func parseOptionalNonNegativeInt(raw, name string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("%s must be >= 0", name)
	}
	return n, nil
}

func validateResolvedConfig(cfg domain.Config) error {
	if len(cfg.Storage.BasePaths) == 0 {
		return fmt.Errorf("storage.base_paths is required")
	}
	// auth.bearer_token is no longer required: an empty one is generated on
	// first boot and stored, so a fresh install needs no configuration at all.
	if err := validateRuntimeConfigPath(cfg.Storage); err != nil {
		return err
	}
	if err := validatePublicURL(cfg.Server.PublicURL); err != nil {
		return err
	}
	if err := validateCORSConfig(cfg.Server.CORS); err != nil {
		return err
	}
	backend := strings.ToLower(strings.TrimSpace(cfg.Detection.Backend))
	if backend == "" {
		backend = "auto"
	}
	switch backend {
	case "auto", "poll", "btrfs":
	default:
		return fmt.Errorf("detection.backend must be one of: auto, poll, btrfs")
	}
	if cfg.Upload.MaxSessionUploadBytes < cfg.Upload.MaxChunkBytes {
		return fmt.Errorf("upload.max_session_upload_bytes must be >= upload.max_chunk_bytes")
	}
	versioningEnabled := strings.ToLower(strings.TrimSpace(cfg.Versioning.Enabled))
	switch versioningEnabled {
	case "", "auto", "on", "off":
	default:
		return fmt.Errorf("versioning.enabled must be one of: auto, on, off")
	}
	if cfg.Versioning.MaxPinnedPerFile < 0 {
		return fmt.Errorf("versioning.max_pinned_per_file must be >= 0 (use 0 explicitly to disable the cap)")
	}
	if err := validateS3Config(cfg); err != nil {
		return err
	}
	return nil
}

// validateRuntimeConfigPath enforces the separation the runtime store depends
// on. Rebuilding the index removes its directory outright, so a runtime store
// nested inside it would take every stored credential with it.
func validateRuntimeConfigPath(storage domain.StorageConfig) error {
	runtimePath := strings.TrimSpace(storage.RuntimeConfigPath)
	if runtimePath == "" {
		return fmt.Errorf("storage.runtime_config_path is required")
	}

	index, err := filepath.Abs(strings.TrimSpace(storage.IndexPath))
	if err != nil {
		return err
	}
	runtime, err := filepath.Abs(runtimePath)
	if err != nil {
		return err
	}

	if runtime == index || strings.HasPrefix(runtime, index+string(filepath.Separator)) {
		return fmt.Errorf("storage.runtime_config_path (%s) must not live inside storage.index_path (%s): rebuilding the index deletes that directory", runtime, index)
	}
	return nil
}

func validatePublicURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("server.public_url must be an absolute http(s) URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("server.public_url must use http or https")
	}
	return nil
}

func validateCORSConfig(cfg domain.CORSConfig) error {
	origins := cleanStringList(cfg.AllowedOrigins)
	if len(origins) == 0 {
		return nil
	}
	if cfg.MaxAge < 0 {
		return fmt.Errorf("server.cors.max_age must be >= 0")
	}
	for _, origin := range origins {
		if origin == "*" {
			if cfg.AllowCredentials {
				return fmt.Errorf("server.cors.allow_credentials cannot be true when allowed_origins contains *")
			}
			continue
		}
		u, err := url.Parse(origin)
		if err != nil || u.Scheme == "" || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("server.cors.allowed_origins must contain origins like https://app.example.com")
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("server.cors.allowed_origins must use http or https")
		}
	}
	return nil
}

func cleanStringList(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func validateS3Config(cfg domain.Config) error {
	mounts := configuredMountNames(cfg.Storage.BasePaths)
	if cfg.S3.MaxConcurrentWrites < 0 {
		return fmt.Errorf("s3.max_concurrent_writes must be >= 0 (use 0 explicitly for the default)")
	}
	if cfg.S3.Enabled {
		if cfg.S3.AccessKey == "" && cfg.S3.SecretKey == "" && len(cfg.S3.Keys) == 0 {
			return fmt.Errorf("s3.enabled=true requires either s3.access_key/s3.secret_key or at least one s3.keys entry")
		}
		for name := range mounts {
			if err := domain.ValidateBucketName(name); err != nil {
				return err
			}
		}
	}
	seen := map[string]struct{}{}
	if cfg.S3.AccessKey != "" || cfg.S3.SecretKey != "" {
		if cfg.S3.AccessKey == "" || cfg.S3.SecretKey == "" {
			return fmt.Errorf("s3.access_key and s3.secret_key must be set together")
		}
		seen[cfg.S3.AccessKey] = struct{}{}
	}
	for i, key := range cfg.S3.Keys {
		if key.AccessKey == "" || key.SecretKey == "" {
			return fmt.Errorf("s3.keys[%d]: access_key and secret_key must be non-empty", i)
		}
		if _, ok := seen[key.AccessKey]; ok {
			return fmt.Errorf("s3.keys[%d]: access key %q is duplicated", i, key.AccessKey)
		}
		seen[key.AccessKey] = struct{}{}
		if key.RequestsPerSecond < 0 {
			return fmt.Errorf("s3.keys[%d]: requests_per_second must be >= 0", i)
		}
		if key.Burst < 0 {
			return fmt.Errorf("s3.keys[%d]: burst must be >= 0", i)
		}
		for _, bucket := range key.Buckets {
			bucket = strings.TrimSpace(bucket)
			if bucket == "" || bucket == "*" {
				continue
			}
			if _, ok := mounts[bucket]; !ok {
				return fmt.Errorf("s3.keys[%d]: bucket %q is not a configured mount", i, bucket)
			}
		}
	}
	return nil
}

func configuredMountNames(paths []string) map[string]struct{} {
	out := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		name := filepath.Base(filepath.Clean(p))
		if name != "." && name != string(filepath.Separator) && strings.TrimSpace(name) != "" {
			out[name] = struct{}{}
		}
	}
	return out
}
