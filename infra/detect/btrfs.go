package detect

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var btrfsGenerationRE = regexp.MustCompile(`(?m)^\s*Generation:\s*([0-9]+)\s*$`)
var btrfsTransidMarkerRE = regexp.MustCompile(`(?m)^\s*transid marker was\s+([0-9]+)\s*$`)
var btrfsInodeRE = regexp.MustCompile(`(?m)\binode\s+([0-9]+)\b`)

var runBTRFSCommand = func(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "btrfs", args...)
	return cmd.CombinedOutput()
}

// BTRFSDetector detects filesystem changes using btrfs generation-based delta queries.
type BTRFSDetector struct {
	basePaths []string
	interval  time.Duration

	events chan []Event

	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu             sync.Mutex
	lastGeneration map[string]uint64

	// Observability, guarded by mu together with lastGeneration.
	cycle            uint64
	lastScanAt       time.Time
	lastScanDuration time.Duration
	scanErrors       uint64
}

// NewBTRFSDetector creates a btrfs-optimized change detector for the given paths.
func NewBTRFSDetector(basePaths []string, interval time.Duration) *BTRFSDetector {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	clean := make([]string, 0, len(basePaths))
	for _, p := range basePaths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		clean = append(clean, filepath.Clean(p))
	}
	return &BTRFSDetector{
		basePaths:      clean,
		interval:       interval,
		events:         make(chan []Event, 64),
		lastGeneration: make(map[string]uint64, len(clean)),
	}
}

func (d *BTRFSDetector) Name() string {
	return "btrfs"
}

func (d *BTRFSDetector) Events() <-chan []Event {
	return d.events
}

