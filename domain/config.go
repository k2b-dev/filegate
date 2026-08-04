package domain

import "time"

// Config is the top-level configuration for the Filegate service.
type Config struct {
	Server     ServerConfig     `mapstructure:"server"`
	Auth       AuthConfig       `mapstructure:"auth"`
	Storage    StorageConfig    `mapstructure:"storage"`
	Detection  DetectionConfig  `mapstructure:"detection"`
	Cache      CacheConfig      `mapstructure:"cache"`
	Jobs       JobsConfig       `mapstructure:"jobs"`
	Upload     UploadConfig     `mapstructure:"upload"`
	Thumbnail  ThumbnailConfig  `mapstructure:"thumbnail"`
	Versioning VersioningConfig `mapstructure:"versioning"`
	S3         S3Config         `mapstructure:"s3"`
	Metrics    MetricsConfig    `mapstructure:"metrics"`
	Activity   ActivityConfig   `mapstructure:"activity"`
}

// ActivityConfig controls the in-process activity log exposed through
// /v1/activity. It is intentionally a bounded ring buffer: good for
// operator introspection and admin UX, not durable audit retention.
type ActivityConfig struct {
	RingBufferSize int `mapstructure:"ring_buffer_size"`
}

// MetricsConfig controls the optional Prometheus /metrics endpoint.
// The endpoint is served on the EXISTING REST listener (no extra
// port) at Path. Auth is layered: if Token is set it is required;
// otherwise the REST Auth.BearerToken is required; if neither is set
// the endpoint is served openly (suitable for an internal-only
// network where the scraper has no credentials).
//
// Enabled defaults to false — REST/S3-only deployments pay nothing.
type MetricsConfig struct {
	Enabled bool   `mapstructure:"enabled"`
	Path    string `mapstructure:"path"`
	Token   string `mapstructure:"token"`
}

// S3Config controls the optional S3-compatible HTTP frontend. When
// Enabled is true, every configured mount name must satisfy the
// S3 bucket-name rules (see ValidateBucketName) — Filegate startup
// fails loudly if any mount fails the check, so the operator catches
// the misconfiguration before clients hit it. When Enabled is false
// (the default), bucket-name validation is skipped and the listener
// is not started — REST-only deployments shouldn't care about
// DNS-safety of mount names.
//
// Listen is a "host:port" string the S3 HTTP server binds to. Region
// is the value clients must use in their SigV4 credential scope —
// "us-east-1" works for any operator who doesn't care.
//
// Auth model:
//
//   - The Keys list is the multi-tenant key store. Every entry pairs
//     an access key with its secret and a per-key bucket whitelist.
//     The whitelist is enforced on every operation: ListBuckets is
//     filtered to the allowed set, and any access to a bucket not in
//     the list returns AccessDenied — bucket existence is NOT
//     revealed (forbidden buckets answer 403, not 404).
//
//   - AccessKey + SecretKey at the top level are the legacy single-
//     tenant convenience knobs from M1. When set, they're folded
//     into the keys list as one entry with full bucket access ("*"
//     wildcard). They're kept for backward compatibility with
//     existing configs and trivial single-tenant deployments.
//
// At least one credential source (Keys or AccessKey/SecretKey) must
// be set when Enabled=true — startup fails loudly otherwise.
type S3Config struct {
	Enabled   bool   `mapstructure:"enabled"`
	Listen    string `mapstructure:"listen"`
	Region    string `mapstructure:"region"`
	AccessKey string `mapstructure:"access_key"`
	SecretKey string `mapstructure:"secret_key"`
	// MaxConcurrentWrites bounds concurrent S3 object/part writes.
	// This protects file descriptors and staging space when clients
	// upload many large files in parallel. 0 means use the default.
	MaxConcurrentWrites int `mapstructure:"max_concurrent_writes"`

	// Keys is the multi-tenant key store. Each entry exists
	// independently and is matched at request time by AccessKey;
	// whatever the request signs with determines which bucket
	// whitelist applies. Order is irrelevant.
	Keys []S3KeyConfig `mapstructure:"keys"`

	// Cleanup controls the recurring sweep that retires finished
	// multipart manifests + their durable Pebble records, and
	// forcibly aborts uploads stuck open past a max age. Without
	// it, .fg-uploads/s3-* and the 0x07 keyspace grow unbounded.
	// All zero values mean "use defaults" — see
	// MultipartCleanupConfig in the s3 adapter for the policy.
	Cleanup S3CleanupConfig `mapstructure:"cleanup"`
}

