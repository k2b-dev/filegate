package cli

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	httpadapter "github.com/valentinkolb/filegate/adapter/http"
	s3adapter "github.com/valentinkolb/filegate/adapter/s3"
	apiv1 "github.com/valentinkolb/filegate/api/v1"
	"github.com/valentinkolb/filegate/domain"
	"github.com/valentinkolb/filegate/infra/activity"
	"github.com/valentinkolb/filegate/infra/detect"
	"github.com/valentinkolb/filegate/infra/eventbus"
	"github.com/valentinkolb/filegate/infra/filesystem"
	"github.com/valentinkolb/filegate/infra/metrics"
	indexpebble "github.com/valentinkolb/filegate/infra/pebble"
	"github.com/valentinkolb/filegate/infra/runtimecfg"

	"github.com/spf13/cobra"
)

// Build identity, injectable via ldflags
// (-X github.com/valentinkolb/filegate/cli.buildVersion=...). Default
// to "dev"/"none" so an unstamped build still emits a well-formed
// filegate_build_info series.
var (
	buildVersion = "dev"
	buildCommit  = "none"
)

// metricsRESTHandler returns the /metrics http.Handler when metrics is
// enabled, or nil otherwise (NewRouter then doesn't mount the route).
// The registry is always live; this only gates the endpoint.
func metricsRESTHandler(cfg domain.Config, reg *metrics.Registry) http.Handler {
	if !cfg.Metrics.Enabled {
		return nil
	}
	return reg.Handler()
}

// wrapMetrics wraps an adapter handler with the RED middleware when
// metrics is enabled, otherwise returns it untouched (zero per-request
// overhead when disabled). skipPath, when set, bypasses instrumentation
// for that exact path — used to keep the /metrics scrape endpoint from
// counting itself on every scrape.
func wrapMetrics(h http.Handler, reg *metrics.Registry, adapter string, enabled bool, skipPath string) http.Handler {
	if !enabled {
		return h
	}
	instrumented := reg.Middleware(adapter)(h)
	if skipPath == "" {
		return instrumented
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == skipPath {
			h.ServeHTTP(w, r)
			return
		}
		instrumented.ServeHTTP(w, r)
	})
}

