package cli

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/valentinkolb/filegate/domain"
	"github.com/valentinkolb/filegate/infra/detect"
	"github.com/valentinkolb/filegate/infra/metrics"
)

// versionsDirName mirrors domain.versionsDirName — kept here so the CLI
// orphan-warning check doesn't need to import an unexported domain
// constant. Both must stay in sync.
const versionsDirName = ".fg-versions"

// warnOrphanVersionDirs scans every base path for a leftover
// .fg-versions directory before the Pebble index is rebuilt. Detached
// blobs aren't fatal but do leak storage; the warning is loud so the
// operator notices and either copies the blobs out for recovery or
// removes them.
func warnOrphanVersionDirs(basePaths []string) {
	for _, base := range basePaths {
		dir := filepath.Join(base, versionsDirName)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // no orphan dir, no problem
		}
		log.Printf("[filegate] WARNING: index rebuild detached version blobs at %s (%d files). "+
			"After Filegate starts, the orphans cannot be reattached automatically. "+
			"Consider copying the blobs out before they are reclaimed, or `rm -rf %s` to free the storage.",
			dir, len(entries), dir)
	}
}

// versioningShouldEnable resolves the operator's versioning.enabled
// setting to a definitive on/off boolean.
//
//   - "off" : feature disabled, regardless of filesystem.
//   - "on"  : feature enabled. The user explicitly opted in; no btrfs
//     check (operator is responsible for capability).
//   - "auto" (default): on iff every base_path is btrfs. We check once
//     at startup; live mount changes are not detected.
func versioningShouldEnable(cfg domain.VersioningConfig, basePaths []string) bool {
	switch cfg.Enabled {
	case "off":
		return false
	case "on":
		return true
	}
	// auto
	ok, err := detect.SupportsBTRFS(context.Background(), basePaths)
	if err != nil {
		log.Printf("[filegate] versioning auto-detect: btrfs check failed: %v — disabling feature", err)
		return false
	}
	return ok
}

// runVersioningPruner periodically calls Service.PruneVersions until the
// context is cancelled. The first prune happens after one interval (not
// at startup) so a flapping daemon doesn't immediately churn through
// reflinked blobs after every restart.
func runVersioningPruner(ctx context.Context, svc *domain.Service, interval time.Duration, reg *metrics.Registry, state *lifecycleState, done chan<- struct{}) {
	defer close(done)
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Routed through the shared guard so a manual trigger and the
			// ticker cannot run at the same time. Recorded either way: a
			// failing pruner is exactly what an operator needs to see, and it
			// used to leave only a log line.
			stats, err := state.Run(svc.PruneVersions)
			if errors.Is(err, ErrPruneInProgress) {
				continue
			}
			if err != nil {
				log.Printf("[filegate] versioning pruner: %v", err)
				continue
			}
			reg.PruneStats(stats.VersionsDeleted, stats.VersionsKept, stats.Errors)
			if stats.VersionsDeleted > 0 || stats.OrphansPurged > 0 {
				log.Printf("[filegate] versioning pruner: scanned=%d kept=%d deleted=%d orphans=%d errors=%d",
					stats.FilesScanned, stats.VersionsKept, stats.VersionsDeleted,
					stats.OrphansPurged, stats.Errors)
			}
		}
	}
}
