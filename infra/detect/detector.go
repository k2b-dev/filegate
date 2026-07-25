package detect

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// EventType identifies the kind of filesystem change detected.
type EventType int

const (
	EventCreated EventType = iota
	EventChanged
	EventDeleted
	EventUnknown
)

// Event describes a single filesystem change detected by a Runner.
type Event struct {
	Type    EventType
	Base    string
	AbsPath string
	IsDir   bool
	Size    int64
	MtimeMS int64
}

// Stats is a point-in-time view of a detector's health.
//
// Detection is what keeps the index consistent with filesystem writes that did
// not come through the API, so a stalled detector means silent drift. LastScanAt
// falling behind Interval is the signal for that; Errors and PendingBatches say
// why. Backend-specific fields are zero on the backend that does not track them.
type Stats struct {
	Backend  string
	Interval time.Duration

	// Cycles counts completed scan rounds since start.
	Cycles     uint64
	LastScanAt time.Time
	// LastScanDuration is how long the most recent round took.
	LastScanDuration time.Duration
	Errors           uint64

	// PendingBatches is the depth of the outbound event channel. A sustained
	// non-zero value means the consumer cannot keep up with detection.
	PendingBatches int
	QueueCapacity  int

	// TrackedDirs and TrackedFiles are poll-backend only.
	TrackedDirs  int
	TrackedFiles int

	// Generations is the last observed btrfs generation per base path,
	// btrfs-backend only.
	Generations map[string]uint64
}

// Runner is the interface for pluggable filesystem change detection backends.
type Runner interface {
	Start(context.Context)
	Events() <-chan []Event
	ForceRescan(context.Context) error
	Close()
	Name() string
	Stats() Stats
}

// New creates a Runner for the specified backend ("auto", "poll", or "btrfs").
func New(backend string, basePaths []string, interval time.Duration) (Runner, error) {
	mode := strings.ToLower(strings.TrimSpace(backend))
	if mode == "" {
		mode = "auto"
	}

	switch mode {
	case "poll":
		return NewPoller(basePaths, interval), nil
	case "btrfs":
		ok, err := supportsBTRFS(context.Background(), basePaths)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("detection backend btrfs requested, but at least one base path is not btrfs")
		}
		return NewBTRFSDetector(basePaths, interval), nil
	case "auto":
		ok, err := supportsBTRFS(context.Background(), basePaths)
		if err != nil {
			return nil, err
		}
		if ok {
			return NewBTRFSDetector(basePaths, interval), nil
		}
		return NewPoller(basePaths, interval), nil
	default:
		return nil, fmt.Errorf("unknown detection backend: %s", mode)
	}
}