// S3CleanupConfig controls the multipart-cleanup sweep cadence
// and retention policy. Zero values fall back to the adapter's
// DefaultMultipartCleanupConfig (24h done / 1h aborted / 7d
// stuck-upload, 1h interval). Set Interval to a negative duration
// to disable the loop entirely (operator opts out — staging dir
// growth is then their responsibility).
type S3CleanupConfig struct {
	DoneRetention     time.Duration `mapstructure:"done_retention"`
	AbortedRetention  time.Duration `mapstructure:"aborted_retention"`
	StuckUploadMaxAge time.Duration `mapstructure:"stuck_upload_max_age"`
	Interval          time.Duration `mapstructure:"interval"`
}

// S3KeyConfig is one entry in the multi-tenant S3 key store. The
// Buckets list is the per-key whitelist of accessible buckets. The
// special wildcard "*" grants access to every configured mount —
// useful for an "admin" key — and is the only allowed wildcard form
// (we deliberately don't support glob patterns; explicit lists are
// auditable).
//
// An empty Buckets slice means the key has no access at all. Such
// a key authenticates successfully but every operation returns
// AccessDenied — useful for staging revocation without deleting the
// entry.
type S3KeyConfig struct {
	AccessKey string   `mapstructure:"access_key"`
	SecretKey string   `mapstructure:"secret_key"`
	Buckets   []string `mapstructure:"buckets"`

	// RequestsPerSecond throttles the key's sustained request
	// rate. 0 (default) means unlimited. Burst defaults to RPS
	// when unset. Over-limit requests get 503 SlowDown — every
	// real S3 SDK honours that with exponential backoff. Use to
	// blast-radius-limit a misbehaving or compromised key
	// without disabling it entirely.
	RequestsPerSecond int `mapstructure:"requests_per_second"`
	Burst             int `mapstructure:"burst"`
}

// ServerConfig controls HTTP server behavior.
//
// TrustedProxies lists the peers (IPs or CIDRs) whose
// X-Forwarded-For / X-Real-Ip headers are honored for the logged
// client address. Empty (the default) means the headers are ignored —
// any direct client could otherwise spoof its logged IP. Operators
// behind Traefik/Caddy/nginx list the proxy's address here.
type ServerConfig struct {
	Listen           string        `mapstructure:"listen"`
	PublicURL        string        `mapstructure:"public_url"`
	TrustedProxies   []string      `mapstructure:"trusted_proxies"`
	CORS             CORSConfig    `mapstructure:"cors"`
	WriteTimeout     time.Duration `mapstructure:"write_timeout"`
	AccessLogEnabled bool          `mapstructure:"access_log_enabled"`
	// ShutdownTimeout bounds how long the daemon waits for
	// in-flight HTTP handlers to finish on SIGINT/SIGTERM
	// before force-closing the listener. The S3 multipart
	// Complete path can take several seconds on large uploads
	// (concat parts → fsync → rename → Pebble batch); a too-
	// short timeout aborts those mid-commit. Default: 60s.
	// Connections still active after the timeout get force-
	// closed via http.Server.Close — clients see RST, the
	// crash-recovery sweep handles any half-committed state.
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
	// HTTP2Cleartext accepts unencrypted HTTP/2 on the REST
	// listener, alongside HTTP/1.1 on the same port. Off by
	// default: h2c only engages for a client that opens with the
	// HTTP/2 preface, but accepting a second protocol on a
	// listener is an operator's decision, not a default.
	//
	// This is for a reverse proxy configured to speak h2 to its
	// backends. Browsers cannot use it — they require TLS for
	// HTTP/2, and the edge terminates that. It buys no measurable
	// throughput either; see bench/results.
	HTTP2Cleartext bool `mapstructure:"http2_cleartext"`
}

// CORSConfig controls optional browser cross-origin access for the REST
// listener. Empty AllowedOrigins disables CORS entirely.
type CORSConfig struct {
	AllowedOrigins   []string      `mapstructure:"allowed_origins"`
	AllowedMethods   []string      `mapstructure:"allowed_methods"`
	AllowedHeaders   []string      `mapstructure:"allowed_headers"`
	ExposedHeaders   []string      `mapstructure:"exposed_headers"`
	MaxAge           time.Duration `mapstructure:"max_age"`
	AllowCredentials bool          `mapstructure:"allow_credentials"`
}