func newDaemonServeCmd() *cobra.Command {
	var configFile string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start Filegate HTTP server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS != "linux" {
				return fmt.Errorf("filegate v2 currently supports linux only")
			}

			cfg, err := loadConfig(configFile)
			if err != nil {
				return err
			}
			if err := applyChangedConfigFlags(cmd.Flags(), &cfg); err != nil {
				return err
			}

			// Probe every mount before opening the index. Catches
			// the operator who mounted ext4 without user_xattr,
			// who pointed at a read-only path, or whose mount
			// silently dropped to disk-full overnight. Failing here
			// is way better than failing on the first PUT, when
			// the symptom is a confusing 500 to a real client.
			if err := ensureDefaultBasePath(cfg); err != nil {
				return err
			}
			if err := checkMountsHealthOrFail(cfg.Storage.BasePaths); err != nil {
				return err
			}

			// Validate the metrics path before doing any work so a
			// misconfiguration fails loudly at startup, not on the
			// first scrape.
			if cfg.Metrics.Enabled {
				if err := httpadapter.ValidateMetricsPath(cfg.Metrics.Path); err != nil {
					return err
				}
			}
			// The runtime store is opened before anything else durable so a
			// bad path fails fast, and it is validated to sit outside the
			// index directory, which index rebuilds delete.
			runtimeStore, err := runtimecfg.Open(cfg.Storage.RuntimeConfigPath)
			if err != nil {
				return err
			}
			defer func() { _ = runtimeStore.Close() }()

			// Re-resolve now that the store is available, so runtime overrides
			// stored by a previous run apply from this boot onward.
			resolved, err := resolveConfig(configFile, runtimeStore.Overrides())
			if err != nil {
				return err
			}
			cfg = resolved.Config

			// A fresh install has no token configured; generate and store one
			// so the service is reachable without editing any file first.
			token, tErr := bootstrapBearerToken(runtimeStore, cfg.Auth.BearerToken)
			if tErr != nil {
				return tErr
			}
			cfg.Auth.BearerToken = token
			resolved.Config = cfg

			configManager := newConfigManager(configFile, runtimeStore, resolved)
			// Created before the router because the router exposes it; the S3
			// adapter is attached later, once its listener is built.
			s3Keys := newS3KeyService(runtimeStore)

			idx, svc, err := buildCore(cfg)
			if err != nil {
				return err
			}

			// Metrics registry: always constructed so the background-
			// loop + rate-limit counters are live from boot. Only the
			// /metrics endpoint and the per-request middleware are
			// gated on metrics.enabled. Pass it to the adapters and
			// loops below.
			// The detector is created further down, so the provider reads it
			// through this holder rather than capturing a nil value.
			var detectorRef detect.Runner
			metricsReg := metrics.New(
				metrics.BuildInfo{Version: buildVersion, Commit: buildCommit},
				metricsStatsProvider{
					svc:       svc,
					indexPath: cfg.Storage.IndexPath,
					detectorStats: func() detect.Stats {
						if detectorRef == nil {
							return detect.Stats{}
						}
						return detectorRef.Stats()
					},
				},
			)
			activityLog := activity.NewRing(cfg.Activity.RingBufferSize)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			detector, err := detect.New(cfg.Detection.Backend, cfg.Storage.BasePaths, cfg.Detection.PollInterval)
			if err != nil {
				_ = idx.Close()
				return err
			}
			detectorRef = detector
			log.Printf("[filegate] detection backend: %s", detector.Name())
			detector.Start(ctx)
			detectorDone := make(chan struct{})
			go func() {
				defer close(detectorDone)
				consumeDetectorEvents(ctx, svc, detector.Events(), metricsReg)
			}()

			lifecycle := &lifecycleState{}
			versioningEnabled := versioningShouldEnable(cfg.Versioning, cfg.Storage.BasePaths)
			svc.EnableVersioning(cfg.Versioning, versioningEnabled)
			prunerDone := make(chan struct{})
			if versioningEnabled {
				log.Printf("[filegate] versioning: enabled (cooldown=%s, pruner_interval=%s)",
					cfg.Versioning.Cooldown, cfg.Versioning.PrunerInterval)
				go runVersioningPruner(ctx, svc, cfg.Versioning.PrunerInterval, metricsReg, lifecycle, prunerDone)
			} else {
				close(prunerDone)
				log.Printf("[filegate] versioning: disabled (config=%q, btrfs check failed for at least one mount)",
					cfg.Versioning.Enabled)
			}

			trustedProxies, err := httpadapter.ParseTrustedProxies(cfg.Server.TrustedProxies)
			if err != nil {
				cancel()
				detector.Close()
				<-detectorDone
				<-prunerDone
				_ = idx.Close()
				return err
			}

			router := httpadapter.NewRouter(svc, httpadapter.RouterOptions{
				BearerToken:                cfg.Auth.BearerToken,
				AccessLogEnabled:           cfg.Server.AccessLogEnabled,
				PublicURL:                  cfg.Server.PublicURL,
				TrustedProxies:             trustedProxies,
				CORS:                       cfg.Server.CORS,
				IndexPath:                  cfg.Storage.IndexPath,
				JobWorkers:                 cfg.Jobs.Workers,
				JobQueueSize:               cfg.Jobs.QueueSize,
				ThumbnailJobWorkers:        cfg.Jobs.ThumbnailWorkers,
				ThumbnailJobQueueSize:      cfg.Jobs.ThumbnailQueueSize,
				UploadExpiry:               cfg.Upload.Expiry,
				UploadCleanupInterval:      cfg.Upload.CleanupInterval,
				MaxChunkBytes:              cfg.Upload.MaxChunkBytes,
				MaxUploadBytes:             cfg.Upload.MaxUploadBytes,
				MaxSessionUploadBytes:      cfg.Upload.MaxSessionUploadBytes,
				MaxConcurrentSegmentWrites: cfg.Upload.MaxConcurrentSegmentWrites,
				UploadMinFreeBytes:         cfg.Upload.MinFreeBytes,
				ThumbnailLRUCacheSize:      cfg.Thumbnail.LRUCacheSize,
				ThumbnailMaxSourceBytes:    cfg.Thumbnail.MaxSourceBytes,
				ThumbnailMaxPixels:         cfg.Thumbnail.MaxPixels,
				Rescan:                     svc.Rescan,
				MetricsHandler:             metricsRESTHandler(cfg, metricsReg),
				MetricsPath:                cfg.Metrics.Path,
				MetricsToken:               cfg.Metrics.Token,
				ActivityLog:                activityLog,
				Config:                     configManager.Holder(),
				ConfigService:              configManager,
				S3Keys:                     s3Keys,

				BuildVersion:  buildVersion,
				BuildCommit:   buildCommit,
				BasePaths:     cfg.Storage.BasePaths,
				PathCacheSize: cfg.Cache.PathCacheSize,
				DetectorStats: detector.Stats,
				Lifecycle: func() apiv1.LifecycleRuntime {
					return lifecycle.Snapshot(cfg.Versioning.PrunerInterval)
				},
				PruneNow: func() (domain.PruneStats, error) {
					if !versioningEnabled {
						return domain.PruneStats{}, fmt.Errorf("versioning is disabled, so there is nothing to prune")
					}
					return lifecycle.Run(svc.PruneVersions)
				},

				VersioningEnabled:          versioningEnabled,
				VersioningMode:             cfg.Versioning.Enabled,
				VersioningCooldown:         cfg.Versioning.Cooldown,
				VersioningPrunerInterval:   cfg.Versioning.PrunerInterval,
				VersioningMaxPinnedPerFile: cfg.Versioning.MaxPinnedPerFile,
			})
			var routerCloser interface{ Close() error }
			if closer, ok := router.(interface{ Close() error }); ok {
				routerCloser = closer
			}

			srv := &http.Server{
				Addr:              cfg.Server.Listen,
				Handler:           wrapMetrics(router, metricsReg, "rest", cfg.Metrics.Enabled, cfg.Metrics.Path),
				ReadHeaderTimeout: 10 * time.Second,
				ReadTimeout:       30 * time.Second,
				WriteTimeout:      cfg.Server.WriteTimeout,
				IdleTimeout:       120 * time.Second,
				MaxHeaderBytes:    1 << 20,
			}
			errCh := make(chan error, 2)
			go func() {
				log.Printf("[filegate] listening on %s", cfg.Server.Listen)
				errCh <- srv.ListenAndServe()
			}()

			// Start the S3 listener if configured. Lives on its own
			// port so the operator can bind it to a different
			// interface (e.g. internal-only) and so SigV4 middleware
			// doesn't apply to REST routes. Multi-tenant via Keys
			// list; legacy single-tenant AccessKey/SecretKey is
			// folded into the key store by the adapter.
			var s3Srv *http.Server
			var s3CleanupDone chan struct{}
			if cfg.S3.Enabled {
				if cfg.S3.AccessKey == "" && cfg.S3.SecretKey == "" && len(cfg.S3.Keys) == 0 {
					cancel()
					detector.Close()
					<-detectorDone
					<-prunerDone
					_ = idx.Close()
					return errors.New("s3.enabled=true requires either s3.access_key/s3.secret_key or at least one s3.keys entry")
				}
				keys := make([]s3adapter.KeyEntry, 0, len(cfg.S3.Keys))
				for _, k := range cfg.S3.Keys {
					keys = append(keys, s3adapter.KeyEntry{
						AccessKey:         k.AccessKey,
						SecretKey:         k.SecretKey,
						Buckets:           k.Buckets,
						RequestsPerSecond: k.RequestsPerSecond,
						Burst:             k.Burst,
					})
				}
				s3Handler, hErr := s3adapter.NewHandler(svc, s3adapter.Options{
					Region:              cfg.S3.Region,
					AccessKey:           cfg.S3.AccessKey,
					SecretKey:           cfg.S3.SecretKey,
					Keys:                keys,
					AccessLogEnabled:    cfg.Server.AccessLogEnabled,
					Metrics:             metricsReg,
					MaxConcurrentWrites: cfg.S3.MaxConcurrentWrites,
					ActivityLog:         activityLog,
				})
				// Seed before attaching: attaching publishes, and publishing an
				// empty store first would leave the adapter with no keys until
				// the seed landed.
				if hErr == nil {
					hErr = s3Keys.SeedOnce(cfg.S3.AccessKey, cfg.S3.SecretKey, keys)
				}
				if hErr == nil {
					hErr = s3Keys.AttachHandler(s3Handler)
				}
				if hErr != nil {
					cancel()
					detector.Close()
					<-detectorDone
					<-prunerDone
					_ = idx.Close()
					return hErr
				}
				s3Srv = &http.Server{
					Addr:              cfg.S3.Listen,
					Handler:           wrapMetrics(s3Handler, metricsReg, "s3", cfg.Metrics.Enabled, ""),
					ReadHeaderTimeout: 10 * time.Second,
					ReadTimeout:       30 * time.Second,
					WriteTimeout:      cfg.Server.WriteTimeout,
					IdleTimeout:       120 * time.Second,
					MaxHeaderBytes:    1 << 20,
				}
				go func() {
					log.Printf("[filegate-s3] listening on %s", cfg.S3.Listen)
					errCh <- s3Srv.ListenAndServe()
				}()

				// Multipart cleanup loop: retires done/aborted
				// manifests + their durable Pebble records past the
				// retention window, forcibly aborts uploads stuck
				// open past max age. Interval < 0 disables it.
				cleanupCfg := resolveS3CleanupConfig(cfg.S3.Cleanup)
				if cleanupCfg.Interval > 0 {
					log.Printf("[filegate-s3] multipart cleanup: interval=%s done-retention=%s aborted-retention=%s stuck-max-age=%s",
						cleanupCfg.Interval, cleanupCfg.DoneRetention, cleanupCfg.AbortedRetention, cleanupCfg.StuckUploadMaxAge)
					s3CleanupDone = make(chan struct{})
					go runMultipartCleanupLoop(ctx, svc, cleanupCfg, metricsReg, s3CleanupDone)
				} else {
					log.Printf("[filegate-s3] multipart cleanup: disabled (interval=%s)", cleanupCfg.Interval)
				}
			}

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

			// Build the shutdown plan so both the signal-driven
			// path and the listener-error path drain identically.
			plan := shutdownPlan{
				Timeout: cfg.Server.ShutdownTimeout,
				Listeners: []namedListener{
					{Name: "rest", Server: srv},
					{Name: "s3", Server: s3Srv}, // nil-safe: runShutdown skips
				},
				CancelBackground: cancel,
				BackgroundDone:   []chan struct{}{detectorDone, prunerDone, s3CleanupDone},
				AfterDrain: []func() error{
					func() error { detector.Close(); return nil },
					func() error {
						if routerCloser != nil {
							return routerCloser.Close()
						}
						return nil
					},
					idx.Close,
				},
			}

			select {
			case sig := <-sigCh:
				log.Printf("[filegate] received signal %s, shutting down", sig)
				return runShutdown(plan)
			case err := <-errCh:
				if err != nil && !errors.Is(err, http.ErrServerClosed) {
					log.Printf("[filegate] listener errored, shutting down: %v", err)
					_ = runShutdown(plan)
					return err
				}
				return runShutdown(plan)
			}
		},
	}
	cmd.Flags().StringVar(&configFile, "config", "", "path to config file")
	registerConfigFlags(cmd.Flags())
	return cmd
}

