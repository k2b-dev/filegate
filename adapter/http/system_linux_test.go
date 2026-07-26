//go:build linux

package httpadapter

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/valentinkolb/filegate/api/v1"
	"github.com/valentinkolb/filegate/domain"
	"github.com/valentinkolb/filegate/infra/detect"
)

func decodeJSON[T any](t *testing.T, r http.Handler, target string) (T, int) {
	t.Helper()

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(http.MethodGet, target))

	var out T
	if w.Result().StatusCode == http.StatusOK || w.Result().StatusCode == http.StatusServiceUnavailable {
		if err := json.NewDecoder(w.Result().Body).Decode(&out); err != nil {
			t.Fatalf("decode %s: %v", target, err)
		}
	}
	return out, w.Result().StatusCode
}

func TestSystemInfoReportsBuildMountsAndLimits(t *testing.T) {
	base := t.TempDir()
	opts := RouterOptions{
		BuildVersion:               "1.2.3",
		BuildCommit:                "abc1234",
		BasePaths:                  []string{base},
		PathCacheSize:              4096,
		MaxUploadBytes:             1 << 20,
		VersioningEnabled:          true,
		VersioningMode:             "on",
		VersioningCooldown:         15 * time.Minute,
		VersioningMaxPinnedPerFile: 100,
		DetectorStats: func() detect.Stats {
			return detect.Stats{Backend: "poll", Interval: 3 * time.Second}
		},
	}
	r, _, cleanup := newTestRouterWithBasePathsAndOptions(t, []string{base}, opts)
	defer cleanup()

	info, status := decodeJSON[apiv1.SystemInfoResponse](t, r, "/v1/system/info")
	if status != http.StatusOK {
		t.Fatalf("status=%d, want 200", status)
	}

	if info.Build.Version != "1.2.3" || info.Build.Commit != "abc1234" {
		t.Errorf("build = %+v, want version 1.2.3 commit abc1234", info.Build)
	}
	if info.Build.Go == "" {
		t.Error("build.go is empty")
	}
	if info.Detector.Backend != "poll" || info.Detector.IntervalMs != 3000 {
		t.Errorf("detector = %+v, want poll at 3000ms", info.Detector)
	}
	if !info.Versioning.Enabled || info.Versioning.Mode != "on" {
		t.Errorf("versioning = %+v, want enabled in mode on", info.Versioning)
	}
	if info.Limits.PathCacheCapacity != 4096 {
		t.Errorf("pathCacheCapacity = %d, want 4096", info.Limits.PathCacheCapacity)
	}
	if len(info.Mounts) != 1 {
		t.Fatalf("mounts = %d, want 1", len(info.Mounts))
	}
	mount := info.Mounts[0]
	if !mount.Exists || !mount.Writable {
		t.Errorf("mount = %+v, want an existing writable mount", mount)
	}
	if mount.Path != base {
		t.Errorf("mount path = %q, want %q", mount.Path, base)
	}
	if info.UptimeMs < 0 {
		t.Errorf("uptime = %d, want >= 0", info.UptimeMs)
	}
}

func TestSystemInfoWithoutDetectorReportsUnknownBackend(t *testing.T) {
	// The router must stay usable when the caller supplies no detector hook,
	// which is how every existing test constructs it.
	r, _, cleanup := newTestRouter(t)
	defer cleanup()

	info, status := decodeJSON[apiv1.SystemInfoResponse](t, r, "/v1/system/info")
	if status != http.StatusOK {
		t.Fatalf("status=%d, want 200", status)
	}
	if info.Detector.Backend != "unknown" {
		t.Errorf("detector backend = %q, want unknown", info.Detector.Backend)
	}
}

func TestSystemRuntimeReportsDetectorAndPools(t *testing.T) {
	lastScan := time.Now().Add(-2 * time.Second)
	opts := RouterOptions{
		DetectorStats: func() detect.Stats {
			return detect.Stats{
				Backend:        "btrfs",
				Interval:       2 * time.Second,
				Cycles:         42,
				LastScanAt:     lastScan,
				Errors:         3,
				PendingBatches: 1,
				QueueCapacity:  64,
				Generations:    map[string]uint64{"/data": 99},
			}
		},
	}
	base := t.TempDir()
	r, svc, cleanup := newTestRouterWithBasePathsAndOptions(t, []string{base}, opts)
	defer cleanup()

	// Touch a path so the cache records at least one lookup.
	root := svc.ListRoot()[0]
	if _, err := svc.CreateChild(root.ID, "a.txt", false, nil); err != nil {
		t.Fatalf("create child: %v", err)
	}

	rt, status := decodeJSON[apiv1.SystemRuntimeResponse](t, r, "/v1/system/runtime")
	if status != http.StatusOK {
		t.Fatalf("status=%d, want 200", status)
	}

	if rt.Detector.Backend != "btrfs" || rt.Detector.Cycles != 42 || rt.Detector.Errors != 3 {
		t.Errorf("detector = %+v, want btrfs with 42 cycles and 3 errors", rt.Detector)
	}
	if rt.Detector.Generations["/data"] != 99 {
		t.Errorf("generations = %v, want /data at 99", rt.Detector.Generations)
	}
	if rt.Detector.StaleForMs < 1000 {
		t.Errorf("staleForMs = %d, want at least the ~2s since the last scan", rt.Detector.StaleForMs)
	}
	if rt.Jobs.QueueCapacity <= 0 {
		t.Errorf("jobs queue capacity = %d, want > 0", rt.Jobs.QueueCapacity)
	}
	if rt.PathCache.Capacity <= 0 {
		t.Errorf("path cache capacity = %d, want > 0", rt.PathCache.Capacity)
	}
	if rt.UploadSessions.WriteSlotsLimit <= 0 {
		t.Errorf("write slot limit = %d, want > 0", rt.UploadSessions.WriteSlotsLimit)
	}
}

