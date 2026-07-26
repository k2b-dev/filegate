package httpadapter

import (
	"archive/tar"
	"context"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	apiv1 "github.com/valentinkolb/filegate/api/v1"
	"github.com/valentinkolb/filegate/domain"
	"github.com/valentinkolb/filegate/infra/activity"
	"github.com/valentinkolb/filegate/infra/detect"
	"github.com/valentinkolb/filegate/infra/jobs"
)

// RouterOptions configures the HTTP router including authentication, upload limits,
// thumbnail generation, and background job worker pools.
type RouterOptions struct {
	BearerToken      string
	AccessLogEnabled bool
	PublicURL        string
	// TrustedProxies are the peers whose X-Forwarded-For / X-Real-Ip
	// headers are honored (see ParseTrustedProxies). Empty = headers
	// ignored.
	TrustedProxies             []netip.Prefix
	CORS                       domain.CORSConfig
	IndexPath                  string
	JobWorkers                 int
	JobQueueSize               int
	ThumbnailJobWorkers        int
	ThumbnailJobQueueSize      int
	UploadExpiry               time.Duration
	UploadCleanupInterval      time.Duration
	MaxChunkBytes              int64
	MaxUploadBytes             int64
	MaxSessionUploadBytes      int64
	MaxConcurrentSegmentWrites int
	UploadMinFreeBytes         int64
	ThumbnailLRUCacheSize      int
	ThumbnailMaxSourceBytes    int64
	ThumbnailMaxPixels         int64
	Rescan                     func() error

	// MetricsHandler, when non-nil, is mounted at MetricsPath on the
	// REST listener (no separate port). Auth is layered: MetricsToken
	// if set, else the REST BearerToken, else open. The caller
	// (cli/serve.go) validates MetricsPath does not collide with /v1
	// or /health before constructing the router.
	MetricsHandler http.Handler
	MetricsPath    string
	MetricsToken   string
	ActivityLog    *activity.Ring

	// Config is the live snapshot. Runtime-scoped handlers read from it per
	// request so a change applies without a restart; nil falls back to the
	// values captured in this struct, which keeps existing callers working.
	Config *domain.ConfigHolder

	// Operational context for GET /v1/system/info, /v1/system/runtime and
	// /v1/health. All optional: zero values degrade the reported detail
	// rather than breaking the endpoints, which keeps existing router
	// callers (including tests) working unchanged.
	BuildVersion string
	BuildCommit  string
	BasePaths    []string
	// PathCacheSize is the configured capacity, reported alongside the live
	// occupancy the service tracks.
	PathCacheSize int
	// DetectorStats returns live detector state. Nil means the router reports
	// an unknown backend instead of guessing.
	DetectorStats func() detect.Stats

	VersioningEnabled          bool
	VersioningMode             string
	VersioningCooldown         time.Duration
	VersioningPrunerInterval   time.Duration
	VersioningMaxPinnedPerFile int
}

type closeableHandler struct {
	handler   http.Handler
	closeFn   func()
	closeOnce sync.Once
}

type middlewareFunc func(http.Handler) http.Handler

type requestIDKey struct{}
type activityActorKey struct{}

type activityActorHolder struct {
	actor activity.Actor
}

var reqIDCounter uint64

var copyBufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 128*1024)
		return &buf
	},
}

var mountInfoCache struct {
	mu      sync.Mutex
	loaded  time.Time
	entries []mountInfo
}

func (h *closeableHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.handler.ServeHTTP(w, r)
}

func (h *closeableHandler) Close() error {
	if h == nil {
		return nil
	}
	h.closeOnce.Do(func() {
		if h.closeFn != nil {
			h.closeFn()
		}
	})
	return nil
}