func buildCore(cfg domain.Config) (*indexpebble.Index, *domain.Service, error) {
	if err := os.MkdirAll(cfg.Storage.IndexPath, 0o755); err != nil {
		return nil, nil, err
	}
	idx, err := indexpebble.Open(cfg.Storage.IndexPath, 128<<20)
	if err != nil && errors.Is(err, indexpebble.ErrUnsupportedIndexFormat) {
		log.Printf("[filegate] index format mismatch, rebuilding index at %s", cfg.Storage.IndexPath)
		// Pre-rebuild check: if any watched mount already has a
		// .fg-versions directory, the upcoming RemoveAll will detach
		// it from its index records — version blobs will linger as
		// untracked storage. Surface this loudly so the operator can
		// decide (typically: rm -rf the .fg-versions dirs after
		// confirming they don't need recovery).
		warnOrphanVersionDirs(cfg.Storage.BasePaths)
		if rmErr := os.RemoveAll(cfg.Storage.IndexPath); rmErr != nil {
			return nil, nil, rmErr
		}
		if mkErr := os.MkdirAll(cfg.Storage.IndexPath, 0o755); mkErr != nil {
			return nil, nil, mkErr
		}
		idx, err = indexpebble.Open(cfg.Storage.IndexPath, 128<<20)
	}
	if err != nil {
		return nil, nil, err
	}
	svc, err := domain.NewService(idx, filesystem.New(), eventbus.New(), cfg.Storage.BasePaths, cfg.Cache.PathCacheSize)
	if err != nil {
		_ = idx.Close()
		return nil, nil, err
	}
	// When S3 is enabled, every mount name must be a valid S3 bucket
	// name — otherwise the S3 listener (M1+) would refuse requests
	// or, worse, produce confusing 404s. Fail startup loudly here so
	// the operator catches the misconfiguration up-front.
	if cfg.S3.Enabled {
		if err := svc.ValidateMountsForS3(); err != nil {
			_ = idx.Close()
			return nil, nil, err
		}
	}
	return idx, svc, nil
}

