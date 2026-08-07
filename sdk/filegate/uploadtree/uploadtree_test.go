package uploadtree

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/k2b-dev/filegate/v3/api/v1"
	"github.com/k2b-dev/filegate/v3/sdk/filegate"
)

// fakeServer is a minimal stand-in for the upload-session endpoints. It records
// what the orchestrator actually sent, which is the part worth asserting on.
type fakeServer struct {
	mu sync.Mutex

	segmentSize int64

	sessions     map[string]*apiv1.UploadSessionResponse
	uploadedBody map[string][]byte
	committed    []string
	createCalls  int
	batchSizes   []int
	putCalls     int
	resolveCalls int
	pathPuts     []string

	// preloaded lets a test seed sessions that a previous run left behind.
	preloaded []apiv1.UploadSessionSummary

	// failSegmentOnce makes the first PUT of each segment fail with the
	// given status, exercising the retry path.
	failSegmentOnce int
	failedSegments  map[string]bool

	// failCreate makes session creation respond with this status.
	failCreate int

	inFlight    atomic.Int32
	maxInFlight atomic.Int32

	resolveExisting map[string]bool
}

func newFakeServer() *fakeServer {
	return &fakeServer{
		segmentSize:     8 << 20,
		sessions:        map[string]*apiv1.UploadSessionResponse{},
		uploadedBody:    map[string][]byte{},
		failedSegments:  map[string]bool{},
		resolveExisting: map[string]bool{},
	}
}

func (f *fakeServer) start(t *testing.T) *filegate.Filegate {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	client, err := filegate.New(filegate.Config{BaseURL: server.URL, Token: "secret"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return client
}

func (f *fakeServer) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && path == "/v1/uploads/sessions:batch":
		f.handleBatchCreate(w, r)
	case r.Method == http.MethodPost && path == "/v1/uploads/sessions":
		f.handleCreate(w, r)
	case r.Method == http.MethodGet && path == "/v1/uploads/sessions":
		f.handleList(w, r)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/commit"):
		f.handleCommit(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/uploads/sessions/"):
		f.handleStatus(w, r)
	case r.Method == http.MethodPut && strings.Contains(path, "/segments/"):
		f.handlePutSegment(w, r)
	case r.Method == http.MethodPut && strings.HasPrefix(path, "/v1/paths/"):
		f.handlePathPut(w, r)
	case r.Method == http.MethodPost && path == "/v1/index/resolve":
		f.handleResolve(w, r)
	default:
		http.Error(w, `{"error":"unexpected "`+r.Method+" "+path+`"}`, http.StatusNotFound)
	}
}

func (f *fakeServer) newSession(req apiv1.UploadSessionCreateRequest) apiv1.UploadSessionResponse {
	segmentSize := req.SegmentSize
	if segmentSize <= 0 {
		segmentSize = f.segmentSize
	}
	id := fmt.Sprintf("upl_%032x", len(f.sessions)+1)
	var plan []apiv1.UploadSessionSegment
	for offset := int64(0); offset < req.Size; offset += segmentSize {
		size := segmentSize
		if offset+size > req.Size {
			size = req.Size - offset
		}
		plan = append(plan, apiv1.UploadSessionSegment{Index: len(plan), Offset: offset, Size: size})
	}
	session := apiv1.UploadSessionResponse{
		ID:            id,
		Path:          req.Path,
		Size:          req.Size,
		Checksum:      req.Checksum,
		SegmentSize:   segmentSize,
		TotalSegments: len(plan),
		Segments:      plan,
		Phase:         "in_progress",
	}
	f.sessions[id] = &session
	return session
}

func (f *fakeServer) handleCreate(w http.ResponseWriter, r *http.Request) {
	var body apiv1.UploadSessionCreateRequest
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.createCalls++
	f.batchSizes = append(f.batchSizes, 1)
	if f.failCreate != 0 {
		status := f.failCreate
		f.mu.Unlock()
		http.Error(w, `{"error":"nope"}`, status)
		return
	}
	session := f.newSession(body)
	f.mu.Unlock()
	writeJSON(w, http.StatusCreated, session)
}

func (f *fakeServer) handleBatchCreate(w http.ResponseWriter, r *http.Request) {
	var body apiv1.UploadSessionBatchCreateRequest
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.createCalls++
	f.batchSizes = append(f.batchSizes, len(body.Uploads))
	if f.failCreate != 0 {
		status := f.failCreate
		f.mu.Unlock()
		http.Error(w, `{"error":"nope"}`, status)
		return
	}
	out := apiv1.UploadSessionBatchCreateResponse{}
	for _, upload := range body.Uploads {
		out.Sessions = append(out.Sessions, f.newSession(upload))
	}
	f.mu.Unlock()
	writeJSON(w, http.StatusCreated, out)
}

func (f *fakeServer) handleList(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	items := append([]apiv1.UploadSessionSummary(nil), f.preloaded...)
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, apiv1.UploadSessionListResponse{Items: items, Total: len(items)})
}

