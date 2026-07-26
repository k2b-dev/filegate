package main

// Tree mode answers the question the smart-batched-uploads design ticket asks:
// how long does uploading a whole folder of many small files take, and which
// lever actually moves that number. The load-generator mode in main.go measures
// steady-state ops/s for a single request shape; that cannot show per-file
// overhead, session scheduling, or batched session creation, because it never
// runs a corpus to completion.
//
// Everything here is measured wall-clock to the last commit, so hashing,
// session creation, segment PUTs and commits all land in the same number a
// user would see in the admin UI.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	apiv1 "github.com/valentinkolb/filegate/api/v1"
)

type treeConfig struct {
	BaseURL   string
	Token     string
	PathBase  string
	Timeout   time.Duration
	OutputCSV string

	Shape     string
	Scale     float64
	Seed      int64
	Label     string
	Transport string

	FileConcurrency    int
	HashConcurrency    int
	SegmentConcurrency int
	CreateConcurrency  int
	SegmentSize        int64
	BatchSize          int
	FlushMs            int

	KeepAlive bool
	HTTP2     bool
	Insecure  bool
}

type treeFile struct {
	Index int
	Path  string
	Size  int64
	Seed  int64
}

type hashedFile struct {
	File     treeFile
	Checksum string
}

type sessionWork struct {
	File    treeFile
	Session apiv1.UploadSessionResponse
}

// treePhases accumulates where wall time went. Sums across workers exceed the
// wall clock when stages overlap; the ratio between them is what matters.
type treePhases struct {
	genNs    atomic.Int64
	hashNs   atomic.Int64
	createNs atomic.Int64
	putNs    atomic.Int64
	commitNs atomic.Int64

	createCalls atomic.Int64
	putCalls    atomic.Int64
}

type treeResult struct {
	Label     string
	Shape     string
	Transport string

	Files int
	Bytes int64

	FileConcurrency    int
	SegmentConcurrency int
	SegmentSize        int64
	BatchSize          int
	KeepAlive          bool
	HTTP2              bool

	Wall     time.Duration
	Errors   int64
	FirstErr string
	Proto    string

	FilesPerSec float64
	MiBPerSec   float64

	P50Ms float64
	P95Ms float64
	P99Ms float64

	GenMs    float64
	HashMs   float64
	CreateMs float64
	PutMs    float64
	CommitMs float64

	HTTPRequests int64
}

func validateTreeConfig(cfg treeConfig) error {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return errors.New("base-url required")
	}
	switch cfg.Shape {
	case "photos", "logs", "node-modules":
	default:
		return fmt.Errorf("unsupported tree shape %q", cfg.Shape)
	}
	switch cfg.Transport {
	case "put", "direct", "session":
	default:
		return fmt.Errorf("unsupported tree transport %q", cfg.Transport)
	}
	if cfg.Scale <= 0 {
		return errors.New("tree-scale must be > 0")
	}
	if cfg.FileConcurrency <= 0 || cfg.HashConcurrency <= 0 || cfg.SegmentConcurrency <= 0 || cfg.CreateConcurrency <= 0 {
		return errors.New("concurrency values must be > 0")
	}
	if cfg.SegmentSize <= 0 {
		return errors.New("tree-segment-size must be > 0")
	}
	if cfg.BatchSize <= 0 {
		return errors.New("tree-batch must be > 0")
	}
	return nil
}