func consumeDetectorEvents(ctx context.Context, svc *domain.Service, events <-chan []detect.Event, reg *metrics.Registry) {
	for {
		select {
		case <-ctx.Done():
			return
		case batch, ok := <-events:
			if !ok {
				return
			}
			if len(batch) == 0 {
				continue
			}
			merged := coalesceDetectorBatches(ctx, events, batch)
			if len(merged) == 0 {
				continue
			}
			recordDetectorEvents(reg, merged)
			if err := applyDetectorBatch(svc, merged); err != nil {
				if isDetectorTerminalError(err) {
					log.Printf("[filegate] detector stopping: %v", err)
					return
				}
				log.Printf("[filegate] detector batch apply failed: %v, falling back to full rescan", err)
				if rescanErr := svc.Rescan(); rescanErr != nil {
					if isDetectorTerminalError(rescanErr) {
						log.Printf("[filegate] detector stopping after rescan error: %v", rescanErr)
						return
					}
					log.Printf("[filegate] fallback rescan failed: %v", rescanErr)
				}
			}
		}
	}
}

// recordDetectorEvents tallies a coalesced detector batch into the
// per-type counter. Counting after coalescing (not per raw event)
// keeps it cheap. reg may be nil (no-op).
func recordDetectorEvents(reg *metrics.Registry, batch []detect.Event) {
	if reg == nil {
		return
	}
	var created, changed, deleted, unknown int
	for _, e := range batch {
		switch e.Type {
		case detect.EventCreated:
			created++
		case detect.EventChanged:
			changed++
		case detect.EventDeleted:
			deleted++
		default:
			unknown++
		}
	}
	reg.DetectorEvents("created", created)
	reg.DetectorEvents("changed", changed)
	reg.DetectorEvents("deleted", deleted)
	reg.DetectorEvents("unknown", unknown)
}

