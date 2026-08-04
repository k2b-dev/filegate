package httpadapter

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	apiv1 "github.com/valentinkolb/filegate/api/v1"
	"github.com/valentinkolb/filegate/domain"
	"github.com/valentinkolb/filegate/infra/cache"
	"github.com/valentinkolb/filegate/infra/detect"
	"github.com/valentinkolb/filegate/infra/filesystem"
	"github.com/valentinkolb/filegate/infra/jobs"
)

// systemReporter answers the operational endpoints. It reads state that the
// server already tracks; nothing here mutates anything.
type systemReporter struct {
	svc       *domain.Service
	opts      RouterOptions
	live      liveConfig
	thumbs    *thumbnailer
	uploads   *uploadSessionManager
	startedAt time.Time
}

func newSystemReporter(svc *domain.Service, opts RouterOptions, live liveConfig, thumbs *thumbnailer, uploads *uploadSessionManager) *systemReporter {
	return &systemReporter{
		svc:       svc,
		opts:      opts,
		live:      live,
		thumbs:    thumbs,
		uploads:   uploads,
		startedAt: time.Now(),
	}
}

func (r *systemReporter) detectorStats() detect.Stats {
	if r.opts.DetectorStats == nil {
		return detect.Stats{Backend: "unknown"}
	}
	return r.opts.DetectorStats()
}

// handleInfo serves GET /v1/system/info. It probes mount health, so it touches
// the filesystem and is meant to be read occasionally rather than polled.
func (r *systemReporter) handleInfo(w http.ResponseWriter, _ *http.Request) {
	now := time.Now()
	detector := r.detectorStats()

	info := apiv1.SystemInfoResponse{
		GeneratedAt: now.UnixMilli(),
		Build: apiv1.BuildInfo{
			Version: fallback(r.opts.BuildVersion, "dev"),
			Commit:  fallback(r.opts.BuildCommit, "none"),
			Go:      runtime.Version(),
		},
		StartedAt: r.startedAt.UnixMilli(),
		UptimeMs:  now.Sub(r.startedAt).Milliseconds(),
		Detector: apiv1.DetectorInfo{
			Backend:    detector.Backend,
			IntervalMs: detector.Interval.Milliseconds(),
		},
		Versioning: apiv1.VersioningInfo{
			Enabled:          r.opts.VersioningEnabled,
			Mode:             fallback(r.opts.VersioningMode, "auto"),
			CooldownMs:       r.opts.VersioningCooldown.Milliseconds(),
			PrunerIntervalMs: r.opts.VersioningPrunerInterval.Milliseconds(),
			MaxPinnedPerFile: r.opts.VersioningMaxPinnedPerFile,
		},
		Limits: apiv1.LimitsInfo{
			MaxChunkBytes:              r.live.maxChunkBytes(),
			MaxUploadBytes:             r.live.maxUploadBytes(),
			MaxSessionUploadBytes:      r.live.maxSessionUploadBytes(),
			MaxConcurrentSegmentWrites: r.opts.MaxConcurrentSegmentWrites,
			UploadMinFreeBytes:         r.live.uploadMinFreeBytes(),
			UploadExpiryMs:             r.opts.UploadExpiry.Milliseconds(),
			UploadCleanupIntervalMs:    r.opts.UploadCleanupInterval.Milliseconds(),
			ThumbnailMaxSourceBytes:    r.opts.ThumbnailMaxSourceBytes,
			ThumbnailMaxPixels:         r.opts.ThumbnailMaxPixels,
			PathCacheCapacity:          r.opts.PathCacheSize,
			ActivityRingCapacity:       r.opts.ActivityLog.Capacity(),
		},
		Mounts:    r.mountInfo(),
		IndexPath: r.opts.IndexPath,
	}

	writeJSON(w, http.StatusOK, info)
}