// NewRouter constructs the HTTP handler tree with all routes, middleware, and background workers.
func NewRouter(svc *domain.Service, opts RouterOptions) http.Handler {
	root := http.NewServeMux()

	thumbnailWorkers := resolveThumbnailJobWorkers(opts)
	thumbnailQueueSize := resolveThumbnailQueueSize(opts)

	thumbnailScheduler := jobs.New(thumbnailWorkers, thumbnailQueueSize)
	directUploads := newDirectUploadManager(svc, opts.BearerToken, opts.PublicURL, opts.MaxUploadBytes, opts.TrustedProxies)
	directDownloads := newDirectDownloadManager(svc, opts.BearerToken, opts.PublicURL, opts.TrustedProxies)
	uploadSessions := newUploadSessionManager(
		svc,
		opts.BearerToken,
		opts.PublicURL,
		opts.MaxChunkBytes,
		opts.MaxSessionUploadBytes,
		opts.MaxConcurrentSegmentWrites,
		opts.UploadMinFreeBytes,
		opts.UploadExpiry,
		opts.UploadCleanupInterval,
		opts.TrustedProxies,
	)
	thumbs := newThumbnailer(
		svc,
		opts.ThumbnailLRUCacheSize,
		opts.ThumbnailMaxSourceBytes,
		opts.ThumbnailMaxPixels,
		thumbnailScheduler,
	)

	root.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	// Prometheus /metrics on the REST listener with layered-token
	// auth. Mounted only when the caller supplies a handler (i.e.
	// metrics.enabled). The caller pre-validates the path.
	if opts.MetricsHandler != nil {
		metricsPath := opts.MetricsPath
		if strings.TrimSpace(metricsPath) == "" {
			metricsPath = "/metrics"
		}
		root.Handle("GET "+metricsPath,
			metricsAuthMiddleware(opts.MetricsToken, opts.BearerToken)(opts.MetricsHandler))
	}

	root.HandleFunc("PUT /v1/uploads/direct/{token}", directUploads.handlePut)
	root.HandleFunc("GET /v1/downloads/direct/{token}", directDownloads.handleGet)
	root.HandleFunc("HEAD /v1/downloads/direct/{token}", directDownloads.handleGet)
	root.HandleFunc("GET /v1/uploads/sessions/{sessionId}", uploadSessions.handleStatus)
	root.HandleFunc("PUT /v1/uploads/sessions/{sessionId}/segments/{index}", uploadSessions.handlePutSegment)
	root.HandleFunc("POST /v1/uploads/sessions/{sessionId}/commit", uploadSessions.handleCommit)
	root.HandleFunc("DELETE /v1/uploads/sessions/{sessionId}", uploadSessions.handleAbort)

	auth := authMiddleware(opts.BearerToken, opts.ActivityLog)
	handleV1 := func(pattern string, handler http.HandlerFunc) {
		root.Handle(pattern, auth(http.HandlerFunc(handler)))
	}

	system := newSystemReporter(svc, opts, thumbs, uploadSessions)
	handleV1("GET /v1/system/info", system.handleInfo)
	handleV1("GET /v1/system/runtime", system.handleRuntime)
	handleV1("GET /v1/health", system.handleHealth)
	handleV1("GET /v1/uploads/sessions", system.handleListUploadSessions)

	handleV1("GET /v1/stats", func(w http.ResponseWriter, _ *http.Request) {
		stats, err := svc.Stats()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "failed to collect stats")
			return
		}

		indexDBSizeBytes := int64(0)
		if strings.TrimSpace(opts.IndexPath) != "" {
			if sz, err := dirSizeBytes(opts.IndexPath); err == nil {
				indexDBSizeBytes = sz
			}
		}

		mounts := make([]apiv1.StatsMount, 0, len(stats.Mounts))
		for _, m := range stats.Mounts {
			mounts = append(mounts, apiv1.StatsMount{
				ID:    m.ID.String(),
				Name:  m.Name,
				Path:  m.Path,
				Files: m.Files,
				Dirs:  m.Dirs,
			})
		}

		writeJSON(w, http.StatusOK, apiv1.StatsResponse{
			GeneratedAt: stats.GeneratedAt,
			Index: apiv1.StatsIndex{
				TotalEntities: stats.TotalEntities,
				TotalFiles:    stats.TotalFiles,
				TotalDirs:     stats.TotalDirs,
				DBSizeBytes:   indexDBSizeBytes,
			},
			Cache: apiv1.StatsCache{
				PathEntries:   stats.PathCacheEntries,
				PathCapacity:  stats.PathCacheCapacity,
				PathUtilRatio: stats.PathCacheUtilRatio,
			},
			Mounts: mounts,
			Disks:  collectDiskUsage(stats.Mounts, svc),
			System: collectSystemStats(),
		})
	})

	handleV1("GET /v1/activity", func(w http.ResponseWriter, r *http.Request) {
		limit := parseIntDefault(r.URL.Query().Get("limit"), 100)
		if limit < 0 {
			limit = 0
		}
		if limit > 1000 {
			limit = 1000
		}
		offset := parseIntDefault(r.URL.Query().Get("offset"), 0)
		if offset < 0 {
			offset = 0
		}
		writeJSON(w, http.StatusOK, activityListResponse(opts.ActivityLog, activityListQuery{
			limit:     limit,
			offset:    offset,
			q:         r.URL.Query().Get("q"),
			operation: r.URL.Query().Get("operation"),
			outcome:   r.URL.Query().Get("outcome"),
		}))
	})

	handleV1("GET /v1/capabilities", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, apiv1.CapabilitiesResponse{
			Uploads: apiv1.UploadCapabilities{
				MaxChunkBytes:              uploadSessions.maxSegmentBytes,
				MaxUploadBytes:             opts.MaxUploadBytes,
				MaxSessionUploadBytes:      uploadSessions.maxUploadBytes,
				MaxConcurrentSegmentWrites: uploadSessions.maxWrites,
			},
		})
	})

	handleV1("GET /v1/paths/{$}", func(w http.ResponseWriter, r *http.Request) {
		computeRecursiveSizes := strings.EqualFold(r.URL.Query().Get("computeRecursiveSizes"), "true")
		fingerprint, err := parseFingerprintMode(r.URL.Query().Get("fingerprint"))
		if err != nil {
			statusFromErr(w, err)
			return
		}
		mounts := svc.ListRoot()
		items := make([]apiv1.Node, 0, len(mounts))
		for _, m := range mounts {
			meta, err := svc.GetFile(m.ID)
			if err != nil {
				continue
			}
			if meta.Type == "directory" && computeRecursiveSizes {
				copyMeta := *meta
				if size, ok := svc.RecursiveDirectorySize(meta.ID); ok {
					copyMeta.Size = size
				}
				meta = &copyMeta
			}
			items = append(items, nodeResponseForFingerprint(meta, fingerprint))
		}
		writeJSON(w, http.StatusOK, apiv1.NodeListResponse{Items: items, Total: len(items)})
	})

	handleV1("GET /v1/paths/{path...}", func(w http.ResponseWriter, r *http.Request) {
		vp := strings.TrimPrefix(r.PathValue("path"), "/")
		if strings.TrimSpace(vp) == "" {
			writeErr(w, http.StatusBadRequest, "path required")
			return
		}
		meta, err := svc.GetFileByVirtualPath(vp)
		if err != nil {
			statusFromErr(w, err)
			return
		}
		respondMetaWithOptionalChildren(w, r, svc, meta)
	})

	handleV1("PUT /v1/paths/{path...}", func(w http.ResponseWriter, r *http.Request) {
		vp := strings.TrimPrefix(r.PathValue("path"), "/")
		if strings.TrimSpace(vp) == "" {
			writeErr(w, http.StatusBadRequest, "path required")
			return
		}
		mode, err := domain.ParseConflictMode(r.URL.Query().Get("onConflict"), domain.FileConflictModes)
		if err != nil {
			statusFromErr(w, err)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, opts.MaxUploadBytes)
		meta, created, err := svc.WriteContentByVirtualPath(vp, r.Body, mode)
		if err != nil {
			if errors.Is(err, domain.ErrConflict) {
				existingID, existingPath := lookupExistingByPath(svc, vp)
				writeConflict(w, "path already exists", existingID, existingPath)
				return
			}
			statusFromErr(w, err)
			return
		}
		w.Header().Set("X-Node-Id", meta.ID.String())
		if created {
			w.Header().Set("X-Created-Id", meta.ID.String())
			writeJSON(w, http.StatusCreated, nodeResponse(meta))
			return
		}
		writeJSON(w, http.StatusOK, nodeResponse(meta))
	})

	handleV1("GET /v1/nodes/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseID(w, r.PathValue("id"))
		if !ok {
			return
		}
		respondNodeWithOptionalChildren(w, r, svc, id)
	})

	handleV1("GET /v1/nodes/{id}/content", func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseID(w, r.PathValue("id"))
		if !ok {
			return
		}
		streamNodeContent(w, r, svc, id, r.URL.Query().Get("inline") == "true")
	})

	handleV1("GET /v1/nodes/{id}/thumbnail", thumbs.handleGet)

	handleV1("POST /v1/nodes/{id}/mkdir", func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseID(w, r.PathValue("id"))
		if !ok {
			return
		}
		var body apiv1.MkdirRequest
		if ok := decodeJSONBody(w, r, &body); !ok {
			return
		}
		recursive := true
		if body.Recursive != nil {
			recursive = *body.Recursive
		}
		mode, err := domain.ParseConflictMode(body.OnConflict, domain.MkdirConflictModes)
		if err != nil {
			statusFromErr(w, err)
			return
		}
		created, err := svc.MkdirRelative(id, body.Path, recursive, ownershipToDomain(body.Ownership), mode)
		if err != nil {
			if errors.Is(err, domain.ErrConflict) {
				existingID, existingPath := lookupExistingUnderParent(svc, id, body.Path)
				writeConflict(w, "path already exists", existingID, existingPath)
				return
			}
			statusFromErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, nodeResponse(created))
	})

	handleV1("PUT /v1/nodes/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseID(w, r.PathValue("id"))
		if !ok {
			return
		}
		meta, err := svc.GetFile(id)
		if err != nil {
			statusFromErr(w, err)
			return
		}

		if meta.Type != "file" {
			writeErr(w, http.StatusBadRequest, "content writes are only allowed on file nodes")
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, opts.MaxUploadBytes)
		if err := svc.WriteContent(id, r.Body); err != nil {
			statusFromErr(w, err)
			return
		}
		updated, err := svc.GetFile(id)
		if err != nil {
			statusFromErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, nodeResponse(updated))
	})

	handleV1("PATCH /v1/nodes/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseID(w, r.PathValue("id"))
		if !ok {
			return
		}
		recursiveOwnership := parseBoolDefault(r.URL.Query().Get("recursiveOwnership"), true)
		var body apiv1.UpdateNodeRequest
		if ok := decodeJSONBody(w, r, &body); !ok {
			return
		}
		if body.Name == nil && body.Ownership == nil {
			writeErr(w, http.StatusBadRequest, "name or ownership required")
			return
		}
		updated, err := svc.UpdateNode(id, body.Name, ownershipToDomain(body.Ownership), recursiveOwnership)
		if err != nil {
			statusFromErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, nodeResponse(updated))
	})

	handleV1("DELETE /v1/nodes/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseID(w, r.PathValue("id"))
		if !ok {
			return
		}
		if err := svc.Delete(id); err != nil {
			statusFromErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	registerVersionRoutes(handleV1, svc)

	handleV1("POST /v1/transfers", func(w http.ResponseWriter, r *http.Request) {
		recursiveOwnership := parseBoolDefault(r.URL.Query().Get("recursiveOwnership"), true)
		var body apiv1.TransferRequest
		if ok := decodeJSONBody(w, r, &body); !ok {
			return
		}
		sourceID, err := domain.ParseFileID(body.SourceID)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid sourceId")
			return
		}
		targetParentID, err := domain.ParseFileID(body.TargetParentID)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid targetParentId")
			return
		}
		mode, err := domain.ParseConflictMode(body.OnConflict, domain.FileConflictModes)
		if err != nil {
			statusFromErr(w, err)
			return
		}

		out, err := svc.Transfer(domain.TransferRequest{
			Op:                 body.Op,
			SourceID:           sourceID,
			TargetParentID:     targetParentID,
			TargetName:         body.TargetName,
			OnConflict:         mode,
			Ownership:          ownershipToDomain(body.Ownership),
			RecursiveOwnership: &recursiveOwnership,
		})
		if err != nil {
			if errors.Is(err, domain.ErrConflict) {
				existingID, existingPath := lookupExistingChildOfID(svc, targetParentID, body.TargetName)
				writeConflict(w, "target name already exists in parent", existingID, existingPath)
				return
			}
			statusFromErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, apiv1.TransferResponse{
			Node: nodeResponse(out),
			Op:   strings.ToLower(strings.TrimSpace(body.Op)),
		})
	})

	handleV1("GET /v1/search/glob", func(w http.ResponseWriter, r *http.Request) {
		pattern := strings.TrimSpace(r.URL.Query().Get("pattern"))
		if pattern == "" {
			writeErr(w, http.StatusBadRequest, "pattern required")
			return
		}
		if len(pattern) > 500 {
			writeErr(w, http.StatusBadRequest, "pattern too long")
			return
		}
		if strings.Count(pattern, "**") > 10 {
			writeErr(w, http.StatusBadRequest, "too many recursive wildcards")
			return
		}

		limit := parseIntDefault(r.URL.Query().Get("limit"), 100)
		showHidden := parseBoolDefault(r.URL.Query().Get("showHidden"), false)
		includeFiles := parseBoolDefault(r.URL.Query().Get("files"), true)
		includeDirs := parseBoolDefault(r.URL.Query().Get("directories"), false)
		paths := parseList(r.URL.Query().Get("paths"))

		out, err := svc.SearchGlob(domain.GlobSearchRequest{
			Pattern:      pattern,
			Paths:        paths,
			Limit:        limit,
			ShowHidden:   showHidden,
			IncludeFiles: includeFiles,
			IncludeDirs:  includeDirs,
		})
		if err != nil {
			statusFromErr(w, err)
			return
		}

		results := make([]apiv1.Node, 0, len(out.Results))
		for i := range out.Results {
			results = append(results, nodeResponse(&out.Results[i]))
		}
		errorsOut := make([]apiv1.GlobSearchError, 0, len(out.Errors))
		for i := range out.Errors {
			errorsOut = append(errorsOut, apiv1.GlobSearchError{
				Path:  out.Errors[i].Path,
				Cause: out.Errors[i].Cause,
			})
		}
		pathsOut := make([]apiv1.GlobSearchPath, 0, len(out.Paths))
		for i := range out.Paths {
			pathsOut = append(pathsOut, apiv1.GlobSearchPath{
				Path:     out.Paths[i].Path,
				Returned: out.Paths[i].Returned,
				HasMore:  out.Paths[i].HasMore,
			})
		}

		writeJSON(w, http.StatusOK, apiv1.GlobSearchResponse{
			Results: results,
			Errors:  errorsOut,
			Meta: apiv1.GlobSearchMeta{
				Pattern:     pattern,
				Limit:       limit,
				ResultCount: len(results),
				ErrorCount:  len(out.Errors),
			},
			Paths: pathsOut,
		})
	})

	handleV1("POST /v1/uploads/direct", directUploads.handleCreate)
	handleV1("POST /v1/downloads/direct", directDownloads.handleCreate)
	handleV1("POST /v1/uploads/sessions", uploadSessions.handleCreate)
	handleV1("POST /v1/uploads/sessions:batch", uploadSessions.handleCreateBatch)

	handleV1("POST /v1/index/rescan", func(w http.ResponseWriter, _ *http.Request) {
		if opts.Rescan == nil {
			writeErr(w, http.StatusNotImplemented, "rescan not configured")
			return
		}
		if err := opts.Rescan(); err != nil {
			writeErr(w, http.StatusInternalServerError, "rescan failed")
			return
		}
		writeJSON(w, http.StatusOK, apiv1.OKResponse{OK: true})
	})

	handleV1("POST /v1/index/resolve", func(w http.ResponseWriter, r *http.Request) {
		var body apiv1.IndexResolveRequest
		if ok := decodeJSONBody(w, r, &body); !ok {
			return
		}

		singlePath := strings.TrimSpace(body.Path)
		if singlePath != "" {
			item, err := resolveNodeByVirtualPath(svc, singlePath)
			if err != nil {
				statusFromErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, apiv1.IndexResolveSingleResponse{Item: item})
			return
		}

		if len(body.Paths) > 0 {
			items := make([]*apiv1.Node, 0, len(body.Paths))
			for _, raw := range body.Paths {
				item, err := resolveNodeByVirtualPath(svc, raw)
				if err != nil {
					statusFromErr(w, err)
					return
				}
				items = append(items, item)
			}
			writeJSON(w, http.StatusOK, apiv1.IndexResolveManyResponse{Items: items, Total: len(items)})
			return
		}

		singleID := strings.TrimSpace(body.ID)
		if singleID != "" {
			item, err := resolveNodeByID(svc, singleID)
			if err != nil {
				statusFromErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, apiv1.IndexResolveSingleResponse{Item: item})
			return
		}

		if len(body.IDs) == 0 {
			writeErr(w, http.StatusBadRequest, "path/paths or id/ids required")
			return
		}

		items := make([]*apiv1.Node, 0, len(body.IDs))
		for _, raw := range body.IDs {
			item, err := resolveNodeByID(svc, raw)
			if err != nil {
				statusFromErr(w, err)
				return
			}
			items = append(items, item)
		}
		writeJSON(w, http.StatusOK, apiv1.IndexResolveManyResponse{Items: items, Total: len(items)})
	})
	chain := []middlewareFunc{recoverMiddleware, requestIDMiddleware, activityMiddleware(opts.ActivityLog)}
	if realIP := realIPMiddleware(opts.TrustedProxies); realIP != nil {
		chain = append(chain, realIP)
	}
	chain = append(chain, secureHeadersMiddleware)
	if cors := corsMiddleware(opts.CORS); cors != nil {
		chain = append(chain, cors)
	}
	if opts.AccessLogEnabled {
		chain = append(chain, accessLogMiddleware)
	}
	handler := chainMiddleware(root, chain...)
	return &closeableHandler{
		handler: handler,
		closeFn: func() {
			uploadSessions.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := thumbnailScheduler.Close(ctx); err != nil {
				log.Printf("[filegate] thumbnail scheduler close: %v", err)
			}
		},
	}
}