func (d *BTRFSDetector) Start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	d.cancel = cancel

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.initialize(ctx)

		ticker := time.NewTicker(d.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				batch := d.poll(ctx)
				if len(batch) == 0 {
					continue
				}
				select {
				case d.events <- batch:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
}

func (d *BTRFSDetector) ForceRescan(ctx context.Context) error {
	d.mu.Lock()
	for _, basePath := range d.basePaths {
		d.lastGeneration[basePath] = 0
	}
	d.mu.Unlock()

	batch := make([]Event, 0, len(d.basePaths))
	for _, basePath := range d.basePaths {
		batch = append(batch, Event{Type: EventUnknown, Base: basePath, AbsPath: basePath, IsDir: true})
	}
	select {
	case d.events <- batch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *BTRFSDetector) Close() {
	if d.cancel != nil {
		d.cancel()
	}
	d.wg.Wait()
	close(d.events)
}

func (d *BTRFSDetector) initialize(ctx context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, basePath := range d.basePaths {
		gen, err := currentGeneration(ctx, basePath)
		if err != nil {
			d.scanErrors++
			log.Printf("[filegate] btrfs detector init failed for %q: %v", basePath, err)
			continue
		}
		d.lastGeneration[basePath] = gen
	}
}

func (d *BTRFSDetector) poll(ctx context.Context) []Event {
	started := time.Now()
	d.mu.Lock()
	defer func() {
		d.lastScanAt = time.Now()
		d.lastScanDuration = time.Since(started)
		d.mu.Unlock()
	}()

	d.cycle++
	batch := make([]Event, 0, 64)
	for _, basePath := range d.basePaths {
		prev := d.lastGeneration[basePath]
		if prev == 0 {
			gen, err := currentGeneration(ctx, basePath)
			if err != nil {
				d.scanErrors++
				log.Printf("[filegate] btrfs detector generation read failed for %q: %v", basePath, err)
				continue
			}
			d.lastGeneration[basePath] = gen
			continue
		}

		current, err := currentGeneration(ctx, basePath)
		if err != nil {
			d.scanErrors++
			log.Printf("[filegate] btrfs detector generation read failed for %q: %v", basePath, err)
			continue
		}
		if current < prev {
			// Generation went backwards: the subvolume was rolled back,
			// replaced, or recreated. The delta base is meaningless now —
			// resync from the new generation and rescan the mount instead
			// of silently skipping every cycle forever.
			log.Printf("[filegate] btrfs generation went backwards for %q (%d -> %d): scheduling rescan", basePath, prev, current)
			d.lastGeneration[basePath] = current
			batch = append(batch, Event{Type: EventUnknown, Base: basePath, AbsPath: basePath, IsDir: true})
			continue
		}

		events, nextGen, err := d.deltaEvents(ctx, basePath, prev, current)
		if err != nil {
			d.scanErrors++
			log.Printf("[filegate] btrfs delta scan failed for %q: %v", basePath, err)
			batch = append(batch, Event{Type: EventUnknown, Base: basePath, AbsPath: basePath, IsDir: true})
			d.lastGeneration[basePath] = current
			continue
		}
		if nextGen == 0 {
			nextGen = current
		}
		d.lastGeneration[basePath] = nextGen
		batch = append(batch, events...)
	}

	return batch
}

func (d *BTRFSDetector) deltaEvents(ctx context.Context, basePath string, fromGen, currentGen uint64) ([]Event, uint64, error) {
	out, err := runBTRFSCommand(ctx, "subvolume", "find-new", basePath, strconv.FormatUint(fromGen, 10))
	if err != nil {
		return nil, 0, fmt.Errorf("btrfs subvolume find-new %q from %d failed: %w (%s)", basePath, fromGen, err, strings.TrimSpace(string(out)))
	}

	nextGen := currentGen
	if m := btrfsTransidMarkerRE.FindStringSubmatch(string(out)); len(m) == 2 {
		if parsed, parseErr := strconv.ParseUint(strings.TrimSpace(m[1]), 10, 64); parseErr == nil {
			nextGen = parsed
		}
	}

	inodeSet := make(map[uint64]struct{}, 64)
	for _, m := range btrfsInodeRE.FindAllStringSubmatch(string(out), -1) {
		if len(m) != 2 {
			continue
		}
		ino, parseErr := strconv.ParseUint(strings.TrimSpace(m[1]), 10, 64)
		if parseErr != nil {
			continue
		}
		inodeSet[ino] = struct{}{}
	}
	if len(inodeSet) == 0 {
		// find-new can advance only the marker without resolvable inode rows
		// (e.g. deletes). Emit Unknown to keep index correctness via scoped rescan.
		if nextGen > fromGen {
			return []Event{{Type: EventUnknown, Base: basePath, AbsPath: basePath, IsDir: true}}, nextGen, nil
		}
		return nil, nextGen, nil
	}

	inodes := make([]uint64, 0, len(inodeSet))
	for ino := range inodeSet {
		inodes = append(inodes, ino)
	}
	sort.Slice(inodes, func(i, j int) bool { return inodes[i] < inodes[j] })

	events := make([]Event, 0, len(inodes))
	eventByAbs := make(map[string]Event, len(inodes))
	unresolved := 0

	for _, ino := range inodes {
		paths, resolveErr := inodeToPaths(ctx, basePath, ino)
		if resolveErr != nil || len(paths) == 0 {
			unresolved++
			continue
		}
		for _, absPath := range paths {
			info, statErr := os.Lstat(absPath)
			if statErr != nil {
				if errors.Is(statErr, os.ErrNotExist) {
					unresolved++
					continue
				}
				continue
			}
			if info.Mode()&os.ModeSymlink != 0 {
				continue
			}
			eventByAbs[absPath] = Event{
				Type:    EventChanged,
				Base:    basePath,
				AbsPath: absPath,
				IsDir:   info.IsDir(),
				Size:    info.Size(),
				MtimeMS: info.ModTime().UnixMilli(),
			}
		}
	}

	for _, ev := range eventByAbs {
		events = append(events, ev)
	}
	sort.Slice(events, func(i, j int) bool { return events[i].AbsPath < events[j].AbsPath })

	if unresolved > 0 {
		events = append(events, Event{Type: EventUnknown, Base: basePath, AbsPath: basePath, IsDir: true})
	}

	return events, nextGen, nil
}

func supportsBTRFS(ctx context.Context, basePaths []string) (bool, error) {
	for _, p := range basePaths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, err := currentGeneration(ctx, p); err != nil {
			var execErr *exec.Error
			if ok := strings.Contains(err.Error(), "executable file not found"); ok {
				return false, nil
			}
			if ok := strings.Contains(err.Error(), "not a btrfs filesystem"); ok {
				return false, nil
			}
			if ok := strings.Contains(err.Error(), "Not a Btrfs"); ok {
				return false, nil
			}
			if ok := strings.Contains(err.Error(), "not a subvolume"); ok {
				return false, nil
			}
			if errors.As(err, &execErr) {
				return false, nil
			}
			return false, err
		}
	}
	return true, nil
}

func currentGeneration(ctx context.Context, basePath string) (uint64, error) {
	out, err := runBTRFSCommand(ctx, "subvolume", "show", basePath)
	if err != nil {
		return 0, fmt.Errorf("btrfs subvolume show %q failed: %w (%s)", basePath, err, strings.TrimSpace(string(out)))
	}
	m := btrfsGenerationRE.FindStringSubmatch(string(out))
	if len(m) != 2 {
		return 0, fmt.Errorf("btrfs generation not found for %q", basePath)
	}
	gen, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid btrfs generation for %q: %w", basePath, err)
	}
	return gen, nil
}

func inodeToPaths(ctx context.Context, basePath string, inode uint64) ([]string, error) {
	out, err := runBTRFSCommand(ctx, "inspect-internal", "inode-resolve", strconv.FormatUint(inode, 10), basePath)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, 2)
	s := bufio.NewScanner(strings.NewReader(string(out)))
	seen := make(map[string]struct{}, 4)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		absPath := line
		if !filepath.IsAbs(absPath) {
			absPath = filepath.Join(basePath, line)
		}
		absPath = filepath.Clean(absPath)
		if absPath != basePath && !strings.HasPrefix(absPath, basePath+string(os.PathSeparator)) {
			continue
		}
		if _, ok := seen[absPath]; ok {
			continue
		}
		seen[absPath] = struct{}{}
		paths = append(paths, absPath)
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return paths, nil
}

// Stats reports scan progress plus the last observed generation per base path,
// which is what makes detector lag measurable on btrfs.
func (d *BTRFSDetector) Stats() Stats {
	d.mu.Lock()
	defer d.mu.Unlock()

	generations := make(map[string]uint64, len(d.lastGeneration))
	for path, gen := range d.lastGeneration {
		generations[path] = gen
	}

	return Stats{
		Backend:          d.Name(),
		Interval:         d.interval,
		Cycles:           d.cycle,
		LastScanAt:       d.lastScanAt,
		LastScanDuration: d.lastScanDuration,
		Errors:           d.scanErrors,
		PendingBatches:   len(d.events),
		QueueCapacity:    cap(d.events),
		Generations:      generations,
	}
}