func (r *systemReporter) mountInfo() []apiv1.MountInfo {
	paths := r.opts.BasePaths
	out := make([]apiv1.MountInfo, 0, len(paths))
	for _, health := range filesystem.CheckMountsHealth(paths) {
		out = append(out, apiv1.MountInfo{
			Name:           filepath.Base(health.Path),
			Path:           health.Path,
			Exists:         health.Exists,
			Writable:       health.Writable,
			XAttrSupported: health.XAttrSupported,
			FreeBytes:      health.FreeBytes,
			TotalBytes:     health.TotalBytes,
			Errors:         health.Errors,
		})
	}
	return out
}

// handleRuntime serves GET /v1/system/runtime. Every value is an in-memory
// counter, so this is the endpoint a dashboard should poll.
func (r *systemReporter) handleRuntime(w http.ResponseWriter, _ *http.Request) {
	now := time.Now()
	detector := r.detectorStats()

	staleFor := int64(0)
	if !detector.LastScanAt.IsZero() {
		staleFor = now.Sub(detector.LastScanAt).Milliseconds()
	}
	lastScanAt := int64(0)
	if !detector.LastScanAt.IsZero() {
		lastScanAt = detector.LastScanAt.UnixMilli()
	}

	pathEntries, pathCapacity, pathHits, pathMisses := r.svc.PathCacheStats()

	out := apiv1.SystemRuntimeResponse{
		GeneratedAt: now.UnixMilli(),
		Detector: apiv1.DetectorRuntime{
			Backend:            detector.Backend,
			IntervalMs:         detector.Interval.Milliseconds(),
			Cycles:             detector.Cycles,
			LastScanAt:         lastScanAt,
			LastScanDurationMs: detector.LastScanDuration.Milliseconds(),
			StaleForMs:         staleFor,
			Errors:             detector.Errors,
			PendingBatches:     detector.PendingBatches,
			QueueCapacity:      detector.QueueCapacity,
			TrackedDirs:        detector.TrackedDirs,
			TrackedFiles:       detector.TrackedFiles,
			Generations:        detector.Generations,
		},
		Jobs:           jobsRuntime(r.thumbs.schedulerStats()),
		PathCache:      cacheRuntime(pathEntries, pathCapacity, pathHits, pathMisses),
		ThumbnailCache: thumbCacheRuntime(r.thumbs.cacheStats()),
		UploadSessions: r.uploadSessionRuntime(),
		Lifecycle:      r.lifecycleRuntime(),
	}

	writeJSON(w, http.StatusOK, out)
}

func (r *systemReporter) lifecycleRuntime() apiv1.LifecycleRuntime {
	if r.opts.Lifecycle == nil {
		return apiv1.LifecycleRuntime{}
	}
	return r.opts.Lifecycle()
}

func (r *systemReporter) uploadSessionRuntime() apiv1.UploadSessionsRuntime {
	out := apiv1.UploadSessionsRuntime{
		WriteSlotsInUse: r.uploads.writeSlotsInUse(),
		WriteSlotsLimit: r.uploads.writeSlotsLimit(),
	}
	counts := map[domain.UploadSessionPhase]*int{
		domain.UploadSessionInProgress: &out.InProgress,
		domain.UploadSessionCommitting: &out.Committing,
		domain.UploadSessionCommitted:  &out.Committed,
		domain.UploadSessionAborted:    &out.Aborted,
	}
	for phase, target := range counts {
		sessions, err := r.svc.ListUploadSessions(phase)
		if err != nil {
			continue
		}
		*target = len(sessions)
	}
	return out
}