func respondNodeWithOptionalChildren(w http.ResponseWriter, r *http.Request, svc *domain.Service, id domain.FileID) {
	meta, err := svc.GetFile(id)
	if err != nil {
		statusFromErr(w, err)
		return
	}
	respondMetaWithOptionalChildren(w, r, svc, meta)
}

func respondMetaWithOptionalChildren(w http.ResponseWriter, r *http.Request, svc *domain.Service, meta *domain.FileMeta) {
	computeRecursiveSizes := strings.EqualFold(r.URL.Query().Get("computeRecursiveSizes"), "true")
	fingerprint, err := parseFingerprintMode(r.URL.Query().Get("fingerprint"))
	if err != nil {
		statusFromErr(w, err)
		return
	}
	meta, err = metaForFingerprint(svc, meta, fingerprint)
	if err != nil {
		statusFromErr(w, err)
		return
	}
	if meta.Type == "directory" && computeRecursiveSizes {
		copyMeta := *meta
		if size, ok := svc.RecursiveDirectorySize(meta.ID); ok {
			copyMeta.Size = size
		}
		meta = &copyMeta
	}
	response := nodeResponseForFingerprint(meta, fingerprint)
	if meta.Type == "directory" {
		pageSize := parseIntDefault(r.URL.Query().Get("pageSize"), 100)
		cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
		listed, err := svc.ListNodeChildren(meta.ID, cursor, pageSize, computeRecursiveSizes)
		if err != nil {
			statusFromErr(w, err)
			return
		}
		items := make([]apiv1.Node, 0, len(listed.Items))
		for i := range listed.Items {
			items = append(items, nodeResponseForFingerprint(&listed.Items[i], fingerprint))
		}
		response.Children = items
		response.PageSize = &pageSize
		if listed.NextCursor != "" {
			response.NextCursor = listed.NextCursor
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func parseIntDefault(v string, def int) int {
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

type fingerprintMode string

const (
	fingerprintCached fingerprintMode = "cached"
	fingerprintNone   fingerprintMode = "none"
	fingerprintEnsure fingerprintMode = "ensure"
)

func parseFingerprintMode(raw string) (fingerprintMode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "cached":
		return fingerprintCached, nil
	case "none":
		return fingerprintNone, nil
	case "ensure":
		return fingerprintEnsure, nil
	default:
		return "", domain.ErrInvalidArgument
	}
}

func metaForFingerprint(svc *domain.Service, meta *domain.FileMeta, mode fingerprintMode) (*domain.FileMeta, error) {
	if mode != fingerprintEnsure || meta == nil || meta.Type != "file" || meta.SHA256 != "" {
		return meta, nil
	}
	return svc.EnsureFileSHA256(meta.ID)
}

func streamNodeContent(w http.ResponseWriter, r *http.Request, svc *domain.Service, id domain.FileID, inline bool) {
	meta, err := svc.GetFile(id)
	if err != nil {
		statusFromErr(w, err)
		return
	}

	if meta.Type == "directory" {
		abs, err := svc.ResolveAbsPath(id)
		if err != nil {
			statusFromErr(w, err)
			return
		}
		if err := preflightDirectoryTar(abs); err != nil {
			if errors.Is(err, os.ErrPermission) {
				writeErr(w, http.StatusForbidden, "directory content is not readable")
				return
			}
			var budgetErr directoryTarBudgetError
			if errors.As(err, &budgetErr) {
				writeErr(w, http.StatusRequestEntityTooLarge, err.Error())
				return
			}
			writeErr(w, http.StatusInternalServerError, "failed to prepare directory download")
			return
		}
		w.Header().Set("Content-Type", "application/x-tar")
		setContentDisposition(w, "attachment", meta.Name+".tar")
		if r.Method == http.MethodHead {
			return
		}
		if err := writeDirectoryTar(w, abs, meta.Name); err != nil {
			log.Printf("[filegate] tar stream failed for node=%s path=%s: %v", id.String(), abs, err)
		}
		return
	}

	reader, size, _, err := svc.OpenContent(id)
	if err != nil {
		statusFromErr(w, err)
		return
	}
	defer reader.Close()

	contentType := mime.TypeByExtension(filepath.Ext(meta.Name))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Accept-Ranges", "bytes")
	if inline {
		setContentDisposition(w, "inline", meta.Name)
	} else {
		setContentDisposition(w, "attachment", meta.Name)
	}

	if seeker, ok := reader.(io.ReadSeeker); ok {
		http.ServeContent(w, r, meta.Name, time.UnixMilli(meta.Mtime), seeker)
		return
	}
	if r.Method == http.MethodHead {
		return
	}
	bufPtr := copyBufPool.Get().(*[]byte)
	buf := *bufPtr
	_, _ = io.CopyBuffer(w, reader, buf)
	copyBufPool.Put(bufPtr)
}

func parseBoolDefault(v string, def bool) bool {
	if strings.TrimSpace(v) == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func parseList(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.FieldsFunc(v, func(r rune) bool {
		return r == ',' || r == ';'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

const maxJSONBodyBytes int64 = 1 << 20

func decodeJSONBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json body")
		return false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeErr(w, http.StatusBadRequest, "invalid json body")
		return false
	}
	return true
}

func resolveNodeByID(svc *domain.Service, rawID string) (*apiv1.Node, error) {
	id, err := domain.ParseFileID(strings.TrimSpace(rawID))
	if err != nil {
		return nil, domain.ErrInvalidArgument
	}
	meta, err := svc.GetFile(id)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	node := nodeResponse(meta)
	return &node, nil
}

func resolveNodeByVirtualPath(svc *domain.Service, rawPath string) (*apiv1.Node, error) {
	id, err := svc.ResolvePath(rawPath)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	meta, err := svc.GetFile(id)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	node := nodeResponse(meta)
	return &node, nil
}

func dirSizeBytes(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func collectSystemStats() apiv1.StatsSystem {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	return apiv1.StatsSystem{
		Goroutines:     runtime.NumGoroutine(),
		HeapAllocBytes: mem.HeapAlloc,
		HeapSysBytes:   mem.HeapSys,
		HeapObjects:    mem.HeapObjects,
		NumGC:          mem.NumGC,
		LastGCPauseNs:  lastGCPause(mem),
		OpenFDs:        countOpenFDs(),
		MaxFDs:         maxOpenFDs(),
	}
}

func lastGCPause(mem runtime.MemStats) uint64 {
	if mem.NumGC == 0 {
		return 0
	}
	return mem.PauseNs[(mem.NumGC+255)%256]
}

func countOpenFDs() int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0
	}
	return len(entries)
}

func maxOpenFDs() uint64 {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return 0
	}
	return lim.Cur
}

type mountInfo struct {
	devID      string
	mountPoint string
	fsType     string
	source     string
}

func collectDiskUsage(mounts []domain.StatsMount, svc *domain.Service) []apiv1.StatsDisk {
	infos := readMountInfoCached(30 * time.Second)
	type diskGroup struct {
		DiskName string
		FSType   string
		Used     uint64
		Size     uint64
		Roots    []string
	}
	groups := make(map[string]*diskGroup)

	for _, mount := range mounts {
		abs, err := svc.ResolveAbsPath(mount.ID)
		if err != nil {
			continue
		}

		var st syscall.Statfs_t
		if err := syscall.Statfs(abs, &st); err != nil {
			continue
		}
		size := uint64(st.Blocks) * uint64(st.Bsize)
		used := uint64(st.Blocks-st.Bfree) * uint64(st.Bsize)

		key := abs
		diskName := "unknown"
		fsType := ""
		if info, ok := bestMountInfo(abs, infos); ok {
			if strings.TrimSpace(info.devID) != "" {
				key = info.devID
			}
			if strings.TrimSpace(info.source) != "" {
				diskName = info.source
			}
			fsType = info.fsType
		}

		group, exists := groups[key]
		if !exists {
			group = &diskGroup{
				DiskName: diskName,
				FSType:   fsType,
				Used:     used,
				Size:     size,
				Roots:    []string{},
			}
			groups[key] = group
		}

		if !containsString(group.Roots, mount.Path) {
			group.Roots = append(group.Roots, mount.Path)
		}
	}

	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]apiv1.StatsDisk, 0, len(keys))
	for _, key := range keys {
		group := groups[key]
		sort.Strings(group.Roots)
		out = append(out, apiv1.StatsDisk{
			DiskName: group.DiskName,
			FSType:   group.FSType,
			Used:     group.Used,
			Size:     group.Size,
			Roots:    group.Roots,
		})
	}
	return out
}

func containsString(values []string, needle string) bool {
	for _, v := range values {
		if v == needle {
			return true
		}
	}
	return false
}

func bestMountInfo(absPath string, infos []mountInfo) (mountInfo, bool) {
	bestLen := -1
	var best mountInfo
	for _, info := range infos {
		mp := strings.TrimSpace(info.mountPoint)
		if mp == "" {
			continue
		}
		if !pathHasPrefix(absPath, mp) {
			continue
		}
		if len(mp) > bestLen {
			bestLen = len(mp)
			best = info
		}
	}
	if bestLen < 0 {
		return mountInfo{}, false
	}
	return best, true
}

func pathHasPrefix(path, prefix string) bool {
	if path == prefix {
		return true
	}
	if prefix == "/" {
		return strings.HasPrefix(path, "/")
	}
	return strings.HasPrefix(path, prefix+"/")
}

func readMountInfo() []mountInfo {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	out := make([]mountInfo, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " - ", 2)
		if len(parts) != 2 {
			continue
		}
		left := strings.Fields(parts[0])
		right := strings.Fields(parts[1])
		if len(left) < 5 || len(right) < 2 {
			continue
		}
		out = append(out, mountInfo{
			devID:      strings.TrimSpace(left[2]),
			mountPoint: decodeMountInfoField(left[4]),
			fsType:     strings.TrimSpace(right[0]),
			source:     strings.TrimSpace(right[1]),
		})
	}
	return out
}