func runTree(cfg treeConfig) (treeResult, error) {
	files := planTree(cfg)
	if len(files) == 0 {
		return treeResult{}, errors.New("empty corpus")
	}
	var totalBytes int64
	for _, f := range files {
		totalBytes += f.Size
	}

	client := treeHTTPClient(cfg)
	defer client.CloseIdleConnections()

	var requests atomic.Int64
	phases := &treePhases{}
	var errCount atomic.Int64
	var firstErr atomic.Value

	fail := func(err error) {
		errCount.Add(1)
		firstErr.CompareAndSwap(nil, err.Error())
	}

	latencies := make([]int64, len(files))
	ctx := context.Background()

	start := time.Now()
	switch cfg.Transport {
	case "put", "direct":
		runWholeFileTree(ctx, cfg, client, files, latencies, phases, &requests, fail)
	case "session":
		runSessionTree(ctx, cfg, client, files, latencies, phases, &requests, fail)
	}
	wall := time.Since(start)

	sorted := make([]int64, 0, len(latencies))
	for _, v := range latencies {
		if v > 0 {
			sorted = append(sorted, v)
		}
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	res := treeResult{
		Label:              cfg.Label,
		Shape:              cfg.Shape,
		Transport:          cfg.Transport,
		Files:              len(files),
		Bytes:              totalBytes,
		FileConcurrency:    cfg.FileConcurrency,
		SegmentConcurrency: cfg.SegmentConcurrency,
		SegmentSize:        cfg.SegmentSize,
		BatchSize:          cfg.BatchSize,
		KeepAlive:          cfg.KeepAlive,
		HTTP2:              cfg.HTTP2,
		Wall:               wall,
		Errors:             errCount.Load(),
		HTTPRequests:       requests.Load(),
		GenMs:              msOf(phases.genNs.Load()),
		HashMs:             msOf(phases.hashNs.Load()),
		CreateMs:           msOf(phases.createNs.Load()),
		PutMs:              msOf(phases.putNs.Load()),
		CommitMs:           msOf(phases.commitNs.Load()),
	}
	if s, ok := firstErr.Load().(string); ok {
		res.FirstErr = s
	}
	if s, ok := observedProto.Load().(string); ok {
		res.Proto = s
	}
	if wall > 0 {
		res.FilesPerSec = float64(len(files)) / wall.Seconds()
		res.MiBPerSec = (float64(totalBytes) / (1024 * 1024)) / wall.Seconds()
	}
	if len(sorted) > 0 {
		res.P50Ms = float64(percentile(sorted, 0.50)) / 1000.0
		res.P95Ms = float64(percentile(sorted, 0.95)) / 1000.0
		res.P99Ms = float64(percentile(sorted, 0.99)) / 1000.0
	}
	return res, nil
}

func msOf(ns int64) float64 { return float64(ns) / float64(time.Millisecond) }

// treeHTTPClient isolates the transport levers the ticket names: keep-alive and
// HTTP/2. HTTP/2 needs TLS here because the Filegate listener speaks cleartext
// HTTP/1.1 only, so h2 runs are pointed at a TLS reverse proxy.
func treeHTTPClient(cfg treeConfig) *http.Client {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          4096,
		MaxIdleConnsPerHost:   4096,
		MaxConnsPerHost:       0,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true,
		DisableKeepAlives:     !cfg.KeepAlive,
		ForceAttemptHTTP2:     cfg.HTTP2,
	}
	if cfg.Insecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	if !cfg.HTTP2 {
		// A non-nil (even empty) TLSNextProto map is the documented way to
		// keep net/http from negotiating h2 over TLS.
		transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}
	return &http.Client{Transport: transport, Timeout: cfg.Timeout}
}