func (f *fakeServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/uploads/sessions/")
	f.mu.Lock()
	session, ok := f.sessions[id]
	var copyOf apiv1.UploadSessionResponse
	if ok {
		copyOf = *session
	}
	f.mu.Unlock()
	if !ok {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, copyOf)
}

func (f *fakeServer) handlePutSegment(w http.ResponseWriter, r *http.Request) {
	current := f.inFlight.Add(1)
	for {
		max := f.maxInFlight.Load()
		if current <= max || f.maxInFlight.CompareAndSwap(max, current) {
			break
		}
	}
	time.Sleep(2 * time.Millisecond)
	defer f.inFlight.Add(-1)

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/uploads/sessions/"), "/segments/")
	id := parts[0]
	index, _ := strconv.Atoi(parts[1])
	key := id + ":" + parts[1]

	f.mu.Lock()
	f.putCalls++
	if f.failSegmentOnce != 0 && !f.failedSegments[key] {
		f.failedSegments[key] = true
		status := f.failSegmentOnce
		f.mu.Unlock()
		http.Error(w, `{"error":"try again"}`, status)
		return
	}
	session, ok := f.sessions[id]
	f.mu.Unlock()
	if !ok {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, `{"error":"read"}`, http.StatusBadRequest)
		return
	}
	if want := r.Header.Get("X-Segment-Checksum"); want != "" {
		sum := sha256.Sum256(body)
		if got := "sha256:" + hex.EncodeToString(sum[:]); got != want {
			http.Error(w, `{"error":"checksum mismatch"}`, http.StatusBadRequest)
			return
		}
	}

	f.mu.Lock()
	f.uploadedBody[key] = body
	found := false
	for _, existing := range session.UploadedSegments {
		if existing == index {
			found = true
		}
	}
	if !found {
		session.UploadedSegments = append(session.UploadedSegments, index)
	}
	uploaded := append([]int(nil), session.UploadedSegments...)
	f.mu.Unlock()

	writeJSON(w, http.StatusOK, apiv1.UploadSegmentResponse{SessionID: id, Index: index, UploadedSegments: uploaded})
}

func (f *fakeServer) handleCommit(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/uploads/sessions/"), "/commit")
	f.mu.Lock()
	session, ok := f.sessions[id]
	if ok {
		f.committed = append(f.committed, id)
		session.Phase = "committed"
	}
	f.mu.Unlock()
	if !ok {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, apiv1.UploadSessionCommitResponse{
		Node:     apiv1.Node{ID: "node-" + id, Path: session.Path, Size: session.Size, Type: "file"},
		Checksum: session.Checksum,
	})
}

func (f *fakeServer) handlePathPut(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	virtual := strings.TrimPrefix(r.URL.Path, "/v1/paths/")
	f.mu.Lock()
	f.pathPuts = append(f.pathPuts, virtual)
	f.mu.Unlock()
	writeJSON(w, http.StatusCreated, apiv1.Node{ID: "node-" + virtual, Path: virtual, Size: int64(len(body)), Type: "file"})
}

func (f *fakeServer) handleResolve(w http.ResponseWriter, r *http.Request) {
	var body apiv1.IndexResolveRequest
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.resolveCalls++
	f.mu.Unlock()
	items := make([]*apiv1.Node, 0, len(body.Paths))
	for _, p := range body.Paths {
		if f.resolveExisting[strings.Trim(p, "/")] {
			items = append(items, &apiv1.Node{ID: "existing", Path: p, Type: "file"})
			continue
		}
		items = append(items, nil)
	}
	writeJSON(w, http.StatusOK, apiv1.IndexResolveManyResponse{Items: items, Total: len(items)})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func payload(size int, seed byte) []byte {
	out := make([]byte, size)
	for i := range out {
		out[i] = seed + byte(i%251)
	}
	return out
}

func sourcesFromBytes(n, size int) ([]Source, [][]byte) {
	sources := make([]Source, 0, n)
	bodies := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		data := payload(size, byte(i))
		bodies = append(bodies, data)
		sources = append(sources, FromBytes(fmt.Sprintf("data/tree/file-%02d.bin", i), data))
	}
	return sources, bodies
}

