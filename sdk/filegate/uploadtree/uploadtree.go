// Package uploadtree uploads whole folders through Filegate upload sessions.
//
// The session protocol is deliberately one logical file per session, so a
// folder upload is a client-side batch over many sessions. The TypeScript SDK
// already does that for browsers (sdk/ts/src/uploads.ts); this package is the
// Go equivalent, with the same pipeline shape: bounded hashing, batched session
// creation, bounded segment uploads, and one global progress view.
//
// It differs from the browser version in one place on purpose. The browser has
// no bearer token, so it asks an application server to create sessions through
// an "allow" callback. A Go caller holds the token, so this package talks to
// POST /v1/uploads/sessions:batch itself.
package uploadtree

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/k2b-dev/filegate/v3/sdk/filegate"
	"github.com/k2b-dev/filegate/v3/sdk/filegate/segments"
)

// Source is one file to upload.
//
// OpenAt is called once for the hash pass and once per segment attempt, so it
// must be repeatable. Use FromFile/FromBytes/FromDir unless the bytes come from
// somewhere unusual.
type Source struct {
	// Path is the destination virtual path, mount name included.
	Path string
	// Size is the exact byte count. A mismatch fails the upload server-side.
	Size int64
	// ContentType is optional and stored with the committed node.
	ContentType string
	// Checksum, when set, must be "sha256:<hex>" over the whole file and
	// skips the hash pass for this source.
	Checksum string
	// OpenAt returns the bytes in [offset, offset+length).
	OpenAt func(offset, length int64) (io.ReadCloser, error)
}

// Concurrency bounds each stage of the pipeline independently. Segment uploads
// are bounded globally rather than per file, because per-file limits multiply
// into a connection storm once a folder has thousands of entries.
type Concurrency struct {
	Hash     int
	Create   int
	Files    int
	Segments int
}

// Batch controls how many sessions are created per request. FlushInterval caps
// how long a partially filled batch waits, so a slow hash stage cannot stall
// session creation.
type Batch struct {
	Size          int
	FlushInterval time.Duration
}

// Retry configures per-request retries. Only transport errors and retryable
// status codes (408, 425, 429, 5xx) are retried; a 4xx is a decision, not a
// hiccup, and repeating it just wastes the server's time.
type Retry struct {
	Attempts int
	Base     time.Duration
	Max      time.Duration
}

// Options configures Upload. The zero value is valid and uses defaults.
type Options struct {
	SegmentSize int64
	OnConflict  filegate.FileConflictMode
	Concurrency Concurrency
	Batch       Batch
	Retry       Retry

	// DirectThresholdBytes routes files at or below this size to a
	// one-shot PUT /v1/paths upload, skipping session creation entirely.
	// Empty files always take this path because a session requires size > 0.
	//
	// 0 means the default: SegmentSize, so any file that would have been a
	// single segment goes direct. A session buys nothing there -- it costs
	// three requests instead of one, and its resumability amounts to retrying
	// the same lone segment. Set a negative value to send everything through
	// sessions.
	DirectThresholdBytes int64

	// SkipExisting resolves every destination path up front and skips the
	// ones that already exist, instead of letting each create fail.
	SkipExisting bool

	// Resume adopts in-progress sessions left by an earlier run whose
	// path, size, segment size and checksum still match, and re-uploads
	// only the missing segments.
	Resume bool

	// OnEvent observes progress. It is called from multiple goroutines and
	// must not block; panics and slow handlers directly slow the upload.
	OnEvent func(Event)
}

// EventType identifies what happened. New values may be added, so callers
// should ignore unknown ones.
type EventType string

const (
	EventStart      EventType = "start"
	EventHashed     EventType = "hashed"
	EventSkipped    EventType = "skipped"
	EventCreated    EventType = "created"
	EventResumed    EventType = "resumed"
	EventProgress   EventType = "progress"
	EventCommitting EventType = "committing"
	EventFileDone   EventType = "done"
	EventFileError  EventType = "error"
	EventFinished   EventType = "finished"
)

// Event carries one progress notification plus the global progress snapshot at
// the time it was emitted.
type Event struct {
	Type      EventType
	Path      string
	SessionID string
	Err       error
	Node      *filegate.Node
	Progress  Progress
}