// planTree builds the corpus shape without touching the disk. Sizes come from a
// seeded RNG so every run of a given shape/scale/seed uploads exactly the same
// bytes, which is what makes lever comparisons meaningful.
func planTree(cfg treeConfig) []treeFile {
	rng := rand.New(rand.NewSource(cfg.Seed))
	base := strings.Trim(strings.TrimSpace(cfg.PathBase), "/")
	root := fmt.Sprintf("%s/%s-%d", base, cfg.Shape, time.Now().UnixNano())

	var count int
	switch cfg.Shape {
	case "photos":
		count = int(600 * cfg.Scale)
	case "logs":
		count = int(20000 * cfg.Scale)
	case "node-modules":
		count = int(20000 * cfg.Scale)
	}
	if count < 1 {
		count = 1
	}

	pkgNames := make([]string, 0, 512)
	if cfg.Shape == "node-modules" {
		for i := 0; i < 512; i++ {
			pkgNames = append(pkgNames, fmt.Sprintf("pkg-%03d", i))
		}
	}
	subdirs := []string{"dist", "lib", "src", "esm", "cjs"}

	files := make([]treeFile, 0, count)
	for i := 0; i < count; i++ {
		var size int64
		var path string
		switch cfg.Shape {
		case "photos":
			// A real photo folder is mostly JPGs with a heavy DNG tail.
			if rng.Float64() < 0.3 {
				size = 25<<20 + rng.Int63n(20<<20)
			} else {
				size = 4<<20 + rng.Int63n(4<<20)
			}
			path = fmt.Sprintf("%s/%03d/IMG_%05d.bin", root, i/100, i)
		case "logs":
			size = 1024 + rng.Int63n(31*1024)
			path = fmt.Sprintf("%s/2026-07-%02d/host-%02d/app-%06d.log", root, 1+i%28, i%16, i)
		case "node-modules":
			r := rng.Float64()
			switch {
			case r < 0.85:
				size = 200 + rng.Int63n(4<<10)
			case r < 0.98:
				size = 4<<10 + rng.Int63n(36<<10)
			default:
				size = 40<<10 + rng.Int63n(360<<10)
			}
			pkg := pkgNames[i%len(pkgNames)]
			sub := subdirs[(i/len(pkgNames))%len(subdirs)]
			path = fmt.Sprintf("%s/node_modules/%s/node_modules/%s/%s/%s/index-%06d.js",
				root, pkg, pkgNames[(i*7)%len(pkgNames)], sub, subdirs[(i*3)%len(subdirs)], i)
		}
		files = append(files, treeFile{Index: i, Path: path, Size: size, Seed: rng.Int63()})
	}
	return files
}

// entropy is a shared random block; per-file payloads are slices of it rotated
// by the file seed. Real content, no per-byte RNG cost in the hot path.
var entropy = func() []byte {
	buf := make([]byte, 8<<20)
	rng := rand.New(rand.NewSource(0x5eed))
	for i := range buf {
		buf[i] = byte(rng.Intn(256))
	}
	return buf
}()

func genPayload(f treeFile) []byte {
	out := make([]byte, f.Size)
	off := int(uint64(f.Seed) % uint64(len(entropy)))
	for filled := int64(0); filled < f.Size; {
		n := copy(out[filled:], entropy[off:])
		if n == 0 {
			off = 0
			continue
		}
		filled += int64(n)
		off = 0
	}
	return out
}

func runWholeFileTree(
	ctx context.Context,
	cfg treeConfig,
	client *http.Client,
	files []treeFile,
	latencies []int64,
	phases *treePhases,
	requests *atomic.Int64,
	fail func(error),
) {
	work := make(chan treeFile)
	var wg sync.WaitGroup
	for i := 0; i < cfg.FileConcurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range work {
				started := time.Now()
				genStart := time.Now()
				payload := genPayload(f)
				phases.genNs.Add(int64(time.Since(genStart)))

				var err error
				if cfg.Transport == "put" {
					err = treePutPath(ctx, cfg, client, requests, f, payload)
				} else {
					err = treeDirectUpload(ctx, cfg, client, requests, phases, f, payload)
				}
				if err != nil {
					fail(err)
					continue
				}
				phases.putNs.Add(int64(time.Since(started)))
				latencies[f.Index] = time.Since(started).Microseconds()
			}
		}()
	}
	for _, f := range files {
		work <- f
	}
	close(work)
	wg.Wait()
}

func treePutPath(ctx context.Context, cfg treeConfig, client *http.Client, requests *atomic.Int64, f treeFile, payload []byte) error {
	url := strings.TrimRight(cfg.BaseURL, "/") + "/v1/paths/" + mustEncodePath(f.Path) + "?onConflict=overwrite"
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(payload))
	req.Header.Set("Content-Type", "application/octet-stream")
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	requests.Add(1)
	return doDiscard(client, req)
}

