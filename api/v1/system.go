package v1

// Types for the operational endpoints: GET /v1/system/info, GET /v1/system/runtime,
// GET /v1/health and GET /v1/uploads/sessions.
//
// The split between info and runtime is deliberate. Info probes the mounts,
// which touches the filesystem, so it is meant to be read occasionally. Runtime
// is made of cheap in-memory counters and is safe to poll for a live dashboard.

// BuildInfo identifies the running binary.
type BuildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Go      string `json:"go"`
}

// MountInfo reports a configured mount and the outcome of its health probe.
//
// Writable and XAttrSupported are the two properties that silently break
// Filegate when absent: a read-only mount rejects every write, and a mount
// without user xattr support cannot carry stable file IDs. They were previously
// checked once at startup and then discarded.
type MountInfo struct {
	Name           string   `json:"name"`
	Path           string   `json:"path"`
	Exists         bool     `json:"exists"`
	Writable       bool     `json:"writable"`
	XAttrSupported bool     `json:"xattrSupported"`
	FreeBytes      uint64   `json:"freeBytes"`
	TotalBytes     uint64   `json:"totalBytes"`
	Errors         []string `json:"errors,omitempty"`
}

// VersioningInfo reports the effective versioning configuration. Enabled is the
// resolved answer, which for the "auto" mode depends on whether the mounts are
// btrfs and was previously only visible in a startup log line.
type VersioningInfo struct {
	Enabled          bool   `json:"enabled"`
	Mode             string `json:"mode"`
	CooldownMs       int64  `json:"cooldownMs"`
	PrunerIntervalMs int64  `json:"prunerIntervalMs"`
	MaxPinnedPerFile int    `json:"maxPinnedPerFile"`
}

// LimitsInfo is the curated, non-secret slice of configuration an operator
// needs to interpret rejections. This is deliberately an allowlist rather than a
// dump of the config file: every field here is one someone chose to publish.
type LimitsInfo struct {
	MaxChunkBytes              int64 `json:"maxChunkBytes"`
	MaxUploadBytes             int64 `json:"maxUploadBytes"`
	MaxSessionUploadBytes      int64 `json:"maxSessionUploadBytes"`
	MaxConcurrentSegmentWrites int   `json:"maxConcurrentSegmentWrites"`
	UploadMinFreeBytes         int64 `json:"uploadMinFreeBytes"`
	UploadExpiryMs             int64 `json:"uploadExpiryMs"`
	UploadCleanupIntervalMs    int64 `json:"uploadCleanupIntervalMs"`
	ThumbnailMaxSourceBytes    int64 `json:"thumbnailMaxSourceBytes"`
	ThumbnailMaxPixels         int64 `json:"thumbnailMaxPixels"`
	PathCacheCapacity          int   `json:"pathCacheCapacity"`
	ActivityRingCapacity       int   `json:"activityRingCapacity"`
}

// SystemInfoResponse is the body of GET /v1/system/info.
type SystemInfoResponse struct {
	GeneratedAt int64          `json:"generatedAt"`
	Build       BuildInfo      `json:"build"`
	StartedAt   int64          `json:"startedAt"`
	UptimeMs    int64          `json:"uptimeMs"`
	Detector    DetectorInfo   `json:"detector"`
	Versioning  VersioningInfo `json:"versioning"`
	Limits      LimitsInfo     `json:"limits"`
	Mounts      []MountInfo    `json:"mounts"`
	IndexPath   string         `json:"indexPath"`
}

// DetectorInfo is the static half of detector state.
type DetectorInfo struct {
	Backend    string `json:"backend"`
	IntervalMs int64  `json:"intervalMs"`
}

// DetectorRuntime is the live half: whether detection is actually keeping up.
//
// LastScanAt falling far behind IntervalMs is the signal that the detector
// goroutine died, which otherwise causes silent index drift for writes that did
// not come through the API.
type DetectorRuntime struct {
	Backend            string            `json:"backend"`
	IntervalMs         int64             `json:"intervalMs"`
	Cycles             uint64            `json:"cycles"`
	LastScanAt         int64             `json:"lastScanAt"`
	LastScanDurationMs int64             `json:"lastScanDurationMs"`
	StaleForMs         int64             `json:"staleForMs"`
	Errors             uint64            `json:"errors"`
	PendingBatches     int               `json:"pendingBatches"`
	QueueCapacity      int               `json:"queueCapacity"`
	TrackedDirs        int               `json:"trackedDirs,omitempty"`
	TrackedFiles       int               `json:"trackedFiles,omitempty"`
	Generations        map[string]uint64 `json:"generations,omitempty"`
}