func TestHealthReportsDependencies(t *testing.T) {
	base := t.TempDir()
	opts := RouterOptions{
		BasePaths: []string{base},
		DetectorStats: func() detect.Stats {
			return detect.Stats{Backend: "poll", Interval: 3 * time.Second, LastScanAt: time.Now()}
		},
	}
	r, _, cleanup := newTestRouterWithBasePathsAndOptions(t, []string{base}, opts)
	defer cleanup()

	health, status := decodeJSON[apiv1.HealthResponse](t, r, "/v1/health")
	if status != http.StatusOK {
		t.Fatalf("status=%d, want 200", status)
	}
	if health.Status != apiv1.HealthOK {
		t.Errorf("status = %q, want ok; checks=%+v", health.Status, health.Checks)
	}
	if len(health.Checks) != 3 {
		t.Fatalf("checks = %d, want index, detector and mounts", len(health.Checks))
	}
}

func TestHealthDegradesWhenDetectorStalls(t *testing.T) {
	// A detector goroutine that died is the failure this endpoint exists for:
	// writes made outside the API silently stop being indexed.
	base := t.TempDir()
	opts := RouterOptions{
		BasePaths: []string{base},
		DetectorStats: func() detect.Stats {
			return detect.Stats{
				Backend:    "poll",
				Interval:   time.Second,
				LastScanAt: time.Now().Add(-time.Hour),
			}
		},
	}
	r, _, cleanup := newTestRouterWithBasePathsAndOptions(t, []string{base}, opts)
	defer cleanup()

	health, status := decodeJSON[apiv1.HealthResponse](t, r, "/v1/health")
	if status != http.StatusOK {
		t.Fatalf("status=%d, want 200 for degraded", status)
	}
	if health.Status != apiv1.HealthDegraded {
		t.Fatalf("status = %q, want degraded; checks=%+v", health.Status, health.Checks)
	}

	var detector *apiv1.HealthCheck
	for i := range health.Checks {
		if health.Checks[i].Name == "detector" {
			detector = &health.Checks[i]
		}
	}
	if detector == nil || detector.Status != apiv1.HealthDegraded {
		t.Fatalf("detector check = %+v, want degraded", detector)
	}
	if detector.Detail == "" {
		t.Error("degraded detector check has no detail explaining the staleness")
	}
}

func TestHealthFailsWhenMountIsGone(t *testing.T) {
	base := t.TempDir()
	opts := RouterOptions{BasePaths: []string{base, base + "-does-not-exist"}}
	r, _, cleanup := newTestRouterWithBasePathsAndOptions(t, []string{base}, opts)
	defer cleanup()

	health, status := decodeJSON[apiv1.HealthResponse](t, r, "/v1/health")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503 when a mount is unreachable", status)
	}
	if health.Status != apiv1.HealthFail {
		t.Errorf("status = %q, want fail", health.Status)
	}
}

func TestPlainHealthEndpointIsUnchanged(t *testing.T) {
	// Existing liveness probes point at GET /health and must keep working.
	r, _, cleanup := newTestRouter(t)
	defer cleanup()

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", w.Result().StatusCode)
	}
	if got := w.Body.String(); got != "OK" {
		t.Errorf("body = %q, want OK", got)
	}
}