func treeDirectUpload(ctx context.Context, cfg treeConfig, client *http.Client, requests *atomic.Int64, phases *treePhases, f treeFile, payload []byte) error {
	mintStart := time.Now()
	body, err := json.Marshal(apiv1.DirectUploadURLRequest{
		Path:        f.Path,
		ContentType: "application/octet-stream",
		OnConflict:  "overwrite",
	})
	if err != nil {
		return err
	}
	var minted apiv1.DirectUploadURLResponse
	if err := treeJSON(ctx, cfg, client, requests, http.MethodPost, "/v1/uploads/direct", body, &minted); err != nil {
		return err
	}
	phases.createNs.Add(int64(time.Since(mintStart)))
	phases.createCalls.Add(1)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, minted.UploadURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(payload))
	req.Header.Set("Content-Type", "application/octet-stream")
	requests.Add(1)
	return doDiscard(client, req)
}

func runSessionTree(
	ctx context.Context,
	cfg treeConfig,
	client *http.Client,
	files []treeFile,
	latencies []int64,
	phases *treePhases,
	requests *atomic.Int64,
	fail func(error),
) {
	hashed := make(chan hashedFile, cfg.BatchSize*2)
	batches := make(chan []hashedFile, cfg.CreateConcurrency*2)
	sessions := make(chan sessionWork, cfg.FileConcurrency*2)
	segmentSlots := make(chan struct{}, cfg.SegmentConcurrency)
	starts := make([]time.Time, len(files))

	// Hash stage. Sessions need size and checksum before the server will
	// hand out a plan, so this is unavoidable client work and is measured.
	var hashWG sync.WaitGroup
	hashIn := make(chan treeFile)
	for i := 0; i < cfg.HashConcurrency; i++ {
		hashWG.Add(1)
		go func() {
			defer hashWG.Done()
			for f := range hashIn {
				starts[f.Index] = time.Now()
				genStart := time.Now()
				payload := genPayload(f)
				phases.genNs.Add(int64(time.Since(genStart)))
				hashStart := time.Now()
				sum := sha256.Sum256(payload)
				phases.hashNs.Add(int64(time.Since(hashStart)))
				hashed <- hashedFile{File: f, Checksum: "sha256:" + hex.EncodeToString(sum[:])}
			}
		}()
	}

	// Batcher. Groups hashed files into one sessions:batch call, flushing on
	// a short window so a slow hash stage cannot stall session creation.
	var batchWG sync.WaitGroup
	batchWG.Add(1)
	go func() {
		defer batchWG.Done()
		defer close(batches)
		pending := make([]hashedFile, 0, cfg.BatchSize)
		flush := time.NewTimer(time.Duration(cfg.FlushMs) * time.Millisecond)
		defer flush.Stop()
		if !flush.Stop() {
			<-flush.C
		}
		for {
			select {
			case item, ok := <-hashed:
				if !ok {
					if len(pending) > 0 {
						batches <- pending
					}
					return
				}
				pending = append(pending, item)
				if len(pending) >= cfg.BatchSize {
					batches <- pending
					pending = make([]hashedFile, 0, cfg.BatchSize)
					flush.Stop()
					continue
				}
				if len(pending) == 1 && cfg.FlushMs > 0 {
					flush.Reset(time.Duration(cfg.FlushMs) * time.Millisecond)
				}
			case <-flush.C:
				if len(pending) > 0 {
					batches <- pending
					pending = make([]hashedFile, 0, cfg.BatchSize)
				}
			}
		}
	}()

	// Session creation stage.
	var createWG sync.WaitGroup
	for i := 0; i < cfg.CreateConcurrency; i++ {
		createWG.Add(1)
		go func() {
			defer createWG.Done()
			for batch := range batches {
				created, err := createSessions(ctx, cfg, client, requests, phases, batch)
				if err != nil {
					fail(err)
					continue
				}
				for _, sw := range created {
					sessions <- sw
				}
			}
		}()
	}

	// Upload + commit stage.
	var uploadWG sync.WaitGroup
	for i := 0; i < cfg.FileConcurrency; i++ {
		uploadWG.Add(1)
		go func() {
			defer uploadWG.Done()
			for sw := range sessions {
				if err := uploadSession(ctx, cfg, client, requests, phases, segmentSlots, sw); err != nil {
					fail(err)
					continue
				}
				latencies[sw.File.Index] = time.Since(starts[sw.File.Index]).Microseconds()
			}
		}()
	}

	for _, f := range files {
		hashIn <- f
	}
	close(hashIn)
	hashWG.Wait()
	close(hashed)
	batchWG.Wait()
	createWG.Wait()
	close(sessions)
	uploadWG.Wait()
}

