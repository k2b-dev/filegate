package cli

import (
	"context"
	"log"
	"time"

	s3adapter "github.com/k2b-dev/filegate/v3/adapter/s3"
	"github.com/k2b-dev/filegate/v3/domain"
	"github.com/k2b-dev/filegate/v3/infra/metrics"
)

// resolveS3CleanupConfig fills in the adapter's defaults for any
// zero-valued field in the operator's config, then returns the
// merged config. A negative Interval is preserved (operator opt-
// out signal); zero Interval picks up the default 1h.
func resolveS3CleanupConfig(cfg domain.S3CleanupConfig) s3adapter.MultipartCleanupConfig {
	defaults := s3adapter.DefaultMultipartCleanupConfig()
	out := s3adapter.MultipartCleanupConfig{
		DoneRetention:     cfg.DoneRetention,
		AbortedRetention:  cfg.AbortedRetention,
		StuckUploadMaxAge: cfg.StuckUploadMaxAge,
		Interval:          cfg.Interval,
	}
	if out.DoneRetention <= 0 {
		out.DoneRetention = defaults.DoneRetention
	}
	if out.AbortedRetention <= 0 {
		out.AbortedRetention = defaults.AbortedRetention
	}
	if out.StuckUploadMaxAge <= 0 {
		out.StuckUploadMaxAge = defaults.StuckUploadMaxAge
	}
	if out.Interval == 0 {
		out.Interval = defaults.Interval
	}
	return out
}

// runMultipartCleanupLoop periodically calls
// s3adapter.SweepMultipartCleanup until ctx is cancelled. Mirrors
// runVersioningPruner: the first sweep happens after one interval
// (not at startup) so a flapping daemon doesn't churn through
// recently-completed uploads on every restart. Logs only when the
// pass actually retired or aborted something — quiet steady-state.
func runMultipartCleanupLoop(ctx context.Context, svc *domain.Service, cfg s3adapter.MultipartCleanupConfig, reg *metrics.Registry, done chan<- struct{}) {
	defer close(done)
	if cfg.Interval <= 0 {
		cfg.Interval = time.Hour
	}
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			res := s3adapter.SweepMultipartCleanup(svc, cfg)
			recordCleanupResult(reg, res)
			if res.DoneRetired > 0 || res.AbortedRetired > 0 || res.StuckAborted > 0 || res.Errors > 0 {
				log.Printf("[filegate-s3] multipart cleanup: scanned=%d done-retired=%d aborted-retired=%d stuck-aborted=%d errors=%d",
					res.StageDirsScanned, res.DoneRetired, res.AbortedRetired, res.StuckAborted, res.Errors)
			}
		}
	}
}

// recordCleanupResult tallies a cleanup-sweep result into the metrics
// counters. reg may be nil (no-op). Split out so the wiring is
// unit-testable without driving the ticker loop.
func recordCleanupResult(reg *metrics.Registry, res s3adapter.MultipartCleanupResult) {
	reg.CleanupRetired("done", res.DoneRetired)
	reg.CleanupRetired("aborted", res.AbortedRetired)
	reg.CleanupRetired("stuck", res.StuckAborted)
	reg.CleanupErrors(res.Errors)
}
