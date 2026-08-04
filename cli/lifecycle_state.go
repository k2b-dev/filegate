package cli

import (
	"errors"
	"sync"
	"time"

	apiv1 "github.com/valentinkolb/filegate/api/v1"
	"github.com/valentinkolb/filegate/domain"
)

// lifecycleState remembers the outcome of the last background maintenance run.
//
// The pruner previously computed six numbers, fed three of them to Prometheus,
// and dropped the rest along with any record of when it last ran. An operator
// asking "is retention actually working?" had nothing to look at.
type lifecycleState struct {
	mu sync.RWMutex

	// running guards against overlapping prune rounds. The per-file locks in
	// the pruner already keep concurrent runs from corrupting anything, but two
	// rounds would duplicate the scan and report halves of the same work as if
	// they were separate results.
	running bool

	pruneAt       time.Time
	pruneDuration time.Duration
	pruneStats    domain.PruneStats
	pruneErr      string
	pruneRuns     uint64
}

// ErrPruneInProgress is returned when a round is already running.
var ErrPruneInProgress = errors.New("a pruning round is already in progress")

// Run executes a pruning round unless one is already in flight, and records the
// outcome either way. Both the background ticker and the manual trigger go
// through here, so they cannot overlap.
func (s *lifecycleState) Run(prune func() (domain.PruneStats, error)) (domain.PruneStats, error) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return domain.PruneStats{}, ErrPruneInProgress
	}
	s.running = true
	s.mu.Unlock()

	started := time.Now()
	stats, err := prune()

	s.mu.Lock()
	s.running = false
	s.mu.Unlock()

	s.recordPrune(stats, time.Since(started), err)
	return stats, err
}

func (s *lifecycleState) recordPrune(stats domain.PruneStats, took time.Duration, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneAt = time.Now()
	s.pruneDuration = took
	s.pruneStats = stats
	s.pruneRuns++
	s.pruneErr = ""
	if err != nil {
		s.pruneErr = err.Error()
	}
}

// Snapshot renders the state for the runtime endpoint. Zero values mean the
// loop has not completed a round yet, which the UI shows as such rather than
// as a run that found nothing.
func (s *lifecycleState) Snapshot(interval time.Duration) apiv1.LifecycleRuntime {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := apiv1.LifecycleRuntime{
		PrunerIntervalMs: interval.Milliseconds(),
		PruneRunning:     s.running,
		PruneRuns:        s.pruneRuns,
		PruneError:       s.pruneErr,
	}
	if s.pruneAt.IsZero() {
		return out
	}

	out.LastPruneAt = s.pruneAt.UnixMilli()
	out.LastPruneDurationMs = s.pruneDuration.Milliseconds()
	out.NextPruneAt = s.pruneAt.Add(interval).UnixMilli()
	out.FilesScanned = s.pruneStats.FilesScanned
	out.VersionsKept = s.pruneStats.VersionsKept
	out.VersionsDeleted = s.pruneStats.VersionsDeleted
	out.OrphansPurged = s.pruneStats.OrphansPurged
	out.BlobsDeleted = s.pruneStats.BlobsDeleted
	out.PruneErrors = s.pruneStats.Errors
	return out
}