func createSessions(
	ctx context.Context,
	cfg treeConfig,
	client *http.Client,
	requests *atomic.Int64,
	phases *treePhases,
	batch []hashedFile,
) ([]sessionWork, error) {
	started := time.Now()
	defer func() {
		phases.createNs.Add(int64(time.Since(started)))
		phases.createCalls.Add(1)
	}()

	uploads := make([]apiv1.UploadSessionCreateRequest, 0, len(batch))
	for _, item := range batch {
		uploads = append(uploads, apiv1.UploadSessionCreateRequest{
			Path:        item.File.Path,
			Size:        item.File.Size,
			Checksum:    item.Checksum,
			SegmentSize: cfg.SegmentSize,
			ContentType: "application/octet-stream",
			OnConflict:  "overwrite",
		})
	}

	if len(batch) == 1 {
		var out apiv1.UploadSessionResponse
		body, err := json.Marshal(uploads[0])
		if err != nil {
			return nil, err
		}
		if err := treeJSON(ctx, cfg, client, requests, http.MethodPost, "/v1/uploads/sessions", body, &out); err != nil {
			return nil, err
		}
		return []sessionWork{{File: batch[0].File, Session: out}}, nil
	}

	body, err := json.Marshal(apiv1.UploadSessionBatchCreateRequest{Uploads: uploads})
	if err != nil {
		return nil, err
	}
	var out apiv1.UploadSessionBatchCreateResponse
	if err := treeJSON(ctx, cfg, client, requests, http.MethodPost, "/v1/uploads/sessions:batch", body, &out); err != nil {
		return nil, err
	}
	if len(out.Sessions) != len(batch) {
		return nil, fmt.Errorf("batch create returned %d sessions for %d uploads", len(out.Sessions), len(batch))
	}
	work := make([]sessionWork, 0, len(batch))
	for i := range batch {
		work = append(work, sessionWork{File: batch[i].File, Session: out.Sessions[i]})
	}
	return work, nil
}

func uploadSession(
	ctx context.Context,
	cfg treeConfig,
	client *http.Client,
	requests *atomic.Int64,
	phases *treePhases,
	segmentSlots chan struct{},
	sw sessionWork,
) error {
	payload := genPayload(sw.File)
	putStart := time.Now()
	var wg sync.WaitGroup
	var firstErr atomic.Value
	for _, segment := range sw.Session.Segments {
		wg.Add(1)
		go func(segment apiv1.UploadSessionSegment) {
			defer wg.Done()
			segmentSlots <- struct{}{}
			defer func() { <-segmentSlots }()
			end := segment.Offset + segment.Size
			url := fmt.Sprintf("%s/v1/uploads/sessions/%s/segments/%d",
				strings.TrimRight(cfg.BaseURL, "/"), sw.Session.ID, segment.Index)
			req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(payload[segment.Offset:end]))
			if err != nil {
				firstErr.CompareAndSwap(nil, err)
				return
			}
			req.ContentLength = segment.Size
			req.Header.Set("Content-Type", "application/octet-stream")
			if cfg.Token != "" {
				req.Header.Set("Authorization", "Bearer "+cfg.Token)
			}
			requests.Add(1)
			phases.putCalls.Add(1)
			if err := doDiscard(client, req); err != nil {
				firstErr.CompareAndSwap(nil, err)
			}
		}(segment)
	}
	wg.Wait()
	phases.putNs.Add(int64(time.Since(putStart)))
	if err, ok := firstErr.Load().(error); ok {
		return err
	}

	commitStart := time.Now()
	var out apiv1.UploadSessionCommitResponse
	err := treeJSON(ctx, cfg, client, requests, http.MethodPost,
		"/v1/uploads/sessions/"+sw.Session.ID+"/commit", nil, &out)
	phases.commitNs.Add(int64(time.Since(commitStart)))
	return err
}