// JobsRuntime reports worker-pool saturation. Queued approaching QueueCapacity
// is what precedes 503 responses from the thumbnail endpoint, which is the only
// consumer of the pool today.
type JobsRuntime struct {
	Workers       int    `json:"workers"`
	Queued        int    `json:"queued"`
	QueueCapacity int    `json:"queueCapacity"`
	InFlight      int    `json:"inFlight"`
	Rejected      uint64 `json:"rejected"`
	Panics        uint64 `json:"panics"`
}

// CacheRuntime reports occupancy and cumulative effectiveness of one cache.
type CacheRuntime struct {
	Entries  int     `json:"entries"`
	Capacity int     `json:"capacity"`
	Hits     uint64  `json:"hits"`
	Misses   uint64  `json:"misses"`
	HitRatio float64 `json:"hitRatio"`
}

// UploadSessionsRuntime counts resumable upload sessions by phase, plus the
// concurrent segment-write slots currently held. The slot limit is published
// via /v1/capabilities; this is the matching usage figure.
type UploadSessionsRuntime struct {
	InProgress      int `json:"inProgress"`
	Committing      int `json:"committing"`
	Committed       int `json:"committed"`
	Aborted         int `json:"aborted"`
	WriteSlotsInUse int `json:"writeSlotsInUse"`
	WriteSlotsLimit int `json:"writeSlotsLimit"`
}

// LifecycleRuntime reports the last background maintenance run.
//
// All six PruneStats fields are here, not the three that reach Prometheus:
// OrphansPurged and BlobsDeleted are what tell an operator whether retention is
// actually reclaiming space.
type LifecycleRuntime struct {
	PrunerIntervalMs int64 `json:"prunerIntervalMs"`
	// LastPruneAt is zero when no round has completed yet, which is different
	// from a round that found nothing to do.
	LastPruneAt         int64  `json:"lastPruneAt"`
	LastPruneDurationMs int64  `json:"lastPruneDurationMs"`
	NextPruneAt         int64  `json:"nextPruneAt"`
	PruneRuns           uint64 `json:"pruneRuns"`
	FilesScanned        int    `json:"filesScanned"`
	VersionsKept        int    `json:"versionsKept"`
	VersionsDeleted     int    `json:"versionsDeleted"`
	OrphansPurged       int    `json:"orphansPurged"`
	BlobsDeleted        int    `json:"blobsDeleted"`
	PruneErrors         int    `json:"pruneErrors"`
	// PruneError carries the message when the last run failed outright.
	PruneError string `json:"pruneError,omitempty"`
}

// SystemRuntimeResponse is the body of GET /v1/system/runtime. Every field is an
// in-memory counter, so this endpoint is safe to poll.
type SystemRuntimeResponse struct {
	GeneratedAt    int64                 `json:"generatedAt"`
	Detector       DetectorRuntime       `json:"detector"`
	Jobs           JobsRuntime           `json:"jobs"`
	PathCache      CacheRuntime          `json:"pathCache"`
	ThumbnailCache CacheRuntime          `json:"thumbnailCache"`
	UploadSessions UploadSessionsRuntime `json:"uploadSessions"`
	Lifecycle      LifecycleRuntime      `json:"lifecycle"`
}

// HealthCheck is one dependency probe.
type HealthCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Health status values. Degraded means serving continues but something needs
// attention; fail means the dependency is unusable.
const (
	HealthOK       = "ok"
	HealthDegraded = "degraded"
	HealthFail     = "fail"
)

// HealthResponse is the body of GET /v1/health. Unlike the bare GET /health
// liveness probe, this one actually checks dependencies.
type HealthResponse struct {
	Status      string        `json:"status"`
	GeneratedAt int64         `json:"generatedAt"`
	Checks      []HealthCheck `json:"checks"`
}

// UploadSessionSummary is one row of GET /v1/uploads/sessions. It omits the
// staging directory and ownership, which are internal placement details.
type UploadSessionSummary struct {
	ID               string `json:"id"`
	Path             string `json:"path"`
	Size             int64  `json:"size"`
	SegmentSize      int64  `json:"segmentSize"`
	TotalSegments    int    `json:"totalSegments"`
	UploadedSegments int    `json:"uploadedSegments"`
	UploadedBytes    int64  `json:"uploadedBytes"`
	Phase            string `json:"phase"`
	CreatedAt        int64  `json:"createdAt"`
	UpdatedAt        int64  `json:"updatedAt"`
	AgeMs            int64  `json:"ageMs"`
	ContentType      string `json:"contentType,omitempty"`
}

// UploadSessionListResponse is the body of GET /v1/uploads/sessions.
type UploadSessionListResponse struct {
	Items []UploadSessionSummary `json:"items"`
	Total int                    `json:"total"`
}
