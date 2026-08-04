package metrics

import "github.com/prometheus/client_golang/prometheus"

// StatsProvider supplies the data the scrape-time domain collector
// turns into gauges. It is implemented by a thin adapter over
// domain.Service in the cli package — keeping this package free of any
// domain or syscall dependency, so it stays trivially unit-testable
// with a fake.
//
// Snapshot is read on every Prometheus scrape; the implementation
// should be cheap (the cli adapter does an svc.Stats() plus a Statfs
// per mount, both fast). A scrape that fails to read the snapshot
// emits no domain gauges for that scrape rather than erroring the
// whole /metrics response.
type StatsProvider interface {
	MetricsSnapshot() (Snapshot, error)
}

// Snapshot is a flat, dependency-free view of the domain state the
// collector exposes.
type Snapshot struct {
	Files            int
	Dirs             int
	PathCacheEntries int
	IndexDBBytes     int64
	Mounts           []MountSnapshot

	// PathCacheHits and PathCacheMisses are cumulative since process start.
	// Occupancy alone cannot distinguish an undersized cache from a cold one.
	PathCacheHits   uint64
	PathCacheMisses uint64

	// Worker-pool saturation is deliberately absent: the scheduler lives
	// inside the HTTP router, which this provider cannot reach without an
	// awkward back-channel. It is available on GET /v1/system/runtime.
	//
	// DetectorStaleSeconds is the time since the last completed detection
	// round. Growing far past the scan interval means detection stopped and
	// the index is silently drifting from the filesystem.
	DetectorStaleSeconds float64
	DetectorCycles       uint64
	DetectorErrors       uint64
}

// MountSnapshot is per-mount disk usage. UsedBytes + FreeBytes come
// from a filesystem statfs at snapshot time.
type MountSnapshot struct {
	Name      string
	UsedBytes uint64
	FreeBytes uint64
}

// domainCollector implements prometheus.Collector, emitting the domain
// gauges at scrape time. Using a collector (rather than gauges updated
// on a timer) means the values are always fresh on scrape and there's
// no background goroutine to manage.
type domainCollector struct {
	provider StatsProvider

	entities  *prometheus.Desc // {type=files|dirs}
	indexDB   *prometheus.Desc
	cacheEntr *prometheus.Desc
	mountUsed *prometheus.Desc // {mount}
	mountFree *prometheus.Desc // {mount}

	cacheLookups  *prometheus.Desc // {result=hit|miss}
	detectorStale *prometheus.Desc
	detectorCycle *prometheus.Desc
	detectorErrs  *prometheus.Desc
}

func newDomainCollector(p StatsProvider) *domainCollector {
	return &domainCollector{
		provider: p,
		entities: prometheus.NewDesc(
			"filegate_index_entities",
			"Indexed entities by type (files, dirs).",
			[]string{"type"}, nil),
		indexDB: prometheus.NewDesc(
			"filegate_index_db_bytes",
			"On-disk size of the Pebble index directory in bytes.",
			nil, nil),
		cacheEntr: prometheus.NewDesc(
			"filegate_path_cache_entries",
			"Current entries in the path resolution cache.",
			nil, nil),
		mountUsed: prometheus.NewDesc(
			"filegate_mount_used_bytes",
			"Used bytes on the filesystem backing a mount.",
			[]string{"mount"}, nil),
		mountFree: prometheus.NewDesc(
			"filegate_mount_free_bytes",
			"Free bytes on the filesystem backing a mount.",
			[]string{"mount"}, nil),
		cacheLookups: prometheus.NewDesc(
			"filegate_path_cache_lookups_total",
			"Path cache lookups by result since process start.",
			[]string{"result"}, nil),
		detectorStale: prometheus.NewDesc(
			"filegate_detector_stale_seconds",
			"Seconds since the detector last completed a scan round.",
			nil, nil),
		detectorCycle: prometheus.NewDesc(
			"filegate_detector_cycles_total",
			"Detection scan rounds completed since process start.",
			nil, nil),
		detectorErrs: prometheus.NewDesc(
			"filegate_detector_errors_total",
			"Detection scan errors since process start.",
			nil, nil),
	}
}

func (c *domainCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.entities
	ch <- c.indexDB
	ch <- c.cacheEntr
	ch <- c.mountUsed
	ch <- c.mountFree
	ch <- c.cacheLookups
	ch <- c.detectorStale
	ch <- c.detectorCycle
	ch <- c.detectorErrs
}

func (c *domainCollector) Collect(ch chan<- prometheus.Metric) {
	snap, err := c.provider.MetricsSnapshot()
	if err != nil {
		// Skip domain gauges this scrape rather than failing the whole
		// /metrics response. Runtime + HTTP metrics still render.
		return
	}
	ch <- prometheus.MustNewConstMetric(c.entities, prometheus.GaugeValue, float64(snap.Files), "files")
	ch <- prometheus.MustNewConstMetric(c.entities, prometheus.GaugeValue, float64(snap.Dirs), "dirs")
	ch <- prometheus.MustNewConstMetric(c.indexDB, prometheus.GaugeValue, float64(snap.IndexDBBytes))
	ch <- prometheus.MustNewConstMetric(c.cacheEntr, prometheus.GaugeValue, float64(snap.PathCacheEntries))
	for _, m := range snap.Mounts {
		ch <- prometheus.MustNewConstMetric(c.mountUsed, prometheus.GaugeValue, float64(m.UsedBytes), m.Name)
		ch <- prometheus.MustNewConstMetric(c.mountFree, prometheus.GaugeValue, float64(m.FreeBytes), m.Name)
	}

	ch <- prometheus.MustNewConstMetric(c.cacheLookups, prometheus.CounterValue, float64(snap.PathCacheHits), "hit")
	ch <- prometheus.MustNewConstMetric(c.cacheLookups, prometheus.CounterValue, float64(snap.PathCacheMisses), "miss")
	ch <- prometheus.MustNewConstMetric(c.detectorStale, prometheus.GaugeValue, snap.DetectorStaleSeconds)
	ch <- prometheus.MustNewConstMetric(c.detectorCycle, prometheus.CounterValue, float64(snap.DetectorCycles))
	ch <- prometheus.MustNewConstMetric(c.detectorErrs, prometheus.CounterValue, float64(snap.DetectorErrors))
}