func readMountInfoCached(ttl time.Duration) []mountInfo {
	now := time.Now()
	mountInfoCache.mu.Lock()
	defer mountInfoCache.mu.Unlock()
	if ttl > 0 && len(mountInfoCache.entries) > 0 && now.Sub(mountInfoCache.loaded) < ttl {
		cached := make([]mountInfo, len(mountInfoCache.entries))
		copy(cached, mountInfoCache.entries)
		return cached
	}
	fresh := readMountInfo()
	mountInfoCache.loaded = now
	mountInfoCache.entries = fresh
	cached := make([]mountInfo, len(fresh))
	copy(cached, fresh)
	return cached
}

func decodeMountInfoField(v string) string {
	if !strings.Contains(v, `\`) {
		return v
	}
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] == '\\' && i+3 < len(v) {
			oct := v[i+1 : i+4]
			if oct[0] >= '0' && oct[0] <= '7' &&
				oct[1] >= '0' && oct[1] <= '7' &&
				oct[2] >= '0' && oct[2] <= '7' {
				n, err := strconv.ParseUint(oct, 8, 8)
				if err == nil {
					b.WriteByte(byte(n))
					i += 3
					continue
				}
			}
		}
		b.WriteByte(v[i])
	}
	return b.String()
}

func writeDirectoryTar(w io.Writer, dirPath, rootName string) error {
	tw := tar.NewWriter(w)
	defer tw.Close()

	return filepath.WalkDir(dirPath, func(current string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dirPath, current)
		if err != nil {
			return err
		}
		if rel == "." {
			rel = ""
		}
		name := filepath.ToSlash(filepath.Join(rootName, rel))
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = name
		if info.IsDir() && !strings.HasSuffix(hdr.Name, "/") {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		linfo, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if linfo.Mode()&os.ModeSymlink != 0 || !linfo.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(current)
		if err != nil {
			return err
		}
		bufPtr := copyBufPool.Get().(*[]byte)
		buf := *bufPtr
		_, copyErr := io.CopyBuffer(tw, f, buf)
		copyBufPool.Put(bufPtr)
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}

type directoryTarBudget struct {
	MaxEntries int
	MaxBytes   int64
	MaxDepth   int
}

type directoryTarBudgetError struct {
	message string
}

func (e directoryTarBudgetError) Error() string {
	return e.message
}

var defaultDirectoryTarBudget = directoryTarBudget{
	MaxEntries: 100_000,
	MaxBytes:   10 << 30,
	MaxDepth:   128,
}

func preflightDirectoryTar(dirPath string) error {
	return preflightDirectoryTarWithBudget(dirPath, defaultDirectoryTarBudget)
}

func preflightDirectoryTarWithBudget(dirPath string, budget directoryTarBudget) error {
	var entries int
	var bytes int64
	return filepath.WalkDir(dirPath, func(current string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return nil
		}

		rel, err := filepath.Rel(dirPath, current)
		if err != nil {
			return err
		}
		depth := 0
		if rel != "." {
			depth = strings.Count(filepath.ToSlash(rel), "/") + 1
		}
		if budget.MaxDepth > 0 && depth > budget.MaxDepth {
			return directoryTarBudgetError{message: fmt.Sprintf("directory tar exceeds max depth %d", budget.MaxDepth)}
		}

		entries++
		if budget.MaxEntries > 0 && entries > budget.MaxEntries {
			return directoryTarBudgetError{message: fmt.Sprintf("directory tar exceeds max entries %d", budget.MaxEntries)}
		}

		if info.Mode().IsRegular() {
			bytes += info.Size()
			if budget.MaxBytes > 0 && bytes > budget.MaxBytes {
				return directoryTarBudgetError{message: fmt.Sprintf("directory tar exceeds max content bytes %d", budget.MaxBytes)}
			}
		}
		if info.IsDir() {
			return nil
		}
		f, err := os.Open(current)
		if err != nil {
			return err
		}
		return f.Close()
	})
}

func resolveThumbnailJobWorkers(opts RouterOptions) int {
	if opts.ThumbnailJobWorkers > 0 {
		return opts.ThumbnailJobWorkers
	}
	if opts.JobWorkers > 0 {
		if opts.JobWorkers < 16 {
			return 16
		}
		return opts.JobWorkers
	}
	return 32
}

func resolveThumbnailQueueSize(opts RouterOptions) int {
	if opts.ThumbnailJobQueueSize > 0 {
		return opts.ThumbnailJobQueueSize
	}
	if opts.JobQueueSize > 0 {
		n := opts.JobQueueSize * 2
		if n < 8192 {
			return 8192
		}
		if n > 65536 {
			return 65536
		}
		return n
	}
	return 16384
}

func chainMiddleware(h http.Handler, middlewares ...middlewareFunc) http.Handler {
	out := h
	for i := len(middlewares) - 1; i >= 0; i-- {
		out = middlewares[i](out)
	}
	return out
}

func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[filegate] panic: %v", rec)
				writeErr(w, http.StatusInternalServerError, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var id string
		if v := strings.TrimSpace(r.Header.Get("X-Request-Id")); v != "" {
			id = v
		} else {
			var b [8]byte
			n := atomic.AddUint64(&reqIDCounter, 1)
			binary.BigEndian.PutUint64(b[:], n^uint64(time.Now().UnixNano()))
			id = hex.EncodeToString(b[:])
		}
		w.Header().Set("X-Request-Id", id)
		ctx := context.WithValue(r.Context(), requestIDKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func requestID(r *http.Request) string {
	if r == nil {
		return ""
	}
	if id, ok := r.Context().Value(requestIDKey{}).(string); ok {
		return id
	}
	return ""
}

func setActivityActor(r *http.Request, actor activity.Actor) {
	if r == nil {
		return
	}
	holder, _ := r.Context().Value(activityActorKey{}).(*activityActorHolder)
	if holder == nil {
		return
	}
	holder.actor = actor
}

func activityMiddleware(log *activity.Ring) middlewareFunc {
	return func(next http.Handler) http.Handler {
		if log == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			holder := &activityActorHolder{}
			ctx := context.WithValue(r.Context(), activityActorKey{}, holder)
			req := r.WithContext(ctx)
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(sw, req)

			spec, ok := activitySpecForRequest(req)
			if !ok {
				return
			}
			actor := holder.actor
			if actor.Kind == "" {
				actor = signedURLActor(req)
			}
			if actor.Kind == "" {
				return
			}
			log.Record(activity.Event{
				Actor:      actor,
				Operation:  spec.operation,
				Outcome:    outcomeForStatus(sw.status),
				Target:     spec.target,
				DurationMS: time.Since(start).Milliseconds(),
				RequestID:  requestID(req),
				Error:      errorForStatus(sw.status),
			})
		})
	}
}

type activityRequestSpec struct {
	operation string
	target    *activity.Target
}

func activitySpecForRequest(r *http.Request) (activityRequestSpec, bool) {
	path := strings.TrimRight(r.URL.Path, "/")
	if path == "" {
		path = "/"
	}
	if path == "/v1/activity" || !strings.HasPrefix(path, "/v1/") {
		return activityRequestSpec{}, false
	}
	if strings.Contains(path, "/segments/") {
		return activityRequestSpec{}, false
	}
	switch {
	case strings.HasPrefix(path, "/v1/downloads/direct/") && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		return activityRequestSpec{operation: "direct_download.get", target: &activity.Target{Kind: "signed_url"}}, true
	case strings.HasPrefix(path, "/v1/uploads/direct/") && r.Method == http.MethodPut:
		return activityRequestSpec{operation: "direct_upload.put", target: &activity.Target{Kind: "signed_url"}}, true
	case strings.HasSuffix(path, "/commit") && strings.HasPrefix(path, "/v1/uploads/sessions/") && r.Method == http.MethodPost:
		return activityRequestSpec{operation: "upload_session.commit", target: &activity.Target{Kind: "upload_session", ID: uploadSessionIDFromPath(path)}}, true
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return activityRequestSpec{}, false
	}
	return activityRequestSpec{operation: restOperationName(r.Method, path), target: restTarget(path)}, true
}

func restOperationName(method, path string) string {
	switch {
	case method == http.MethodPost && path == "/v1/index/rescan":
		return "index.rescan"
	case method == http.MethodPost && path == "/v1/uploads/direct":
		return "direct_upload.create_url"
	case method == http.MethodPost && path == "/v1/downloads/direct":
		return "direct_download.create_url"
	case method == http.MethodPost && path == "/v1/uploads/sessions":
		return "upload_session.create"
	case method == http.MethodPost && path == "/v1/uploads/sessions:batch":
		return "upload_session.create_batch"
	case method == http.MethodPost && path == "/v1/transfers":
		return "transfer.create"
	case strings.HasPrefix(path, "/v1/nodes/") && strings.HasSuffix(path, "/mkdir"):
		return "node.mkdir"
	case strings.HasPrefix(path, "/v1/nodes/") && method == http.MethodPatch:
		return "node.update"
	case strings.HasPrefix(path, "/v1/nodes/") && method == http.MethodDelete:
		return "node.delete"
	case strings.HasPrefix(path, "/v1/paths/") && method == http.MethodPut:
		return "path.put"
	default:
		return strings.ToLower(method) + " " + path
	}
}

func restTarget(path string) *activity.Target {
	switch {
	case strings.HasPrefix(path, "/v1/nodes/"):
		parts := strings.Split(strings.TrimPrefix(path, "/v1/nodes/"), "/")
		if parts[0] != "" {
			return &activity.Target{Kind: "node", ID: parts[0]}
		}
	case strings.HasPrefix(path, "/v1/paths/"):
		return &activity.Target{Kind: "path", Path: strings.TrimPrefix(path, "/v1/paths/")}
	case path == "/v1/index/rescan":
		return &activity.Target{Kind: "index"}
	}
	return nil
}

func uploadSessionIDFromPath(path string) string {
	rest := strings.TrimPrefix(path, "/v1/uploads/sessions/")
	id, _, _ := strings.Cut(rest, "/")
	return id
}

func signedURLActor(r *http.Request) activity.Actor {
	token := ""
	switch {
	case strings.HasPrefix(r.URL.Path, "/v1/uploads/direct/"), strings.HasPrefix(r.URL.Path, "/v1/downloads/direct/"):
		token = path.Base(r.URL.Path)
	case strings.HasPrefix(r.URL.Path, "/v1/uploads/sessions/"):
		token = strings.TrimSpace(r.Header.Get("Filegate-Upload-Session"))
		if token == "" {
			token = strings.TrimSpace(r.URL.Query().Get("upload_token"))
		}
	}
	if token == "" {
		return activity.Actor{}
	}
	return activity.Actor{
		Kind:           activity.ActorSignedURL,
		ID:             activity.CredentialID("signed", token),
		DelegatedActor: activity.CleanActorLabel(r.Header.Get("X-Filegate-Actor")),
	}
}

func outcomeForStatus(status int) activity.Outcome {
	if status >= 200 && status < 400 {
		return activity.OutcomeSucceeded
	}
	return activity.OutcomeFailed
}

func errorForStatus(status int) string {
	if status >= 400 {
		return http.StatusText(status)
	}
	return ""
}

type activityListQuery struct {
	limit     int
	offset    int
	q         string
	operation string
	outcome   string
}

func activityListResponse(log *activity.Ring, query activityListQuery) apiv1.ActivityListResponse {
	events := log.Snapshot(0)
	operations := activityOperations(events)
	filtered := make([]activity.Event, 0, len(events))
	for _, event := range events {
		if activityMatches(event, query) {
			filtered = append(filtered, event)
		}
	}

	total := len(filtered)
	limit := query.limit
	if limit <= 0 || limit > total {
		limit = total
	}
	start := query.offset
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}

	page := filtered[start:end]
	items := make([]apiv1.ActivityEvent, 0, len(page))
	for _, event := range page {
		items = append(items, activityEventResponse(event))
	}
	return apiv1.ActivityListResponse{
		Items:      items,
		Total:      total,
		Offset:     start,
		Limit:      limit,
		Retained:   log.Len(),
		Capacity:   log.Capacity(),
		Operations: operations,
	}
}

func activityEventResponse(event activity.Event) apiv1.ActivityEvent {
	return apiv1.ActivityEvent{
		ID: event.ID,
		At: event.At.UnixMilli(),
		Actor: apiv1.ActivityActor{
			Kind:           string(event.Actor.Kind),
			ID:             event.Actor.ID,
			Label:          event.Actor.Label,
			DelegatedActor: event.Actor.DelegatedActor,
		},
		Operation:  event.Operation,
		Outcome:    string(event.Outcome),
		Target:     activityTargetResponse(event.Target),
		DurationMS: event.DurationMS,
		RequestID:  event.RequestID,
		Error:      event.Error,
		Meta:       event.Meta,
	}
}

func activityOperations(events []activity.Event) []string {
	seen := make(map[string]struct{})
	for _, event := range events {
		if event.Operation != "" {
			seen[event.Operation] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for op := range seen {
		out = append(out, op)
	}
	sort.Strings(out)
	return out
}

func activityMatches(event activity.Event, query activityListQuery) bool {
	if query.operation != "" && event.Operation != query.operation {
		return false
	}
	if query.outcome != "" && string(event.Outcome) != query.outcome {
		return false
	}
	q := strings.ToLower(strings.TrimSpace(query.q))
	if q == "" {
		return true
	}
	return strings.Contains(strings.ToLower(strings.Join(activitySearchFields(event), "\x00")), q)
}

func activitySearchFields(event activity.Event) []string {
	fields := []string{
		event.ID,
		event.Operation,
		string(event.Outcome),
		event.Actor.ID,
		string(event.Actor.Kind),
		event.Actor.Label,
		event.Actor.DelegatedActor,
		event.RequestID,
		event.Error,
	}
	if event.Target != nil {
		fields = append(fields, event.Target.Kind, event.Target.ID, event.Target.Path)
	}
	return fields
}

func activityTargetResponse(target *activity.Target) *apiv1.ActivityTarget {
	if target == nil {
		return nil
	}
	return &apiv1.ActivityTarget{Kind: target.Kind, ID: target.ID, Path: target.Path}
}

// ParseTrustedProxies converts the configured server.trusted_proxies
// entries (bare IPs or CIDRs) into prefixes. Returns an error on the
// first malformed entry so startup fails loudly instead of silently
// trusting nobody.
func ParseTrustedProxies(entries []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(entries))
	for _, raw := range entries {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if strings.Contains(raw, "/") {
			p, err := netip.ParsePrefix(raw)
			if err != nil {
				return nil, fmt.Errorf("server.trusted_proxies entry %q is not a valid CIDR: %w", raw, err)
			}
			out = append(out, p.Masked())
			continue
		}
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			return nil, fmt.Errorf("server.trusted_proxies entry %q is not a valid IP or CIDR: %w", raw, err)
		}
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out, nil
}

// realIPMiddleware rewrites r.RemoteAddr from X-Forwarded-For /
// X-Real-Ip, but ONLY when the direct peer is a configured trusted
// proxy — any direct client could otherwise spoof its logged address.
// Returns nil (middleware skipped entirely) when no proxies are
// trusted, which is the default.
func realIPMiddleware(trusted []netip.Prefix) middlewareFunc {
	if len(trusted) == 0 {
		return nil
	}
	inTrusted := func(a netip.Addr) bool {
		a = a.Unmap()
		for _, p := range trusted {
			if p.Contains(a) {
				return true
			}
		}
		return false
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			peer, err := peerAddr(r.RemoteAddr)
			if err == nil && inTrusted(peer) {
				if client, ok := clientFromForwardHeaders(r, inTrusted); ok {
					r.RemoteAddr = client.String()
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// clientFromForwardHeaders extracts the real client address from the
// proxy headers. X-Forwarded-For is walked right to left: the rightmost
// entries were appended by our own trusted proxy chain, anything the
// client pre-filled sits further left — the first hop that is not
// itself a trusted proxy is the client. A malformed entry distrusts the
// whole chain. X-Real-Ip is the fallback when no X-Forwarded-For is
// present.
func clientFromForwardHeaders(r *http.Request, inTrusted func(netip.Addr) bool) (netip.Addr, bool) {
	if xff := strings.TrimSpace(strings.Join(r.Header.Values("X-Forwarded-For"), ",")); xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			hop := strings.TrimSpace(parts[i])
			if hop == "" {
				continue
			}
			addr, err := netip.ParseAddr(hop)
			if err != nil {
				return netip.Addr{}, false
			}
			if inTrusted(addr) {
				continue
			}
			return addr.Unmap(), true
		}
		// Every hop is a trusted proxy — proxy-internal traffic; keep
		// the peer address.
		return netip.Addr{}, false
	}
	if xrip := strings.TrimSpace(r.Header.Get("X-Real-Ip")); xrip != "" {
		if addr, err := netip.ParseAddr(xrip); err == nil {
			return addr.Unmap(), true
		}
	}
	return netip.Addr{}, false
}

// peerAddr parses the connection peer out of r.RemoteAddr, tolerating
// the port-less form some tests use.
func peerAddr(remoteAddr string) (netip.Addr, error) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, err
	}
	return addr.Unmap(), nil
}

func peerTrusted(remoteAddr string, trusted []netip.Prefix) bool {
	if len(trusted) == 0 {
		return false
	}
	peer, err := peerAddr(remoteAddr)
	if err != nil {
		return false
	}
	for _, p := range trusted {
		if p.Contains(peer) {
			return true
		}
	}
	return false
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func accessLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		log.Printf("[filegate] method=%s path=%s status=%d dur=%s remote=%s",
			r.Method, r.URL.Path, sw.status, time.Since(start), r.RemoteAddr)
	})
}

// ValidateMetricsPath rejects a metrics path that would collide with
// the REST route surface. Called at startup so a misconfiguration
// fails loudly instead of producing a confusing ServeMux conflict or
// shadowing a real route. The path must be absolute and must not be
// "/health" or live under "/v1".
func ValidateMetricsPath(path string) error {
	p := strings.TrimSpace(path)
	if p == "" {
		return errors.New("metrics.path must not be empty")
	}
	if !strings.HasPrefix(p, "/") {
		return errors.New("metrics.path must start with /")
	}
	if p == "/health" {
		return errors.New("metrics.path must not be /health (reserved)")
	}
	if p == "/v1" || strings.HasPrefix(p, "/v1/") {
		return errors.New("metrics.path must not live under /v1 (reserved for the REST API)")
	}
	return nil
}

// PathsOverlap reports whether either path is equal to or mounted under
// the other. It is used for startup-time route collision checks.
func PathsOverlap(a, b string) bool {
	a = strings.TrimRight(strings.TrimSpace(a), "/")
	b = strings.TrimRight(strings.TrimSpace(b), "/")
	if a == "" || b == "" {
		return false
	}
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// metricsAuthMiddleware guards the /metrics endpoint with the layered
// token rule: require metricsToken if set, else require bearerToken,
// else serve openly. The "open" case is intentional — operators on a
// trusted internal network where the Prometheus scraper holds no
// filegate credentials can leave both empty and rely on network
// isolation. Comparison is constant-time to avoid leaking the token
// via timing.
func metricsAuthMiddleware(metricsToken, bearerToken string) func(http.Handler) http.Handler {
	effective := strings.TrimSpace(metricsToken)
	if effective == "" {
		effective = strings.TrimSpace(bearerToken)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if effective == "" {
				next.ServeHTTP(w, r) // open — no credential configured
				return
			}
			auth := strings.TrimSpace(r.Header.Get("Authorization"))
			if !strings.HasPrefix(auth, "Bearer ") {
				writeErr(w, http.StatusUnauthorized, "missing bearer token")
				return
			}
			provided := strings.TrimPrefix(auth, "Bearer ")
			if subtle.ConstantTimeCompare([]byte(provided), []byte(effective)) != 1 {
				writeErr(w, http.StatusUnauthorized, "invalid bearer token")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// recordAuthFailure logs a rejected request to the activity ring.
//
// The activity middleware only records requests whose actor could be
// determined, so authentication failures previously left no trace at all --
// exactly the events an operator investigating an intrusion wants to see. The
// actor is "system" because there is, by definition, no authenticated identity.
func recordAuthFailure(ring *activity.Ring, r *http.Request, reason string) {
	if ring == nil {
		return
	}
	ring.Record(activity.Event{
		Actor:     activity.Actor{Kind: activity.ActorSystem, ID: "anonymous"},
		Operation: "auth.denied",
		Outcome:   activity.OutcomeFailed,
		Target:    &activity.Target{Kind: "path", Path: r.URL.Path},
		RequestID: requestID(r),
		Error:     reason,
	})
}

func authMiddleware(token string, ring *activity.Ring) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := strings.TrimSpace(r.Header.Get("Authorization"))
			if token == "" {
				recordAuthFailure(ring, r, "bearer token not configured")
				writeErr(w, http.StatusUnauthorized, "bearer token not configured")
				return
			}
			if !strings.HasPrefix(auth, "Bearer ") {
				recordAuthFailure(ring, r, "missing bearer token")
				writeErr(w, http.StatusUnauthorized, "missing bearer token")
				return
			}
			provided := strings.TrimPrefix(auth, "Bearer ")
			if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
				recordAuthFailure(ring, r, "invalid bearer token")
				writeErr(w, http.StatusUnauthorized, "invalid bearer token")
				return
			}
			setActivityActor(r, activity.Actor{
				Kind:           activity.ActorBearer,
				ID:             activity.CredentialID("bearer", token),
				DelegatedActor: activity.CleanActorLabel(r.Header.Get("X-Filegate-Actor")),
			})
			next.ServeHTTP(w, r)
		})
	}
}

func setContentDisposition(w http.ResponseWriter, disposition, filename string) {
	clean := strings.NewReplacer("\r", "_", "\n", "_").Replace(filename)
	if clean == "" {
		clean = "download"
	}
	if v := mime.FormatMediaType(disposition, map[string]string{"filename": clean}); v != "" {
		w.Header().Set("Content-Disposition", v)
		return
	}
	w.Header().Set("Content-Disposition", disposition)
}

func secureHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func corsMiddleware(cfg domain.CORSConfig) middlewareFunc {
	allowedOrigins := cleanList(cfg.AllowedOrigins)
	if len(allowedOrigins) == 0 {
		return nil
	}
	originSet := make(map[string]struct{}, len(allowedOrigins))
	allowWildcard := false
	for _, origin := range allowedOrigins {
		if origin == "*" {
			allowWildcard = true
			continue
		}
		originSet[origin] = struct{}{}
	}
	allowedMethods := cleanList(cfg.AllowedMethods)
	if len(allowedMethods) == 0 {
		allowedMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"}
	}
	allowedHeaders := cleanList(cfg.AllowedHeaders)
	if len(allowedHeaders) == 0 {
		allowedHeaders = []string{"Authorization", "Content-Type", "Filegate-Upload-Session", "X-Segment-Checksum"}
	}
	exposedHeaders := cleanList(cfg.ExposedHeaders)
	maxAge := ""
	if cfg.MaxAge > 0 {
		maxAge = strconv.FormatInt(int64(cfg.MaxAge/time.Second), 10)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := strings.TrimSpace(r.Header.Get("Origin"))
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}
			allowed := false
			responseOrigin := origin
			if allowWildcard {
				allowed = true
				if !cfg.AllowCredentials {
					responseOrigin = "*"
				}
			} else if _, ok := originSet[origin]; ok {
				allowed = true
			}
			if !allowed {
				next.ServeHTTP(w, r)
				return
			}

			h := w.Header()
			h.Set("Access-Control-Allow-Origin", responseOrigin)
			h.Set("Vary", appendVary(h.Get("Vary"), "Origin"))
			if cfg.AllowCredentials {
				h.Set("Access-Control-Allow-Credentials", "true")
			}
			if len(exposedHeaders) > 0 {
				h.Set("Access-Control-Expose-Headers", strings.Join(exposedHeaders, ", "))
			}

			if r.Method == http.MethodOptions {
				h.Set("Access-Control-Allow-Methods", strings.Join(allowedMethods, ", "))
				h.Set("Access-Control-Allow-Headers", strings.Join(allowedHeaders, ", "))
				if maxAge != "" {
					h.Set("Access-Control-Max-Age", maxAge)
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func cleanList(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func appendVary(existing, value string) string {
	if strings.TrimSpace(existing) == "" {
		return value
	}
	for _, part := range strings.Split(existing, ",") {
		if strings.EqualFold(strings.TrimSpace(part), value) {
			return existing
		}
	}
	return existing + ", " + value
}

func parseID(w http.ResponseWriter, v string) (domain.FileID, bool) {
	id, err := domain.ParseFileID(v)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid id")
		return domain.FileID{}, false
	}
	return id, true
}

func ownershipToDomain(in *apiv1.Ownership) *domain.Ownership {
	if in == nil {
		return nil
	}
	return &domain.Ownership{
		UID:     in.UID,
		GID:     in.GID,
		Mode:    in.Mode,
		DirMode: in.DirMode,
	}
}

func nodeResponse(meta *domain.FileMeta) apiv1.Node {
	resp := apiv1.Node{
		ID:    meta.ID.String(),
		Type:  meta.Type,
		Name:  meta.Name,
		Path:  meta.Path,
		Size:  meta.Size,
		Mtime: meta.Mtime,
		Ownership: apiv1.OwnershipView{
			UID:  meta.UID,
			GID:  meta.GID,
			Mode: strconv.FormatUint(uint64(meta.Mode), 8),
		},
		Exif: map[string]string{},
	}
	if meta.ETag != "" {
		resp.ETag = meta.ETag
	}
	if meta.SHA256 != "" {
		resp.SHA256 = meta.SHA256
	}
	if meta.MimeType != "" {
		resp.MimeType = meta.MimeType
	}
	if len(meta.Exif) > 0 {
		resp.Exif = meta.Exif
	}
	return resp
}

func nodeResponseForFingerprint(meta *domain.FileMeta, mode fingerprintMode) apiv1.Node {
	resp := nodeResponse(meta)
	if mode == fingerprintNone {
		resp.ETag = ""
		resp.SHA256 = ""
	}
	return resp
}

func statusFromErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not found")
	case errors.Is(err, domain.ErrConflict):
		writeErr(w, http.StatusConflict, "conflict")
	case errors.Is(err, domain.ErrForbidden):
		writeErr(w, http.StatusForbidden, "forbidden")
	case errors.Is(err, domain.ErrInvalidArgument):
		writeErr(w, http.StatusBadRequest, "invalid argument")
	case errors.Is(err, domain.ErrInsufficientStorage), errors.Is(err, syscall.ENOSPC):
		writeErr(w, http.StatusInsufficientStorage, "insufficient storage")
	default:
		log.Printf("[filegate] internal error: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal server error")
	}
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, apiv1.ErrorResponse{Error: msg})
}

// writeConflict produces a 409 with diagnostic fields the client can use to
// render a "what should we do?" prompt without an extra resolve round trip.
// existingPath/existingID may be empty if not known.
func writeConflict(w http.ResponseWriter, msg, existingID, existingPath string) {
	writeJSON(w, http.StatusConflict, apiv1.ErrorResponse{
		Error:        msg,
		ExistingID:   existingID,
		ExistingPath: existingPath,
	})
}

// lookupExistingByPath best-effort resolves a virtual path back to an
// existing node so a 409 response can include diagnostic fields. Errors are
// silenced — the conflict response is still useful without these fields.
func lookupExistingByPath(svc *domain.Service, virtualPath string) (id, path string) {
	resolvedID, err := svc.ResolvePath(virtualPath)
	if err != nil {
		return "", ""
	}
	meta, err := svc.GetFile(resolvedID)
	if err != nil {
		return resolvedID.String(), ""
	}
	return meta.ID.String(), meta.Path
}

// lookupExistingUnderParent best-effort resolves parent+relPath to a
// node for the diagnostic fields on a mkdir conflict.
func lookupExistingUnderParent(svc *domain.Service, parentID domain.FileID, relPath string) (id, path string) {
	parent, err := svc.GetFile(parentID)
	if err != nil {
		return "", ""
	}
	cleaned := strings.Trim(strings.TrimSpace(relPath), "/")
	if cleaned == "" {
		return parent.ID.String(), parent.Path
	}
	return lookupExistingByPath(svc, parent.Path+"/"+cleaned)
}

// lookupChildOfParent reports whether a child with the given name exists
// under parent and, if so, returns the diagnostic id/path strings. Used by
// upload session create to fail fast before any segment is uploaded.
func lookupChildOfParent(svc *domain.Service, parent *domain.FileMeta, name string) (id, path string, found bool) {
	if parent == nil {
		return "", "", false
	}
	id2, path2 := lookupExistingByPath(svc, parent.Path+"/"+name)
	if id2 == "" {
		return "", "", false
	}
	return id2, path2, true
}

// lookupExistingChildOfID resolves parentID + child name to the diagnostic
// fields for a 409 response on the transfer endpoint. Best-effort — empty
// strings on lookup failure.
func lookupExistingChildOfID(svc *domain.Service, parentID domain.FileID, name string) (id, path string) {
	parent, err := svc.GetFile(parentID)
	if err != nil {
		return "", ""
	}
	id2, path2, _ := lookupChildOfParent(svc, parent, name)
	return id2, path2
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
