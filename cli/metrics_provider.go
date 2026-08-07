package cli

import (
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/k2b-dev/filegate/v3/domain"
	"github.com/k2b-dev/filegate/v3/infra/detect"
	"github.com/k2b-dev/filegate/v3/infra/metrics"
)

// metricsStatsProvider adapts a domain.Service into the
// metrics.StatsProvider interface. It is read on every Prometheus
// scrape, so each call does an svc.Stats() plus a Statfs per mount
// plus a shallow size walk of the index dir — all fast. Keeping this
// in cli (not in infra/metrics) is deliberate: the metrics package
// stays free of any domain or syscall dependency.
type metricsStatsProvider struct {
	svc       *domain.Service
	indexPath string
	// detectorStats is set after the detector exists, which happens later in
	// startup than the metrics registry. Nil means the detector gauges report
	// zero rather than the provider failing the whole scrape.
	detectorStats func() detect.Stats
}

func (p metricsStatsProvider) MetricsSnapshot() (metrics.Snapshot, error) {
	stats, err := p.svc.Stats()
	if err != nil {
		return metrics.Snapshot{}, err
	}
	_, _, cacheHits, cacheMisses := p.svc.PathCacheStats()
	snap := metrics.Snapshot{
		Files:            stats.TotalFiles,
		Dirs:             stats.TotalDirs,
		PathCacheEntries: stats.PathCacheEntries,
		IndexDBBytes:     dirSizeBytesBestEffort(p.indexPath),
		PathCacheHits:    cacheHits,
		PathCacheMisses:  cacheMisses,
	}
	if p.detectorStats != nil {
		d := p.detectorStats()
		snap.DetectorCycles = d.Cycles
		snap.DetectorErrors = d.Errors
		if !d.LastScanAt.IsZero() {
			snap.DetectorStaleSeconds = time.Since(d.LastScanAt).Seconds()
		}
	}
	for _, m := range stats.Mounts {
		abs, rerr := p.svc.ResolveAbsPath(m.ID)
		if rerr != nil {
			continue
		}
		var st syscall.Statfs_t
		if serr := syscall.Statfs(abs, &st); serr != nil {
			continue
		}
		bsize := uint64(st.Bsize)
		total := uint64(st.Blocks) * bsize
		free := uint64(st.Bavail) * bsize
		used := total - uint64(st.Bfree)*bsize
		snap.Mounts = append(snap.Mounts, metrics.MountSnapshot{
			Name:      m.Name,
			UsedBytes: used,
			FreeBytes: free,
		})
	}
	return snap, nil
}

// dirSizeBytesBestEffort walks a directory tree summing regular-file
// sizes. Returns 0 on any error (the index-size gauge is best-effort
// — a transient read failure shouldn't fail the whole scrape). Mirrors
// the REST adapter's dirSizeBytes but lives here to avoid an
// adapter/http import.
func dirSizeBytesBestEffort(root string) int64 {
	if root == "" {
		return 0
	}
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // best-effort: skip unreadable entries
		}
		if info, ierr := d.Info(); ierr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}