func coalesceDetectorBatches(ctx context.Context, events <-chan []detect.Event, first []detect.Event) []detect.Event {
	combined := append([]detect.Event(nil), first...)
	for {
		select {
		case <-ctx.Done():
			return combined
		case next, ok := <-events:
			if !ok {
				return combined
			}
			if len(next) == 0 {
				continue
			}
			combined = append(combined, next...)
		default:
			return combined
		}
	}
}

// detectEventPriority orders events when the same path appears multiple
// times in a single batch — Unknown beats Deleted beats Changed beats
// Created so a higher-precedence outcome wins coalescing.
func detectEventPriority(t detect.EventType) int {
	switch t {
	case detect.EventUnknown:
		return 4
	case detect.EventDeleted:
		return 3
	case detect.EventChanged:
		return 2
	case detect.EventCreated:
		return 1
	}
	return 0
}

func applyDetectorBatch(svc *domain.Service, batch []detect.Event) error {
	if len(batch) == 0 {
		return nil
	}

	byPath := make(map[string]detect.Event, len(batch))
	for _, ev := range batch {
		abs := strings.TrimSpace(ev.AbsPath)
		if abs == "" {
			continue
		}
		if cur, ok := byPath[abs]; ok {
			if detectEventPriority(ev.Type) >= detectEventPriority(cur.Type) {
				byPath[abs] = ev
			}
			continue
		}
		byPath[abs] = ev
	}
	if len(byPath) == 0 {
		return nil
	}

	hasUnknown := false
	unknownBases := make([]string, 0, 4)
	deletePaths := make([]string, 0, len(byPath))
	syncPaths := make([]string, 0, len(byPath))
	for abs, ev := range byPath {
		switch ev.Type {
		case detect.EventUnknown:
			hasUnknown = true
			if strings.TrimSpace(ev.Base) != "" {
				unknownBases = append(unknownBases, ev.Base)
			}
		case detect.EventDeleted:
			deletePaths = append(deletePaths, abs)
		case detect.EventCreated, detect.EventChanged:
			syncPaths = append(syncPaths, abs)
		}
		_ = abs
	}

	if hasUnknown {
		if len(unknownBases) == 0 {
			return svc.Rescan()
		}
		for _, base := range uniqueStrings(unknownBases) {
			if err := svc.RescanMount(base); err != nil {
				return err
			}
		}
		return nil
	}

	sort.Slice(deletePaths, func(i, j int) bool { return len(deletePaths[i]) > len(deletePaths[j]) })
	for _, abs := range deletePaths {
		if err := svc.RemoveAbsPath(abs); err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
	}

	sort.Strings(syncPaths)
	for _, abs := range syncPaths {
		if err := svc.SyncAbsPath(abs); err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				if rmErr := svc.RemoveAbsPath(abs); rmErr != nil && !errors.Is(rmErr, domain.ErrNotFound) {
					return rmErr
				}
				continue
			}
			return err
		}
	}

	// Directory-sync: every parent dir touched by an event in this batch
	// gets a readdir-driven reconcile pass. This is the cheap correctness
	// primitive that catches stale namespace edges left behind by
	// operations the inode stream alone can't describe — hardlink unlink,
	// in-subvol rename, recursive deletes whose intermediate children
	// vanished without their own delete events, etc.
	//
	// For directory events we reconcile BOTH the parent (to catch the
	// dir's own rename/removal at the parent level) AND the dir itself
	// (to catch new/stale entries inside the touched dir).
	dirtyDirs := make(map[string]struct{}, len(byPath))
	for abs, ev := range byPath {
		dirtyDirs[filepath.Dir(abs)] = struct{}{}
		if ev.IsDir {
			dirtyDirs[abs] = struct{}{}
		}
	}
	for dir := range dirtyDirs {
		if err := svc.ReconcileDirectory(dir); err != nil {
			log.Printf("[filegate] ReconcileDirectory(%q) failed: %v", dir, err)
		}
	}

	return nil
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func isDetectorTerminalError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	return indexpebble.IsTerminalError(err)
}