// handleHealth serves GET /v1/health: a real dependency check, unlike the bare
// GET /health liveness probe which only proves the process is listening.
func (r *systemReporter) handleHealth(w http.ResponseWriter, _ *http.Request) {
	checks := make([]apiv1.HealthCheck, 0, 3)
	status := apiv1.HealthOK

	degrade := func(to string) {
		if to == apiv1.HealthFail {
			status = apiv1.HealthFail
			return
		}
		if status == apiv1.HealthOK {
			status = apiv1.HealthDegraded
		}
	}

	// Index: a point lookup is the cheapest proof that Pebble answers. Stats
	// would also prove it but walks every entity.
	if err := r.svc.PingIndex(); err != nil {
		checks = append(checks, apiv1.HealthCheck{Name: "index", Status: apiv1.HealthFail, Detail: err.Error()})
		degrade(apiv1.HealthFail)
	} else {
		checks = append(checks, apiv1.HealthCheck{Name: "index", Status: apiv1.HealthOK})
	}

	// Detector: silence well past the scan interval means the goroutine died,
	// which causes silent index drift rather than an obvious outage.
	checks = append(checks, r.detectorHealth(&degrade))

	// Mounts: existence only. Writability needs a write probe, which belongs on
	// the occasional /v1/system/info rather than on a pollable health endpoint.
	if missing := missingMounts(r.opts.BasePaths); len(missing) > 0 {
		checks = append(checks, apiv1.HealthCheck{Name: "mounts", Status: apiv1.HealthFail, Detail: "unreachable: " + joinPaths(missing)})
		degrade(apiv1.HealthFail)
	} else {
		checks = append(checks, apiv1.HealthCheck{Name: "mounts", Status: apiv1.HealthOK})
	}

	code := http.StatusOK
	if status == apiv1.HealthFail {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, apiv1.HealthResponse{
		Status:      status,
		GeneratedAt: time.Now().UnixMilli(),
		Checks:      checks,
	})
}

// detectorStaleFactor is how many scan intervals may elapse before detection is
// considered stalled. Scans can overrun their interval under load, so a small
// multiple avoids flapping while still catching a dead goroutine quickly.
const detectorStaleFactor = 5

func (r *systemReporter) detectorHealth(degrade *func(string)) apiv1.HealthCheck {
	stats := r.detectorStats()
	if stats.Interval <= 0 {
		return apiv1.HealthCheck{Name: "detector", Status: apiv1.HealthOK, Detail: "not configured"}
	}
	if stats.LastScanAt.IsZero() {
		// Startup has not completed a first round yet. Not an error on its own.
		return apiv1.HealthCheck{Name: "detector", Status: apiv1.HealthOK, Detail: "awaiting first scan"}
	}

	stale := time.Since(stats.LastScanAt)
	if stale > stats.Interval*detectorStaleFactor {
		(*degrade)(apiv1.HealthDegraded)
		return apiv1.HealthCheck{
			Name:   "detector",
			Status: apiv1.HealthDegraded,
			Detail: "no scan for " + stale.Round(time.Second).String() + "; external filesystem changes may not be indexed",
		}
	}
	return apiv1.HealthCheck{Name: "detector", Status: apiv1.HealthOK}
}