// Progress is the global view across all files in one Upload call.
type Progress struct {
	Files            int
	FilesDone        int
	FilesFailed      int
	FilesSkipped     int
	TotalBytes       int64
	TransferredBytes int64
	Elapsed          time.Duration
	BytesPerSecond   float64
}

// FileResult is the per-source outcome, returned in input order.
type FileResult struct {
	Path      string
	SessionID string
	Node      *filegate.Node
	Skipped   bool
	Resumed   bool
	Err       error
}

// Result is the outcome of one Upload call.
type Result struct {
	Total            int
	Done             int
	Failed           int
	Skipped          int
	TotalBytes       int64
	TransferredBytes int64
	Elapsed          time.Duration
	Files            []FileResult
}

const (
	defaultSegmentSize   = 8 << 20
	defaultHashWorkers   = 4
	defaultCreateWorkers = 4
	defaultFileWorkers   = 8
	defaultSegWorkers    = 8
	defaultBatchSize     = 32
	defaultFlush         = 20 * time.Millisecond
	defaultAttempts      = 3
	defaultRetryBase     = 100 * time.Millisecond
	defaultRetryMax      = 5 * time.Second
	maxBatchSize         = 1000 // server rejects larger sessions:batch bodies
)

// FromFile builds a Source for a local file.
func FromFile(localPath, remotePath string) (Source, error) {
	st, err := os.Stat(localPath)
	if err != nil {
		return Source{}, err
	}
	if st.IsDir() {
		return Source{}, fmt.Errorf("%s is a directory", localPath)
	}
	return Source{
		Path: remotePath,
		Size: st.Size(),
		OpenAt: func(offset, length int64) (io.ReadCloser, error) {
			f, err := os.Open(localPath)
			if err != nil {
				return nil, err
			}
			return sectionCloser{Reader: io.NewSectionReader(f, offset, length), closer: f}, nil
		},
	}, nil
}

// FromBytes builds a Source for an in-memory payload.
func FromBytes(remotePath string, data []byte) Source {
	return Source{
		Path: remotePath,
		Size: int64(len(data)),
		OpenAt: func(offset, length int64) (io.ReadCloser, error) {
			if offset < 0 || length < 0 || offset+length > int64(len(data)) {
				return nil, fmt.Errorf("range out of bounds")
			}
			return io.NopCloser(bytes.NewReader(data[offset : offset+length])), nil
		},
	}
}