// AuthConfig contains authentication settings.
type AuthConfig struct {
	BearerToken string `mapstructure:"bearer_token"`
}

// StorageConfig specifies filesystem paths for data and index storage.
type StorageConfig struct {
	BasePaths []string `mapstructure:"base_paths"`
	IndexPath string   `mapstructure:"index_path"`
	// RuntimeConfigPath holds the applied manifest and runtime resources
	// such as S3 access keys. It must not sit inside IndexPath: rebuilding
	// the index removes that directory outright, and the index is a derived
	// artifact while this store is authoritative.
	RuntimeConfigPath string `mapstructure:"runtime_config_path"`
}

// DetectionConfig controls the filesystem change detection backend.
type DetectionConfig struct {
	Backend      string        `mapstructure:"backend"`
	PollInterval time.Duration `mapstructure:"poll_interval"`
}

// CacheConfig controls in-memory cache sizes.
type CacheConfig struct {
	PathCacheSize int `mapstructure:"path_cache_size"`
}

// JobsConfig controls the bounded worker pool for background tasks.
type JobsConfig struct {
	Workers            int `mapstructure:"workers"`
	QueueSize          int `mapstructure:"queue_size"`
	ThumbnailWorkers   int `mapstructure:"thumbnail_workers"`
	ThumbnailQueueSize int `mapstructure:"thumbnail_queue_size"`
}

// UploadConfig controls one-shot upload and upload-session lifecycle limits.
type UploadConfig struct {
	Expiry                     time.Duration `mapstructure:"expiry"`
	CleanupInterval            time.Duration `mapstructure:"cleanup_interval"`
	MaxChunkBytes              int64         `mapstructure:"max_chunk_bytes"`
	MaxUploadBytes             int64         `mapstructure:"max_upload_bytes"`
	MaxSessionUploadBytes      int64         `mapstructure:"max_session_upload_bytes"`
	MaxConcurrentSegmentWrites int           `mapstructure:"max_concurrent_segment_writes"`
	MinFreeBytes               int64         `mapstructure:"min_free_bytes"`
}

// ThumbnailConfig controls on-demand thumbnail generation behavior.
type ThumbnailConfig struct {
	LRUCacheSize   int   `mapstructure:"lru_cache_size"`
	MaxSourceBytes int64 `mapstructure:"max_source_bytes"`
	MaxPixels      int64 `mapstructure:"max_pixels"`
}

// VersioningConfig controls per-file version history. The feature is
// HTTP-only (external writes via cp/rsync are not captured) and requires
// btrfs reflinks for storage efficiency. Per-mount auto-detection makes
// btrfs mounts opt-in and ext4 mounts no-op.
//
// Enabled values:
//   - "auto"  : btrfs mounts get versioning, ext4 silently disabled
//   - "on"    : forced on; non-btrfs mounts return ErrUnsupportedFS
//   - "off"   : feature globally disabled
//
// Cooldown bounds auto-capture noise: a write within Cooldown of the
// previous version captures nothing. Manual snapshots ignore cooldown.
//
// MinSizeForAutoV1 skips automatic V1 capture for tiny files where
// reflink savings don't offset metadata churn (config files, dotfiles).
// Manual snapshots ignore the floor.
type VersioningConfig struct {
	Enabled                string                  `mapstructure:"enabled"`
	Cooldown               time.Duration           `mapstructure:"cooldown"`
	MinSizeForAutoV1       int64                   `mapstructure:"min_size_for_auto_v1"`
	RetentionBuckets       []RetentionBucketConfig `mapstructure:"retention_buckets"`
	PrunerInterval         time.Duration           `mapstructure:"pruner_interval"`
	MaxPinnedPerFile       int                     `mapstructure:"max_pinned_per_file"`
	PinnedGraceAfterDelete time.Duration           `mapstructure:"pinned_grace_after_delete"`
	MaxLabelBytes          int                     `mapstructure:"max_label_bytes"`
}

// RetentionBucketConfig defines one age window of the bucketed exponential
// decay retention. KeepFor is the bucket window from "now"; MaxCount is the
// number of versions to retain inside it (-1 = unlimited).
//
// The pruner places target points evenly across each bucket and keeps the
// nearest version to each target. Newer buckets win on overlap so a
// version is never double-counted.
type RetentionBucketConfig struct {
	KeepFor  time.Duration `mapstructure:"keep_for"`
	MaxCount int           `mapstructure:"max_count"`
}
