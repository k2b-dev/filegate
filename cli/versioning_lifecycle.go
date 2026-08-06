package cli

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/valentinkolb/filegate/domain"
	"github.com/valentinkolb/filegate/infra/filesystem"
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

type versioningSelection struct {
	Enabled  bool
	CopyMode string
	Reason   string
}

// selectVersioning resolves the operator setting against the real FICLONE
// probe already performed during startup. This keeps policy simple: auto only
// enables cheap versions, while an explicit "on" accepts byte-copy cost.
func selectVersioning(cfg domain.VersioningConfig, mounts []filesystem.MountHealth) versioningSelection {
	allReflink := len(mounts) > 0
	reflinkCount := 0
	for _, mount := range mounts {
		if mount.ReflinkSupported {
			reflinkCount++
			continue
		}
		allReflink = false
	}

	switch cfg.Enabled {
	case "off":
		return versioningSelection{CopyMode: "disabled", Reason: "disabled by configuration"}
	case "on":
		copyMode := "byte-copy"
		if allReflink {
			copyMode = "reflink"
		} else if reflinkCount > 0 {
			copyMode = "mixed"
		}
		return versioningSelection{
			Enabled:  true,
			CopyMode: copyMode,
			Reason:   "enabled by configuration",
		}
	}
	if allReflink {
		return versioningSelection{
			Enabled:  true,
			CopyMode: "reflink",
			Reason:   "auto enabled because every mount supports reflink",
		}
	}
	return versioningSelection{
		CopyMode: "disabled",
		Reason:   "auto disabled because at least one mount lacks reflink support",
	}
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