// FromDir walks localRoot and returns one Source per regular file, mapped under
// remotePrefix with the relative directory structure preserved. Symlinks are
// skipped rather than followed, so a link loop cannot turn into an upload loop.
func FromDir(localRoot, remotePrefix string) ([]Source, error) {
	var out []Source
	err := filepath.WalkDir(localRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(localRoot, p)
		if err != nil {
			return err
		}
		src, err := FromFile(p, path.Join(remotePrefix, filepath.ToSlash(rel)))
		if err != nil {
			return err
		}
		out = append(out, src)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

type sectionCloser struct {
	io.Reader
	closer io.Closer
}

func (s sectionCloser) Close() error { return s.closer.Close() }

// plannedFile is one source after the hash pass.
type plannedFile struct {
	index           int
	source          Source
	checksum        string
	segmentChecksum []string
}

// fileWork is one unit for the upload stage: either a session to fill and
// commit, or a whole-file PUT that skipped the session protocol entirely. Both
// go through the same worker pool so a folder of tiny files is not limited by
// the (much smaller) hash pool.
type fileWork struct {
	index   int
	source  Source
	planned plannedFile
	session filegate.UploadSessionResponse
	resumed bool
	whole   bool
}

type runner struct {
	fg   *filegate.Filegate
	opts Options

	results []FileResult
	mu      sync.Mutex

	started     time.Time
	totalBytes  int64
	transferred atomic.Int64
	done        atomic.Int64
	failed      atomic.Int64
	skipped     atomic.Int64
	files       int

	segmentSlots chan struct{}
	adoptable    map[string]filegate.UploadSessionSummary
}

// Upload uploads every source and returns once all of them have finished. A
// failing file does not stop the others; its error lands in Result.Files. The
// returned error is non-nil only when the whole run could not proceed.
func Upload(ctx context.Context, fg *filegate.Filegate, sources []Source, opts Options) (Result, error) {
	if fg == nil {
		return Result{}, errors.New("client is required")
	}
	opts = withDefaults(opts)
	for i, src := range sources {
		if strings.TrimSpace(src.Path) == "" {
			return Result{}, fmt.Errorf("sources[%d]: path is required", i)
		}
		if src.Size < 0 {
			return Result{}, fmt.Errorf("sources[%d]: size must be >= 0", i)
		}
		if src.OpenAt == nil {
			return Result{}, fmt.Errorf("sources[%d]: OpenAt is required", i)
		}
	}

	r := &runner{
		fg:           fg,
		opts:         opts,
		results:      make([]FileResult, len(sources)),
		started:      time.Now(),
		files:        len(sources),
		segmentSlots: make(chan struct{}, opts.Concurrency.Segments),
	}
	for i, src := range sources {
		r.totalBytes += src.Size
		r.results[i] = FileResult{Path: src.Path}
	}
	r.emit(Event{Type: EventStart})

	pending := make([]int, 0, len(sources))
	for i := range sources {
		pending = append(pending, i)
	}
	if opts.SkipExisting {
		var err error
		pending, err = r.dropExisting(ctx, sources, pending)
		if err != nil {
			return r.result(), err
		}
	}
	if opts.Resume {
		r.adoptable = r.listAdoptable(ctx)
	}

	r.run(ctx, sources, pending)
	r.emit(Event{Type: EventFinished})
	res := r.result()
	if err := ctx.Err(); err != nil {
		return res, err
	}
	return res, nil
}

func withDefaults(o Options) Options {
	if o.SegmentSize <= 0 {
		o.SegmentSize = defaultSegmentSize
	}
	// Resolved after SegmentSize so an explicit segment size carries into the
	// threshold. Negative stays negative: that is how a caller opts out, and
	// the send path treats any non-positive value as "no shortcut".
	if o.DirectThresholdBytes == 0 {
		o.DirectThresholdBytes = o.SegmentSize
	}
	if o.Concurrency.Hash <= 0 {
		o.Concurrency.Hash = defaultHashWorkers
	}
	if o.Concurrency.Create <= 0 {
		o.Concurrency.Create = defaultCreateWorkers
	}
	if o.Concurrency.Files <= 0 {
		o.Concurrency.Files = defaultFileWorkers
	}
	if o.Concurrency.Segments <= 0 {
		o.Concurrency.Segments = defaultSegWorkers
	}
	if o.Batch.Size <= 0 {
		o.Batch.Size = defaultBatchSize
	}
	if o.Batch.Size > maxBatchSize {
		o.Batch.Size = maxBatchSize
	}
	if o.Batch.FlushInterval <= 0 {
		o.Batch.FlushInterval = defaultFlush
	}
	if o.Retry.Attempts <= 0 {
		o.Retry.Attempts = defaultAttempts
	}
	if o.Retry.Base <= 0 {
		o.Retry.Base = defaultRetryBase
	}
	if o.Retry.Max <= 0 {
		o.Retry.Max = defaultRetryMax
	}
	return o
}

// run wires the four stages together. Each stage is a bounded worker pool
// connected by channels, so memory stays proportional to the concurrency
// settings rather than to the number of files.
func (r *runner) run(ctx context.Context, sources []Source, pending []int) {
	hashIn := make(chan int)
	hashed := make(chan plannedFile, r.opts.Batch.Size)
	batches := make(chan []plannedFile, r.opts.Concurrency.Create)
	work := make(chan fileWork, r.opts.Concurrency.Files)

	var hashWG, batchWG, createWG, uploadWG sync.WaitGroup

	for i := 0; i < r.opts.Concurrency.Hash; i++ {
		hashWG.Add(1)
		go func() {
			defer hashWG.Done()
			for idx := range hashIn {
				src := sources[idx]
				// A session needs size > 0, so empty files and
				// anything under the threshold bypass it.
				if src.Size == 0 || (r.opts.DirectThresholdBytes > 0 && src.Size <= r.opts.DirectThresholdBytes) {
					work <- fileWork{index: idx, source: src, whole: true}
					continue
				}
				planned, err := r.hash(ctx, idx, src)
				if err != nil {
					r.fail(idx, src.Path, "", err)
					continue
				}
				r.emit(Event{Type: EventHashed, Path: src.Path})
				hashed <- planned
			}
		}()
	}

	batchWG.Add(1)
	go func() {
		defer batchWG.Done()
		defer close(batches)
		r.batcher(hashed, batches)
	}()

	for i := 0; i < r.opts.Concurrency.Create; i++ {
		createWG.Add(1)
		go func() {
			defer createWG.Done()
			for batch := range batches {
				for _, sw := range r.createSessions(ctx, batch) {
					work <- sw
				}
			}
		}()
	}

	for i := 0; i < r.opts.Concurrency.Files; i++ {
		uploadWG.Add(1)
		go func() {
			defer uploadWG.Done()
			for item := range work {
				if item.whole {
					r.uploadWhole(ctx, item.index, item.source)
					continue
				}
				r.uploadSession(ctx, item)
			}
		}()
	}

	for _, idx := range pending {
		select {
		case hashIn <- idx:
		case <-ctx.Done():
			r.fail(idx, sources[idx].Path, "", ctx.Err())
		}
	}
	// Shutdown order matters: both the hash workers (whole-file PUTs) and
	// the create workers (sessions) feed `work`, so it can only close once
	// both pools are done.
	close(hashIn)
	hashWG.Wait()
	close(hashed)
	batchWG.Wait()
	createWG.Wait()
	close(work)
	uploadWG.Wait()
}

func (r *runner) batcher(in <-chan plannedFile, out chan<- []plannedFile) {
	pending := make([]plannedFile, 0, r.opts.Batch.Size)
	timer := time.NewTimer(r.opts.Batch.FlushInterval)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	for {
		select {
		case item, ok := <-in:
			if !ok {
				if len(pending) > 0 {
					out <- pending
				}
				return
			}
			pending = append(pending, item)
			if len(pending) >= r.opts.Batch.Size {
				out <- pending
				pending = make([]plannedFile, 0, r.opts.Batch.Size)
				timer.Stop()
				continue
			}
			if len(pending) == 1 {
				timer.Reset(r.opts.Batch.FlushInterval)
			}
		case <-timer.C:
			if len(pending) > 0 {
				out <- pending
				pending = make([]plannedFile, 0, r.opts.Batch.Size)
			}
		}
	}
}

// hash reads the file once and produces both the whole-file checksum and the
// per-segment checksums, so segment PUTs can be verified server-side without a
// second pass over the bytes.
func (r *runner) hash(ctx context.Context, idx int, src Source) (plannedFile, error) {
	if err := ctx.Err(); err != nil {
		return plannedFile{}, err
	}
	if strings.TrimSpace(src.Checksum) != "" {
		plan, err := segments.Plan(src.Size, r.opts.SegmentSize)
		if err != nil {
			return plannedFile{}, err
		}
		return plannedFile{index: idx, source: src, checksum: src.Checksum, segmentChecksum: make([]string, len(plan))}, nil
	}

	plan, err := segments.Plan(src.Size, r.opts.SegmentSize)
	if err != nil {
		return plannedFile{}, err
	}
	whole := sha256.New()
	sums := make([]string, 0, len(plan))
	for _, segment := range plan {
		reader, err := src.OpenAt(segment.Offset, segment.Size)
		if err != nil {
			return plannedFile{}, err
		}
		segHash := sha256.New()
		n, err := io.Copy(io.MultiWriter(whole, segHash), reader)
		closeErr := reader.Close()
		if err != nil {
			return plannedFile{}, err
		}
		if closeErr != nil {
			return plannedFile{}, closeErr
		}
		if n != segment.Size {
			return plannedFile{}, fmt.Errorf("segment %d: read %d bytes, expected %d", segment.Index, n, segment.Size)
		}
		sums = append(sums, "sha256:"+hex.EncodeToString(segHash.Sum(nil)))
	}
	return plannedFile{
		index:           idx,
		source:          src,
		checksum:        "sha256:" + hex.EncodeToString(whole.Sum(nil)),
		segmentChecksum: sums,
	}, nil
}

// createSessions turns one batch into sessions, adopting resumable ones first.
// A batch that fails as a whole fails every file in it, matching the server's
// all-or-nothing rollback on POST /v1/uploads/sessions:batch.
func (r *runner) createSessions(ctx context.Context, batch []plannedFile) []fileWork {
	out := make([]fileWork, 0, len(batch))
	fresh := make([]plannedFile, 0, len(batch))
	for _, item := range batch {
		if session, ok := r.adopt(ctx, item); ok {
			r.emit(Event{Type: EventResumed, Path: item.source.Path, SessionID: session.ID})
			out = append(out, fileWork{planned: item, session: *session, resumed: true})
			continue
		}
		fresh = append(fresh, item)
	}
	if len(fresh) == 0 {
		return out
	}

	uploads := make([]filegate.UploadSessionCreateRequest, 0, len(fresh))
	for _, item := range fresh {
		uploads = append(uploads, filegate.UploadSessionCreateRequest{
			Path:        item.source.Path,
			Size:        item.source.Size,
			Checksum:    item.checksum,
			SegmentSize: r.opts.SegmentSize,
			ContentType: item.source.ContentType,
			OnConflict:  string(r.opts.OnConflict),
		})
	}

	if len(fresh) == 1 {
		session, err := retryValue(ctx, r.opts.Retry, func() (*filegate.UploadSessionResponse, error) {
			return r.fg.Uploads.Sessions.Create(ctx, uploads[0])
		})
		if err != nil {
			r.fail(fresh[0].index, fresh[0].source.Path, "", err)
			return out
		}
		r.emit(Event{Type: EventCreated, Path: fresh[0].source.Path, SessionID: session.ID})
		return append(out, fileWork{planned: fresh[0], session: *session})
	}

	created, err := retryValue(ctx, r.opts.Retry, func() (*filegate.UploadSessionBatchCreateResponse, error) {
		return r.fg.Uploads.Sessions.CreateBatch(ctx, filegate.UploadSessionBatchCreateRequest{Uploads: uploads})
	})
	if err == nil && len(created.Sessions) != len(fresh) {
		err = fmt.Errorf("batch create returned %d sessions for %d uploads", len(created.Sessions), len(fresh))
	}
	if err != nil {
		for _, item := range fresh {
			r.fail(item.index, item.source.Path, "", err)
		}
		return out
	}
	for i, item := range fresh {
		r.emit(Event{Type: EventCreated, Path: item.source.Path, SessionID: created.Sessions[i].ID})
		out = append(out, fileWork{planned: item, session: created.Sessions[i]})
	}
	return out
}

// adopt reuses an in-progress session from an earlier run. Every identifying
// field has to match, checksum included, because adopting a session for
// different content would silently commit the wrong bytes.
func (r *runner) adopt(ctx context.Context, item plannedFile) (*filegate.UploadSessionResponse, bool) {
	if !r.opts.Resume || len(r.adoptable) == 0 {
		return nil, false
	}
	summary, ok := r.adoptable[normalizePath(item.source.Path)]
	if !ok || summary.Size != item.source.Size || summary.SegmentSize != r.opts.SegmentSize {
		return nil, false
	}
	session, err := r.fg.Uploads.Sessions.Status(ctx, filegate.UploadSessionStatusRequest{SessionID: summary.ID})
	if err != nil || session.Checksum != item.checksum || session.Phase != "in_progress" {
		return nil, false
	}
	return session, true
}

func (r *runner) listAdoptable(ctx context.Context) map[string]filegate.UploadSessionSummary {
	list, err := r.fg.System.UploadSessions(ctx, "in_progress")
	if err != nil || list == nil {
		return nil
	}
	out := make(map[string]filegate.UploadSessionSummary, len(list.Items))
	for _, item := range list.Items {
		// Keep the newest session per path; older ones for the same
		// target are leftovers the server will expire on its own.
		key := normalizePath(item.Path)
		if prev, ok := out[key]; ok && prev.UpdatedAt >= item.UpdatedAt {
			continue
		}
		out[key] = item
	}
	return out
}

// dropExisting removes destinations that already exist so the run does not burn
// a create round trip per file just to collect 409s.
func (r *runner) dropExisting(ctx context.Context, sources []Source, pending []int) ([]int, error) {
	const chunk = 500
	existing := make(map[string]bool, len(pending))
	for start := 0; start < len(pending); start += chunk {
		end := start + chunk
		if end > len(pending) {
			end = len(pending)
		}
		paths := make([]string, 0, end-start)
		for _, idx := range pending[start:end] {
			paths = append(paths, sources[idx].Path)
		}
		resolved, err := r.fg.Index.ResolvePaths(ctx, paths)
		if err != nil {
			return nil, err
		}
		// The endpoint answers positionally and puts nil where nothing
		// resolved, so index alignment is the only way to read it.
		for i, item := range resolved.Items {
			if item != nil && i < len(paths) {
				existing[normalizePath(paths[i])] = true
			}
		}
	}

	out := make([]int, 0, len(pending))
	for _, idx := range pending {
		if existing[normalizePath(sources[idx].Path)] {
			r.skip(idx, sources[idx].Path)
			continue
		}
		out = append(out, idx)
	}
	return out, nil
}

// uploadWhole handles the files that never get a session: empty ones, and
// anything under DirectThresholdBytes where a single PUT beats create + segment
// + commit.
func (r *runner) uploadWhole(ctx context.Context, idx int, src Source) {
	node, err := retryValue(ctx, r.opts.Retry, func() (*filegate.PathPutResponse, error) {
		reader, err := src.OpenAt(0, src.Size)
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		return r.fg.Paths.Put(ctx, src.Path, reader, filegate.PutPathOptions{
			ContentType: src.ContentType,
			OnConflict:  r.opts.OnConflict,
		})
	})
	if err != nil {
		r.fail(idx, src.Path, "", err)
		return
	}
	r.transferred.Add(src.Size)
	r.succeed(idx, src.Path, "", &node.Node, false)
}

func (r *runner) uploadSession(ctx context.Context, sw fileWork) {
	uploaded := make(map[int]bool, len(sw.session.UploadedSegments))
	for _, index := range sw.session.UploadedSegments {
		uploaded[index] = true
	}
	// Segments already on the server count as transferred so a resumed run
	// does not report a throughput number it never achieved.
	var resumedBytes int64
	for _, segment := range sw.session.Segments {
		if uploaded[segment.Index] {
			resumedBytes += segment.Size
		}
	}
	r.transferred.Add(resumedBytes)

	var wg sync.WaitGroup
	var firstErr atomic.Value
	for _, segment := range sw.session.Segments {
		if uploaded[segment.Index] {
			continue
		}
		wg.Add(1)
		go func(segment filegate.UploadSessionSegment) {
			defer wg.Done()
			select {
			case r.segmentSlots <- struct{}{}:
			case <-ctx.Done():
				firstErr.CompareAndSwap(nil, ctx.Err())
				return
			}
			defer func() { <-r.segmentSlots }()

			err := retry(ctx, r.opts.Retry, func() error {
				reader, err := sw.planned.source.OpenAt(segment.Offset, segment.Size)
				if err != nil {
					return err
				}
				defer reader.Close()
				_, err = r.fg.Uploads.Sessions.PutSegment(ctx, filegate.UploadSessionPutSegmentRequest{
					SessionID:   sw.session.ID,
					Index:       segment.Index,
					Body:        reader,
					Checksum:    segmentChecksum(sw.planned, segment.Index),
					ContentType: sw.planned.source.ContentType,
				})
				return err
			})
			if err != nil {
				firstErr.CompareAndSwap(nil, err)
				return
			}
			r.transferred.Add(segment.Size)
			r.emit(Event{Type: EventProgress, Path: sw.planned.source.Path, SessionID: sw.session.ID})
		}(segment)
	}
	wg.Wait()

	if err, ok := firstErr.Load().(error); ok {
		r.fail(sw.planned.index, sw.planned.source.Path, sw.session.ID, err)
		return
	}

	r.emit(Event{Type: EventCommitting, Path: sw.planned.source.Path, SessionID: sw.session.ID})
	committed, err := retryValue(ctx, r.opts.Retry, func() (*filegate.UploadSessionCommitResponse, error) {
		return r.fg.Uploads.Sessions.Commit(ctx, filegate.UploadSessionCommitRequest{SessionID: sw.session.ID})
	})
	if err != nil {
		r.fail(sw.planned.index, sw.planned.source.Path, sw.session.ID, err)
		return
	}
	r.succeed(sw.planned.index, sw.planned.source.Path, sw.session.ID, &committed.Node, sw.resumed)
}

func segmentChecksum(planned plannedFile, index int) string {
	if index < 0 || index >= len(planned.segmentChecksum) {
		return ""
	}
	return planned.segmentChecksum[index]
}

func (r *runner) succeed(idx int, path, sessionID string, node *filegate.Node, resumed bool) {
	r.mu.Lock()
	r.results[idx] = FileResult{Path: path, SessionID: sessionID, Node: node, Resumed: resumed}
	r.mu.Unlock()
	r.done.Add(1)
	r.emit(Event{Type: EventFileDone, Path: path, SessionID: sessionID, Node: node})
}

func (r *runner) fail(idx int, path, sessionID string, err error) {
	r.mu.Lock()
	r.results[idx] = FileResult{Path: path, SessionID: sessionID, Err: err}
	r.mu.Unlock()
	r.failed.Add(1)
	r.emit(Event{Type: EventFileError, Path: path, SessionID: sessionID, Err: err})
}

func (r *runner) skip(idx int, path string) {
	r.mu.Lock()
	r.results[idx] = FileResult{Path: path, Skipped: true}
	r.mu.Unlock()
	r.skipped.Add(1)
	r.emit(Event{Type: EventSkipped, Path: path})
}

func (r *runner) progress() Progress {
	elapsed := time.Since(r.started)
	transferred := r.transferred.Load()
	var rate float64
	if elapsed > 0 {
		rate = float64(transferred) / elapsed.Seconds()
	}
	return Progress{
		Files:            r.files,
		FilesDone:        int(r.done.Load()),
		FilesFailed:      int(r.failed.Load()),
		FilesSkipped:     int(r.skipped.Load()),
		TotalBytes:       r.totalBytes,
		TransferredBytes: transferred,
		Elapsed:          elapsed,
		BytesPerSecond:   rate,
	}
}

func (r *runner) emit(event Event) {
	if r.opts.OnEvent == nil {
		return
	}
	event.Progress = r.progress()
	r.opts.OnEvent(event)
}

func (r *runner) result() Result {
	r.mu.Lock()
	files := make([]FileResult, len(r.results))
	copy(files, r.results)
	r.mu.Unlock()
	return Result{
		Total:            r.files,
		Done:             int(r.done.Load()),
		Failed:           int(r.failed.Load()),
		Skipped:          int(r.skipped.Load()),
		TotalBytes:       r.totalBytes,
		TransferredBytes: r.transferred.Load(),
		Elapsed:          time.Since(r.started),
		Files:            files,
	}
}

func normalizePath(p string) string {
	return strings.Trim(strings.TrimSpace(p), "/")
}

func retry(ctx context.Context, cfg Retry, fn func() error) error {
	_, err := retryValue(ctx, cfg, func() (struct{}, error) { return struct{}{}, fn() })
	return err
}

func retryValue[T any](ctx context.Context, cfg Retry, fn func() (T, error)) (T, error) {
	var zero T
	var last error
	delay := cfg.Base
	for attempt := 0; attempt < cfg.Attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		out, err := fn()
		if err == nil {
			return out, nil
		}
		last = err
		if !retryable(err) {
			return zero, err
		}
		if attempt == cfg.Attempts-1 {
			break
		}
		// Jitter keeps a batch of segments that failed together from
		// retrying in lockstep.
		sleep := delay + time.Duration(rand.Int63n(int64(delay/2)+1))
		if sleep > cfg.Max {
			sleep = cfg.Max
		}
		select {
		case <-time.After(sleep):
		case <-ctx.Done():
			return zero, ctx.Err()
		}
		if delay < cfg.Max {
			delay *= 2
		}
	}
	return zero, last
}

func retryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var apiErr *filegate.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
			return true
		}
		return apiErr.StatusCode >= 500
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	// A server that closes a keep-alive connection mid-body surfaces as a
	// bare EOF rather than a net.Error; that is exactly the case a retry is
	// meant to cover.
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
}