func TestListUploadSessionsSurfacesOrphans(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	session := domain.UploadSession{
		ID:            "session-orphan",
		Path:          root.Name + "/big.bin",
		ParentID:      root.ID,
		Filename:      "big.bin",
		Size:          4096,
		SegmentSize:   1024,
		TotalSegments: 4,
		Phase:         domain.UploadSessionInProgress,
		CreatedAt:     time.Now().Add(-time.Hour).UnixMilli(),
		UpdatedAt:     time.Now().Add(-time.Hour).UnixMilli(),
	}
	if err := svc.CreateUploadSession(session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	list, status := decodeJSON[apiv1.UploadSessionListResponse](t, r, "/v1/uploads/sessions")
	if status != http.StatusOK {
		t.Fatalf("status=%d, want 200", status)
	}
	if list.Total != 1 || len(list.Items) != 1 {
		t.Fatalf("total=%d items=%d, want exactly the one orphan", list.Total, len(list.Items))
	}

	item := list.Items[0]
	if item.ID != "session-orphan" {
		t.Errorf("id = %q, want session-orphan", item.ID)
	}
	if item.Phase != string(domain.UploadSessionInProgress) {
		t.Errorf("phase = %q, want in_progress", item.Phase)
	}
	if item.AgeMs < int64(time.Minute/time.Millisecond) {
		t.Errorf("ageMs = %d, want roughly an hour", item.AgeMs)
	}
	if item.TotalSegments != 4 || item.UploadedSegments != 0 {
		t.Errorf("segments = %d/%d, want 0 of 4", item.UploadedSegments, item.TotalSegments)
	}
}

func TestListUploadSessionsFiltersByPhase(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	for id, phase := range map[string]domain.UploadSessionPhase{
		"live":    domain.UploadSessionInProgress,
		"stopped": domain.UploadSessionAborted,
	} {
		if err := svc.CreateUploadSession(domain.UploadSession{
			ID: id, Path: root.Name + "/" + id, ParentID: root.ID, Filename: id,
			Phase: phase, CreatedAt: time.Now().UnixMilli(),
		}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}

	list, status := decodeJSON[apiv1.UploadSessionListResponse](t, r, "/v1/uploads/sessions?phase=aborted")
	if status != http.StatusOK {
		t.Fatalf("status=%d, want 200", status)
	}
	if list.Total != 1 || list.Items[0].ID != "stopped" {
		t.Fatalf("got %+v, want only the aborted session", list.Items)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(http.MethodGet, "/v1/uploads/sessions?phase=nonsense"))
	if w.Result().StatusCode != http.StatusBadRequest {
		t.Errorf("unknown phase status=%d, want 400", w.Result().StatusCode)
	}
}

func TestSystemEndpointsRequireAuth(t *testing.T) {
	r, _, cleanup := newTestRouter(t)
	defer cleanup()

	for _, target := range []string{"/v1/system/info", "/v1/system/runtime", "/v1/health", "/v1/uploads/sessions"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
		if w.Result().StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without a token: status=%d, want 401", target, w.Result().StatusCode)
		}
	}
}

// An oversized body is a client error. Before this mapping only the
// direct-upload path translated it, so a plain PUT answered 500 and gave the
// caller nothing to act on.
func TestOversizedUploadAnswers413(t *testing.T) {
	base := t.TempDir()
	r, svc, cleanup := newTestRouterWithBasePathsAndOptions(t, []string{base}, RouterOptions{MaxUploadBytes: 16})
	defer cleanup()

	root := svc.ListRoot()[0]
	body := strings.Repeat("x", 1024)

	req := authedRequest(http.MethodPut, "/v1/paths/"+root.Name+"/big.txt")
	req.Body = io.NopCloser(strings.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got := w.Result().StatusCode; got != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", got)
	}
}

func TestManualPruneReportsWhatItDid(t *testing.T) {
	var calls int
	opts := RouterOptions{
		PruneNow: func() (domain.PruneStats, error) {
			calls++
			return domain.PruneStats{FilesScanned: 12, VersionsKept: 30, VersionsDeleted: 4, OrphansPurged: 1, BlobsDeleted: 5}, nil
		},
	}
	base := t.TempDir()
	r, _, cleanup := newTestRouterWithBasePathsAndOptions(t, []string{base}, opts)
	defer cleanup()

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(http.MethodPost, "/v1/versions/prune"))
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Result().StatusCode)
	}

	var out apiv1.PruneResponse
	if err := json.NewDecoder(w.Result().Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// All six fields, not the three that reach Prometheus: orphans and blobs
	// are what say whether space was actually reclaimed.
	if out.FilesScanned != 12 || out.VersionsDeleted != 4 || out.OrphansPurged != 1 || out.BlobsDeleted != 5 {
		t.Errorf("stats = %+v, want the full result", out)
	}
	if calls != 1 {
		t.Errorf("prune called %d times, want 1", calls)
	}
}

// A round already in flight must be refused, not queued behind the first: two
// overlapping scans duplicate the work and report halves of it separately.
func TestManualPruneRefusesWhenAlreadyRunning(t *testing.T) {
	opts := RouterOptions{
		PruneNow: func() (domain.PruneStats, error) {
			return domain.PruneStats{}, errors.New("a pruning round is already in progress")
		},
	}
	base := t.TempDir()
	r, _, cleanup := newTestRouterWithBasePathsAndOptions(t, []string{base}, opts)
	defer cleanup()

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(http.MethodPost, "/v1/versions/prune"))
	if w.Result().StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Result().StatusCode)
	}
}

// Without versioning there is nothing to prune, and saying so beats pretending
// a round ran and found nothing.
func TestManualPruneUnavailableWithoutTheHook(t *testing.T) {
	r, _, cleanup := newTestRouter(t)
	defer cleanup()

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(http.MethodPost, "/v1/versions/prune"))
	if w.Result().StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", w.Result().StatusCode)
	}
}