// handlePrune serves POST /v1/versions/prune.
//
// A manual trigger exists because the background loop runs on an interval an
// operator cannot see the effect of: after tightening a retention policy, the
// obvious next question is whether it did anything.
//
// This deletes data, so it is a POST, it is recorded in the activity log by the
// middleware, and it refuses to start while a round is already in flight rather
// than doubling the work.
func (r *systemReporter) handlePrune(w http.ResponseWriter, _ *http.Request) {
	if r.opts.PruneNow == nil {
		writeErr(w, http.StatusNotImplemented, "manual pruning is not available")
		return
	}

	started := time.Now()
	stats, err := r.opts.PruneNow()
	if err != nil {
		if strings.Contains(err.Error(), "already in progress") {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, apiv1.PruneResponse{
		FilesScanned:    stats.FilesScanned,
		VersionsKept:    stats.VersionsKept,
		VersionsDeleted: stats.VersionsDeleted,
		OrphansPurged:   stats.OrphansPurged,
		BlobsDeleted:    stats.BlobsDeleted,
		Errors:          stats.Errors,
		DurationMs:      time.Since(started).Milliseconds(),
	})
}

// handleListUploadSessions serves GET /v1/uploads/sessions. Without it an
// interrupted upload leaves a session that nothing can find, only abort by id.
func (r *systemReporter) handleListUploadSessions(w http.ResponseWriter, req *http.Request) {
	phases := []domain.UploadSessionPhase{
		domain.UploadSessionInProgress,
		domain.UploadSessionCommitting,
		domain.UploadSessionCommitted,
		domain.UploadSessionAborted,
	}
	if requested := req.URL.Query().Get("phase"); requested != "" {
		phase := domain.UploadSessionPhase(requested)
		if !validUploadPhase(phase) {
			writeErr(w, http.StatusBadRequest, "phase must be one of in_progress, committing, committed, aborted")
			return
		}
		phases = []domain.UploadSessionPhase{phase}
	}

	now := time.Now().UnixMilli()
	items := make([]apiv1.UploadSessionSummary, 0, 16)
	for _, phase := range phases {
		sessions, err := r.svc.ListUploadSessions(phase)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, session := range sessions {
			items = append(items, r.summarizeSession(session, now))
		}
	}

	writeJSON(w, http.StatusOK, apiv1.UploadSessionListResponse{Items: items, Total: len(items)})
}

func (r *systemReporter) summarizeSession(session domain.UploadSession, now int64) apiv1.UploadSessionSummary {
	uploaded := 0
	var uploadedBytes int64
	if segments, err := r.svc.ListUploadSegments(session.ID); err == nil {
		uploaded = len(segments)
		for _, segment := range segments {
			uploadedBytes += segment.Size
		}
	}
	return apiv1.UploadSessionSummary{
		ID:               session.ID,
		Path:             session.Path,
		Size:             session.Size,
		SegmentSize:      session.SegmentSize,
		TotalSegments:    session.TotalSegments,
		UploadedSegments: uploaded,
		UploadedBytes:    uploadedBytes,
		Phase:            string(session.Phase),
		CreatedAt:        session.CreatedAt,
		UpdatedAt:        session.UpdatedAt,
		AgeMs:            now - session.CreatedAt,
		ContentType:      session.ContentType,
	}
}

func validUploadPhase(phase domain.UploadSessionPhase) bool {
	switch phase {
	case domain.UploadSessionInProgress, domain.UploadSessionCommitting, domain.UploadSessionCommitted, domain.UploadSessionAborted:
		return true
	default:
		return false
	}
}

func jobsRuntime(stats jobs.Stats) apiv1.JobsRuntime {
	return apiv1.JobsRuntime{
		Workers:       stats.Workers,
		Queued:        stats.Queued,
		QueueCapacity: stats.QueueCapacity,
		InFlight:      stats.InFlight,
		Rejected:      stats.Rejected,
		Panics:        stats.Panics,
	}
}

func thumbCacheRuntime(stats cache.Stats) apiv1.CacheRuntime {
	return cacheRuntime(stats.Entries, stats.Capacity, stats.Hits, stats.Misses)
}

// missingMounts returns the configured mounts that cannot be reached at all.
// Existence only: a write probe belongs on /v1/system/info, not on an endpoint
// meant to be polled.
func missingMounts(paths []string) []string {
	var missing []string
	for _, path := range paths {
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			missing = append(missing, path)
		}
	}
	return missing
}

func cacheRuntime(entries, capacity int, hits, misses uint64) apiv1.CacheRuntime {
	ratio := 0.0
	if total := hits + misses; total > 0 {
		ratio = float64(hits) / float64(total)
	}
	return apiv1.CacheRuntime{Entries: entries, Capacity: capacity, Hits: hits, Misses: misses, HitRatio: ratio}
}

func fallback(value, def string) string {
	if value == "" {
		return def
	}
	return value
}

func joinPaths(paths []string) string {
	out := ""
	for i, path := range paths {
		if i > 0 {
			out += ", "
		}
		out += path
	}
	return out
}