func treeJSON(ctx context.Context, cfg treeConfig, client *http.Client, requests *atomic.Int64, method, endpoint string, body []byte, out any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(cfg.BaseURL, "/")+endpoint, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.ContentLength = int64(len(body))
		req.Header.Set("Content-Type", "application/json")
	}
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	requests.Add(1)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	noteProto(resp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: status %d: %s", method, endpoint, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// observedProto records the wire protocol actually negotiated. One process runs
// exactly one benchmark, so a package-level value is enough, and it turns the
// h1-vs-h2 lever into evidence instead of an assumption about ALPN.
var observedProto atomic.Value

func noteProto(resp *http.Response) {
	observedProto.CompareAndSwap(nil, resp.Proto)
}

func doDiscard(client *http.Client, req *http.Request) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	noteProto(resp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: status %d: %s", req.Method, req.URL.Path, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}

func printTreeResult(r treeResult) {
	fmt.Printf(
		"label=%s shape=%s transport=%s files=%d bytes=%d conc=%d seg_conc=%d seg_size=%d batch=%d keepalive=%t http2=%t proto=%s "+
			"wall_s=%.2f files_s=%.1f mib_s=%.1f errors=%d p50_ms=%.1f p95_ms=%.1f p99_ms=%.1f "+
			"gen_ms=%.0f hash_ms=%.0f create_ms=%.0f put_ms=%.0f commit_ms=%.0f reqs=%d%s\n",
		r.Label, r.Shape, r.Transport, r.Files, r.Bytes, r.FileConcurrency, r.SegmentConcurrency, r.SegmentSize,
		r.BatchSize, r.KeepAlive, r.HTTP2, r.Proto, r.Wall.Seconds(), r.FilesPerSec, r.MiBPerSec, r.Errors,
		r.P50Ms, r.P95Ms, r.P99Ms, r.GenMs, r.HashMs, r.CreateMs, r.PutMs, r.CommitMs, r.HTTPRequests,
		firstErrSuffix(r.FirstErr),
	)
}

func firstErrSuffix(msg string) string {
	if msg == "" {
		return ""
	}
	return " first_error=" + strconv.Quote(msg)
}

func appendTreeCSV(path string, r treeResult) error {
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	writeHeader := false
	if st, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		writeHeader = true
	} else if st.Size() == 0 {
		writeHeader = true
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	if writeHeader {
		if err := w.Write([]string{
			"label", "shape", "transport", "files", "bytes", "file_concurrency", "segment_concurrency",
			"segment_size", "batch_size", "keepalive", "http2", "proto", "wall_ms", "files_per_sec", "mib_per_sec",
			"errors", "p50_ms", "p95_ms", "p99_ms", "gen_ms", "hash_ms", "create_ms", "put_ms", "commit_ms",
			"http_requests", "first_error",
		}); err != nil {
			return err
		}
	}
	row := []string{
		r.Label, r.Shape, r.Transport,
		strconv.Itoa(r.Files), strconv.FormatInt(r.Bytes, 10),
		strconv.Itoa(r.FileConcurrency), strconv.Itoa(r.SegmentConcurrency),
		strconv.FormatInt(r.SegmentSize, 10), strconv.Itoa(r.BatchSize),
		strconv.FormatBool(r.KeepAlive), strconv.FormatBool(r.HTTP2), r.Proto,
		strconv.FormatInt(r.Wall.Milliseconds(), 10),
		fmt.Sprintf("%.4f", r.FilesPerSec), fmt.Sprintf("%.4f", r.MiBPerSec),
		strconv.FormatInt(r.Errors, 10),
		fmt.Sprintf("%.4f", r.P50Ms), fmt.Sprintf("%.4f", r.P95Ms), fmt.Sprintf("%.4f", r.P99Ms),
		fmt.Sprintf("%.1f", r.GenMs), fmt.Sprintf("%.1f", r.HashMs), fmt.Sprintf("%.1f", r.CreateMs),
		fmt.Sprintf("%.1f", r.PutMs), fmt.Sprintf("%.1f", r.CommitMs),
		strconv.FormatInt(r.HTTPRequests, 10), r.FirstErr,
	}
	if err := w.Write(row); err != nil {
		return err
	}
	w.Flush()
	return w.Error()
}