func TestUploadCreatesSessionsInBatchesAndCommits(t *testing.T) {
	t.Parallel()
	server := newFakeServer()
	client := server.start(t)

	sources, bodies := sourcesFromBytes(5, 1024)
	res, err := Upload(context.Background(), client, sources, Options{
		Batch:       Batch{Size: 2, FlushInterval: 5 * time.Millisecond},
		Concurrency: Concurrency{Hash: 1, Create: 1, Files: 2, Segments: 2},
		// Fixtures are small, and small files now go direct by default. This
		// test is about the session machinery, so opt out of the shortcut.
		DirectThresholdBytes: -1,
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if res.Done != 5 || res.Failed != 0 {
		t.Fatalf("done=%d failed=%d", res.Done, res.Failed)
	}
	if res.TransferredBytes != int64(5*1024) {
		t.Fatalf("transferred=%d", res.TransferredBytes)
	}
	if len(server.committed) != 5 {
		t.Fatalf("committed=%d", len(server.committed))
	}
	if server.createCalls >= 5 {
		t.Fatalf("expected batched creates, got %d calls for 5 files", server.createCalls)
	}
	for key, got := range server.uploadedBody {
		index, err := strconv.Atoi(strings.Split(key, ":")[1])
		if err != nil || index != 0 {
			t.Fatalf("unexpected segment key %q", key)
		}
		var matched bool
		for _, want := range bodies {
			if string(got) == string(want) {
				matched = true
				break
			}
		}
		if !matched {
			t.Fatalf("segment %q body does not match any source", key)
		}
	}
	for i, file := range res.Files {
		if file.Err != nil || file.Node == nil {
			t.Fatalf("file %d: err=%v node=%v", i, file.Err, file.Node)
		}
		if file.Path != sources[i].Path {
			t.Fatalf("results are out of input order: %q != %q", file.Path, sources[i].Path)
		}
	}
}

func TestUploadSplitsLargeFilesIntoSegments(t *testing.T) {
	t.Parallel()
	server := newFakeServer()
	client := server.start(t)

	data := payload(5000, 7)
	res, err := Upload(context.Background(), client, []Source{FromBytes("data/tree/big.bin", data)}, Options{
		SegmentSize: 1024,
		Concurrency: Concurrency{Segments: 2},
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if res.Done != 1 {
		t.Fatalf("done=%d failed=%d first=%v", res.Done, res.Failed, res.Files[0].Err)
	}
	if server.putCalls != 5 {
		t.Fatalf("put calls=%d want 5", server.putCalls)
	}
	var assembled []byte
	for i := 0; i < 5; i++ {
		assembled = append(assembled, server.uploadedBody[fmt.Sprintf("upl_%032x:%d", 1, i)]...)
	}
	if string(assembled) != string(data) {
		t.Fatalf("reassembled segments differ from the source")
	}
}

func TestUploadUsesWholeFilePutForEmptyAndTinyFiles(t *testing.T) {
	t.Parallel()
	server := newFakeServer()
	client := server.start(t)

	sources := []Source{
		FromBytes("data/tree/empty.bin", nil),
		FromBytes("data/tree/tiny.bin", payload(64, 1)),
		FromBytes("data/tree/large.bin", payload(4096, 2)),
	}
	res, err := Upload(context.Background(), client, sources, Options{
		DirectThresholdBytes: 128,
		Concurrency:          Concurrency{Hash: 1},
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if res.Done != 3 {
		t.Fatalf("done=%d failed=%d", res.Done, res.Failed)
	}
	if len(server.pathPuts) != 2 {
		t.Fatalf("whole-file puts=%v want the empty and tiny file", server.pathPuts)
	}
	if len(server.committed) != 1 {
		t.Fatalf("sessions committed=%d want 1", len(server.committed))
	}
}

func TestUploadRetriesRetryableSegmentFailures(t *testing.T) {
	t.Parallel()
	server := newFakeServer()
	server.failSegmentOnce = http.StatusServiceUnavailable
	client := server.start(t)

	sources, _ := sourcesFromBytes(2, 512)
	res, err := Upload(context.Background(), client, sources, Options{
		Retry: Retry{Attempts: 3, Base: time.Millisecond, Max: 5 * time.Millisecond},
		// Fixtures are small, and small files now go direct by default. This
		// test is about the session machinery, so opt out of the shortcut.
		DirectThresholdBytes: -1,
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if res.Done != 2 || res.Failed != 0 {
		t.Fatalf("done=%d failed=%d first=%v", res.Done, res.Failed, res.Files[0].Err)
	}
	if server.putCalls != 4 {
		t.Fatalf("put calls=%d want 4 (one failure plus one retry per file)", server.putCalls)
	}
}

func TestUploadDoesNotRetryClientErrors(t *testing.T) {
	t.Parallel()
	server := newFakeServer()
	server.failCreate = http.StatusConflict
	client := server.start(t)

	sources, _ := sourcesFromBytes(1, 512)
	res, err := Upload(context.Background(), client, sources, Options{
		Retry: Retry{Attempts: 4, Base: time.Millisecond, Max: 5 * time.Millisecond},
		// Fixtures are small, and small files now go direct by default. This
		// test is about the session machinery, so opt out of the shortcut.
		DirectThresholdBytes: -1,
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if res.Failed != 1 {
		t.Fatalf("failed=%d want 1", res.Failed)
	}
	if server.createCalls != 1 {
		t.Fatalf("create calls=%d want 1, a 409 must not be retried", server.createCalls)
	}
	var apiErr *filegate.APIError
	if !asAPIError(res.Files[0].Err, &apiErr) || apiErr.StatusCode != http.StatusConflict {
		t.Fatalf("error=%v want a 409 APIError", res.Files[0].Err)
	}
}

func asAPIError(err error, target **filegate.APIError) bool {
	casted, ok := err.(*filegate.APIError)
	if ok {
		*target = casted
	}
	return ok
}

func TestUploadResumesInProgressSession(t *testing.T) {
	t.Parallel()
	server := newFakeServer()
	client := server.start(t)

	data := payload(3000, 9)
	src := FromBytes("data/tree/resume.bin", data)

	// Seed the state an interrupted run would have left: a session with the
	// matching checksum and the first of three segments already stored.
	whole := sha256.Sum256(data)
	checksum := "sha256:" + hex.EncodeToString(whole[:])
	seeded := server.newSession(apiv1.UploadSessionCreateRequest{
		Path: src.Path, Size: src.Size, Checksum: checksum, SegmentSize: 1024,
	})
	server.sessions[seeded.ID].UploadedSegments = []int{0}
	server.preloaded = []apiv1.UploadSessionSummary{{
		ID: seeded.ID, Path: src.Path, Size: src.Size, SegmentSize: 1024, Phase: "in_progress",
	}}

	res, err := Upload(context.Background(), client, []Source{src}, Options{
		SegmentSize: 1024,
		Resume:      true,
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if res.Done != 1 {
		t.Fatalf("done=%d failed=%d err=%v", res.Done, res.Failed, res.Files[0].Err)
	}
	if !res.Files[0].Resumed {
		t.Fatalf("expected the file to be marked resumed")
	}
	if server.createCalls != 0 {
		t.Fatalf("create calls=%d want 0, the session should have been adopted", server.createCalls)
	}
	if server.putCalls != 2 {
		t.Fatalf("put calls=%d want 2, segment 0 was already uploaded", server.putCalls)
	}
	if res.TransferredBytes != src.Size {
		t.Fatalf("transferred=%d want %d including the resumed segment", res.TransferredBytes, src.Size)
	}
}

func TestUploadIgnoresSessionsForDifferentContent(t *testing.T) {
	t.Parallel()
	server := newFakeServer()
	client := server.start(t)

	src := FromBytes("data/tree/resume.bin", payload(3000, 9))
	stale := server.newSession(apiv1.UploadSessionCreateRequest{
		Path: src.Path, Size: src.Size, Checksum: "sha256:" + strings.Repeat("0", 64), SegmentSize: 1024,
	})
	server.preloaded = []apiv1.UploadSessionSummary{{
		ID: stale.ID, Path: src.Path, Size: src.Size, SegmentSize: 1024, Phase: "in_progress",
	}}

	res, err := Upload(context.Background(), client, []Source{src}, Options{SegmentSize: 1024, Resume: true})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if res.Done != 1 || res.Files[0].Resumed {
		t.Fatalf("done=%d resumed=%t, a checksum mismatch must not be adopted", res.Done, res.Files[0].Resumed)
	}
	if server.createCalls != 1 {
		t.Fatalf("create calls=%d want 1", server.createCalls)
	}
}

func TestUploadSkipsExistingPaths(t *testing.T) {
	t.Parallel()
	server := newFakeServer()
	server.resolveExisting["data/tree/file-01.bin"] = true
	client := server.start(t)

	sources, _ := sourcesFromBytes(3, 512)
	res, err := Upload(context.Background(), client, sources, Options{SkipExisting: true})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if res.Skipped != 1 || res.Done != 2 {
		t.Fatalf("skipped=%d done=%d", res.Skipped, res.Done)
	}
	if !res.Files[1].Skipped {
		t.Fatalf("expected file-01 to be the skipped one, got %+v", res.Files)
	}
	if server.resolveCalls != 1 {
		t.Fatalf("resolve calls=%d want 1 batched call", server.resolveCalls)
	}
}

func TestUploadBoundsSegmentConcurrencyGlobally(t *testing.T) {
	t.Parallel()
	server := newFakeServer()
	client := server.start(t)

	sources, _ := sourcesFromBytes(12, 4096)
	res, err := Upload(context.Background(), client, sources, Options{
		SegmentSize: 1024,
		Concurrency: Concurrency{Hash: 4, Create: 2, Files: 8, Segments: 3},
		Batch:       Batch{Size: 4, FlushInterval: 2 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if res.Done != 12 {
		t.Fatalf("done=%d failed=%d", res.Done, res.Failed)
	}
	if got := server.maxInFlight.Load(); got > 3 {
		t.Fatalf("max concurrent segment PUTs=%d want <= 3", got)
	}
}

func TestUploadReportsGlobalProgress(t *testing.T) {
	t.Parallel()
	server := newFakeServer()
	client := server.start(t)

	sources, _ := sourcesFromBytes(4, 2048)
	var mu sync.Mutex
	var kinds []EventType
	var lastTransferred int64
	res, err := Upload(context.Background(), client, sources, Options{
		OnEvent: func(e Event) {
			mu.Lock()
			defer mu.Unlock()
			kinds = append(kinds, e.Type)
			if e.Progress.TransferredBytes > lastTransferred {
				lastTransferred = e.Progress.TransferredBytes
			}
			if e.Progress.Files != 4 || e.Progress.TotalBytes != 4*2048 {
				t.Errorf("progress totals=%d/%d", e.Progress.Files, e.Progress.TotalBytes)
			}
		},
		// The event sequence under test is the session pipeline, which small
		// files now bypass by default.
		DirectThresholdBytes: -1,
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if res.Done != 4 {
		t.Fatalf("done=%d", res.Done)
	}
	if lastTransferred != 4*2048 {
		t.Fatalf("peak transferred=%d want %d", lastTransferred, 4*2048)
	}
	want := map[EventType]bool{EventStart: false, EventHashed: false, EventCreated: false, EventFileDone: false, EventFinished: false}
	for _, kind := range kinds {
		if _, ok := want[kind]; ok {
			want[kind] = true
		}
	}
	for kind, seen := range want {
		if !seen {
			t.Fatalf("missing %q event", kind)
		}
	}
}

func TestUploadStopsOnCancelledContext(t *testing.T) {
	t.Parallel()
	server := newFakeServer()
	client := server.start(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sources, _ := sourcesFromBytes(3, 512)
	res, err := Upload(ctx, client, sources, Options{})
	if err == nil {
		t.Fatalf("expected the cancelled context to surface")
	}
	if res.Done != 0 {
		t.Fatalf("done=%d want 0", res.Done)
	}
}

func TestFromDirMapsRelativePaths(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "nested", "deep"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("aa"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "deep", "b.txt"), []byte("bbbb"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	sources, err := FromDir(root, "data/upload")
	if err != nil {
		t.Fatalf("from dir: %v", err)
	}
	if len(sources) != 2 {
		t.Fatalf("sources=%d", len(sources))
	}
	if sources[0].Path != "data/upload/a.txt" || sources[0].Size != 2 {
		t.Fatalf("unexpected first source %+v", sources[0])
	}
	if sources[1].Path != "data/upload/nested/deep/b.txt" || sources[1].Size != 4 {
		t.Fatalf("unexpected second source %+v", sources[1])
	}

	reader, err := sources[1].OpenAt(1, 2)
	if err != nil {
		t.Fatalf("open at: %v", err)
	}
	defer reader.Close()
	got, _ := io.ReadAll(reader)
	if string(got) != "bb" {
		t.Fatalf("range read=%q", string(got))
	}
}

func TestUploadRejectsInvalidSources(t *testing.T) {
	t.Parallel()
	server := newFakeServer()
	client := server.start(t)

	if _, err := Upload(context.Background(), client, []Source{{Path: "", Size: 1}}, Options{}); err == nil {
		t.Fatalf("expected an error for an empty path")
	}
	if _, err := Upload(context.Background(), client, []Source{{Path: "data/x", Size: 1}}, Options{}); err == nil {
		t.Fatalf("expected an error for a missing OpenAt")
	}
	if _, err := Upload(context.Background(), nil, nil, Options{}); err == nil {
		t.Fatalf("expected an error for a nil client")
	}
}

// The shortcut is on by default, bounded by the segment size.
//
// A file that would have been a single segment gains nothing from a session:
// three requests instead of one, and resumability that amounts to retrying the
// same lone segment. The benchmark measured one-shot PUT at 2.5 to 3x the
// throughput of sessions for small files, so leaving the fast path opt-in made
// the slow path the default for the shape that dominates real trees.
func TestUploadDefaultsToWholeFilePutBelowSegmentSize(t *testing.T) {
	t.Parallel()
	server := newFakeServer()
	client := server.start(t)

	const segmentSize = 1024
	sources := []Source{
		FromBytes("data/tree/at-limit.bin", payload(segmentSize, 1)),
		FromBytes("data/tree/over-limit.bin", payload(segmentSize+1, 2)),
	}
	res, err := Upload(context.Background(), client, sources, Options{
		SegmentSize: segmentSize,
		Concurrency: Concurrency{Hash: 1},
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if res.Done != 2 || res.Failed != 0 {
		t.Fatalf("done=%d failed=%d", res.Done, res.Failed)
	}
	if len(server.pathPuts) != 1 || !strings.HasSuffix(server.pathPuts[0], "at-limit.bin") {
		t.Fatalf("whole-file puts=%v, want just the file at the segment size", server.pathPuts)
	}
	// Exactly one segment over the limit still earns a session, so the default
	// tracks the segment size rather than some unrelated constant.
	if len(server.committed) != 1 {
		t.Fatalf("sessions committed=%d want 1", len(server.committed))
	}
}

// A negative threshold is the opt-out, because the zero value has to keep
// meaning "use the defaults".
func TestUploadNegativeDirectThresholdForcesSessions(t *testing.T) {
	t.Parallel()
	server := newFakeServer()
	client := server.start(t)

	sources, _ := sourcesFromBytes(3, 512)
	res, err := Upload(context.Background(), client, sources, Options{
		DirectThresholdBytes: -1,
		Concurrency:          Concurrency{Hash: 1},
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if res.Done != 3 || res.Failed != 0 {
		t.Fatalf("done=%d failed=%d", res.Done, res.Failed)
	}
	if len(server.pathPuts) != 0 {
		t.Fatalf("whole-file puts=%v, want none", server.pathPuts)
	}
	if len(server.committed) != 3 {
		t.Fatalf("sessions committed=%d want 3", len(server.committed))
	}
}

// An empty file has no session to take: creating one requires size > 0. This
// stays true whatever the threshold is set to.
func TestUploadEmptyFileBypassesSessionsEvenWhenOptedOut(t *testing.T) {
	t.Parallel()
	server := newFakeServer()
	client := server.start(t)

	res, err := Upload(context.Background(), client, []Source{FromBytes("data/tree/empty.bin", nil)}, Options{
		DirectThresholdBytes: -1,
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if res.Done != 1 {
		t.Fatalf("done=%d failed=%d", res.Done, res.Failed)
	}
	if len(server.pathPuts) != 1 {
		t.Fatalf("whole-file puts=%v want the empty file", server.pathPuts)
	}
}
