package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type config struct {
	Mode      string
	BaseURL   string
	Token     string
	Scenario  string
	Clients   int
	Duration  time.Duration
	Timeout   time.Duration
	PathBase  string
	OutputCSV string
	SampleCap int

	Tree treeConfig
}

type runResult struct {
	Scenario string
	Clients  int

	Duration time.Duration
	Total    int64
	Success  int64
	Errors   int64
	Bytes    int64

	AvgMs float64
	P50Ms float64
	P95Ms float64
	P99Ms float64

	OpsPerSec   float64
	MbitPerSec  float64
	ErrorRatePc float64
}

type workerStats struct {
	Total   int64
	Success int64
	Errors  int64
	Bytes   int64
	LatSum  int64
	Samples []int64
}

func main() {
	cfg := parseFlags()

	if cfg.Mode == "tree" {
		if err := validateTreeConfig(cfg.Tree); err != nil {
			fatalf("invalid config: %v", err)
		}
		result, err := runTree(cfg.Tree)
		if err != nil {
			fatalf("benchmark failed: %v", err)
		}
		printTreeResult(result)
		if cfg.Tree.OutputCSV != "" {
			if err := appendTreeCSV(cfg.Tree.OutputCSV, result); err != nil {
				fatalf("write csv: %v", err)
			}
		}
		return
	}

	if err := validateConfig(cfg); err != nil {
		fatalf("invalid config: %v", err)
	}

	result, err := run(cfg)
	if err != nil {
		fatalf("benchmark failed: %v", err)
	}

	printResult(result)
	if cfg.OutputCSV != "" {
		if err := appendCSV(cfg.OutputCSV, result); err != nil {
			fatalf("write csv: %v", err)
		}
	}
}

func parseFlags() config {
	var cfg config
	flag.StringVar(&cfg.Mode, "mode", "loadgen", "mode: loadgen (steady-state request rate)|tree (upload a whole corpus once)")
	flag.StringVar(&cfg.BaseURL, "base-url", "http://127.0.0.1:8080", "filegate base URL")
	flag.StringVar(&cfg.Token, "token", "", "bearer token")
	flag.StringVar(&cfg.Scenario, "scenario", "metadata-path", "scenario: metadata-path|metadata-id|read-4k|read-1m|write-4k|write-1m|mixed")
	flag.IntVar(&cfg.Clients, "clients", 8, "parallel clients")
	flag.DurationVar(&cfg.Duration, "duration", 20*time.Second, "benchmark duration")
	flag.DurationVar(&cfg.Timeout, "timeout", 30*time.Second, "request timeout")
	flag.StringVar(&cfg.PathBase, "path-base", "data/bench", "virtual base path for benchmark assets")
	flag.StringVar(&cfg.OutputCSV, "output-csv", "", "append result row to CSV file")
	flag.IntVar(&cfg.SampleCap, "sample-cap", 200000, "max latency samples used for percentile estimates")

	flag.StringVar(&cfg.Tree.Shape, "tree-shape", "logs", "tree corpus shape: photos|logs|node-modules")
	flag.Float64Var(&cfg.Tree.Scale, "tree-scale", 1, "multiplier on the shape's default file count")
	flag.Int64Var(&cfg.Tree.Seed, "tree-seed", 42, "corpus RNG seed; equal seeds upload identical bytes")
	flag.StringVar(&cfg.Tree.Label, "tree-label", "", "free-form label recorded in the CSV row")
	flag.StringVar(&cfg.Tree.Transport, "tree-transport", "session", "upload path: put|direct|session")
	flag.IntVar(&cfg.Tree.FileConcurrency, "tree-files", 8, "files uploaded in parallel")
	flag.IntVar(&cfg.Tree.HashConcurrency, "tree-hash", 4, "files hashed in parallel")
	flag.IntVar(&cfg.Tree.SegmentConcurrency, "tree-segments", 8, "global segment PUTs in flight")
	flag.IntVar(&cfg.Tree.CreateConcurrency, "tree-create", 4, "session-create calls in flight")
	flag.Int64Var(&cfg.Tree.SegmentSize, "tree-segment-size", 8<<20, "segment size in bytes")
	flag.IntVar(&cfg.Tree.BatchSize, "tree-batch", 32, "sessions created per request (1 disables :batch)")
	flag.IntVar(&cfg.Tree.FlushMs, "tree-flush-ms", 20, "batch flush window in milliseconds")
	flag.BoolVar(&cfg.Tree.KeepAlive, "keep-alive", true, "reuse connections")
	flag.BoolVar(&cfg.Tree.HTTP2, "http2", false, "negotiate HTTP/2 (requires a TLS endpoint)")
	flag.BoolVar(&cfg.Tree.Insecure, "insecure", false, "skip TLS verification (bench proxy uses a self-signed cert)")

	flag.Parse()

	cfg.Tree.BaseURL = cfg.BaseURL
	cfg.Tree.Token = cfg.Token
	cfg.Tree.PathBase = cfg.PathBase
	cfg.Tree.Timeout = cfg.Timeout
	cfg.Tree.OutputCSV = cfg.OutputCSV
	return cfg
}

func validateConfig(cfg config) error {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return errors.New("base-url required")
	}
	if cfg.Clients <= 0 {
		return errors.New("clients must be > 0")
	}
	if cfg.Duration <= 0 {
		return errors.New("duration must be > 0")
	}
	if cfg.Timeout <= 0 {
		return errors.New("timeout must be > 0")
	}
	if cfg.SampleCap < 1000 {
		return errors.New("sample-cap must be >= 1000")
	}
	s := strings.TrimSpace(cfg.Scenario)
	switch s {
	case "metadata-path", "metadata-id", "read-4k", "read-1m", "write-4k", "write-1m", "mixed":
		return nil
	default:
		return fmt.Errorf("unsupported scenario %q", cfg.Scenario)
	}
}

func run(cfg config) (runResult, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          65535,
		MaxIdleConnsPerHost:   65535,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true,
	}
	client := &http.Client{Transport: transport, Timeout: cfg.Timeout}

	ops, err := buildOps(client, baseURL, cfg)
	if err != nil {
		return runResult{}, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Duration)
	defer cancel()

	start := time.Now()
	results := make(chan workerStats, cfg.Clients)
	var seq uint64
	var wg sync.WaitGroup
	perWorkerCap := cfg.SampleCap / cfg.Clients
	if perWorkerCap < 1000 {
		perWorkerCap = 1000
	}

	for worker := 0; worker < cfg.Clients; worker++ {
		workerID := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			stats := runWorker(ctx, workerID, perWorkerCap, ops, &seq)
			results <- stats
		}()
	}

	wg.Wait()
	close(results)
	elapsed := time.Since(start)

	agg := workerStats{}
	allSamples := make([]int64, 0, cfg.SampleCap)
	for wr := range results {
		agg.Total += wr.Total
		agg.Success += wr.Success
		agg.Errors += wr.Errors
		agg.Bytes += wr.Bytes
		agg.LatSum += wr.LatSum
		allSamples = append(allSamples, wr.Samples...)
	}
	if len(allSamples) > cfg.SampleCap {
		rand.Shuffle(len(allSamples), func(i, j int) {
			allSamples[i], allSamples[j] = allSamples[j], allSamples[i]
		})
		allSamples = allSamples[:cfg.SampleCap]
	}
	sort.Slice(allSamples, func(i, j int) bool { return allSamples[i] < allSamples[j] })

	res := runResult{
		Scenario: cfg.Scenario,
		Clients:  cfg.Clients,
		Duration: elapsed,
		Total:    agg.Total,
		Success:  agg.Success,
		Errors:   agg.Errors,
		Bytes:    agg.Bytes,
	}
	if agg.Total > 0 {
		res.ErrorRatePc = 100 * float64(agg.Errors) / float64(agg.Total)
		res.OpsPerSec = float64(agg.Total) / elapsed.Seconds()
	}
	if agg.Success > 0 {
		res.AvgMs = float64(agg.LatSum) / float64(agg.Success) / float64(time.Millisecond)
	}
	if elapsed > 0 {
		res.MbitPerSec = (float64(agg.Bytes) * 8 / 1_000_000) / elapsed.Seconds()
	}
	if len(allSamples) > 0 {
		res.P50Ms = float64(percentile(allSamples, 0.50)) / 1000.0
		res.P95Ms = float64(percentile(allSamples, 0.95)) / 1000.0
		res.P99Ms = float64(percentile(allSamples, 0.99)) / 1000.0
	}
	return res, nil
}

func runWorker(ctx context.Context, workerID, sampleCap int, ops operations, seq *uint64) workerStats {
	rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(workerID)*1_000_003))
	stats := workerStats{Samples: make([]int64, 0, sampleCap)}

	for {
		select {
		case <-ctx.Done():
			return stats
		default:
		}

		idx := atomic.AddUint64(seq, 1)
		start := time.Now()
		bytes, err := ops.exec(workerID, idx, rng)
		lat := time.Since(start)

		stats.Total++
		if err != nil {
			stats.Errors++
		} else {
			stats.Success++
			stats.Bytes += bytes
			stats.LatSum += lat.Nanoseconds()
		}
		addSample(&stats.Samples, sampleCap, stats.Total, lat.Microseconds(), rng)
	}
}

func addSample(samples *[]int64, cap int, seen int64, value int64, rng *rand.Rand) {
	if cap <= 0 {
		return
	}
	if len(*samples) < cap {
		*samples = append(*samples, value)
		return
	}
	j := rng.Int63n(seen)
	if j < int64(cap) {
		(*samples)[j] = value
	}
}

type operations struct {
	metadataPath func() (int64, error)
	metadataID   func() (int64, error)
	read4k       func() (int64, error)
	read1m       func() (int64, error)
	write4k      func(worker int, seq uint64) (int64, error)
	write1m      func(worker int, seq uint64) (int64, error)
	mode         string
}

func (o operations) exec(worker int, seq uint64, rng *rand.Rand) (int64, error) {
	switch o.mode {
	case "metadata-path":
		return o.metadataPath()
	case "metadata-id":
		return o.metadataID()
	case "read-4k":
		return o.read4k()
	case "read-1m":
		return o.read1m()
	case "write-4k":
		return o.write4k(worker, seq)
	case "write-1m":
		return o.write1m(worker, seq)
	case "mixed":
		n := rng.Intn(100)
		if n < 60 {
			return o.metadataPath()
		}
		if n < 80 {
			return o.read1m()
		}
		if n < 90 {
			return o.read4k()
		}
		return o.write4k(worker, seq)
	default:
		return 0, fmt.Errorf("unsupported scenario: %s", o.mode)
	}
}

func buildOps(client *http.Client, baseURL string, cfg config) (operations, error) {
	basePath := strings.Trim(strings.TrimSpace(cfg.PathBase), "/")
	if basePath == "" {
		return operations{}, errors.New("path-base cannot be empty")
	}
	path4k := basePath + "/fixtures/read-4k.bin"
	path1m := basePath + "/fixtures/read-1m.bin"

	id4k, err := resolveNodeID(client, baseURL, cfg.Token, path4k)
	if err != nil {
		return operations{}, fmt.Errorf("resolve id for %q: %w", path4k, err)
	}
	id1m, err := resolveNodeID(client, baseURL, cfg.Token, path1m)
	if err != nil {
		return operations{}, fmt.Errorf("resolve id for %q: %w", path1m, err)
	}

	payload4k := bytes.Repeat([]byte("A"), 4*1024)
	payload1m := bytes.Repeat([]byte("B"), 1*1024*1024)

	ops := operations{mode: cfg.Scenario}
	ops.metadataPath = func() (int64, error) {
		return doRequest(client, cfg.Token, http.MethodGet, baseURL+"/v1/paths/"+mustEncodePath(path4k), nil, "", 0)
	}
	ops.metadataID = func() (int64, error) {
		return doRequest(client, cfg.Token, http.MethodGet, baseURL+"/v1/nodes/"+url.PathEscape(id4k), nil, "", 0)
	}
	ops.read4k = func() (int64, error) {
		return doRequest(client, cfg.Token, http.MethodGet, baseURL+"/v1/nodes/"+url.PathEscape(id4k)+"/content", nil, "", 0)
	}
	ops.read1m = func() (int64, error) {
		return doRequest(client, cfg.Token, http.MethodGet, baseURL+"/v1/nodes/"+url.PathEscape(id1m)+"/content", nil, "", 0)
	}
	ops.write4k = func(worker int, seq uint64) (int64, error) {
		vp := fmt.Sprintf("%s/writes-4k/w%03d-%012d.bin", basePath, worker, seq)
		return doRequest(client, cfg.Token, http.MethodPut, baseURL+"/v1/paths/"+mustEncodePath(vp), bytes.NewReader(payload4k), "application/octet-stream", int64(len(payload4k)))
	}
	ops.write1m = func(worker int, seq uint64) (int64, error) {
		vp := fmt.Sprintf("%s/writes-1m/w%03d-%012d.bin", basePath, worker, seq)
		return doRequest(client, cfg.Token, http.MethodPut, baseURL+"/v1/paths/"+mustEncodePath(vp), bytes.NewReader(payload1m), "application/octet-stream", int64(len(payload1m)))
	}
	return ops, nil
}

func doRequest(client *http.Client, token, method, rawURL string, body io.Reader, contentType string, reqBytes int64) (int64, error) {
	req, err := http.NewRequest(method, rawURL, body)
	if err != nil {
		return 0, err
	}
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	respBytes, copyErr := io.Copy(io.Discard, resp.Body)
	if copyErr != nil {
		return 0, copyErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("status %d", resp.StatusCode)
	}
	return reqBytes + respBytes, nil
}

func resolveNodeID(client *http.Client, baseURL, token, virtualPath string) (string, error) {
	endpoint := baseURL + "/v1/paths/" + mustEncodePath(virtualPath)
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("status %d resolving id: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if strings.TrimSpace(out.ID) == "" {
		return "", errors.New("empty id in response")
	}
	return out.ID, nil
}

func mustEncodePath(virtualPath string) string {
	p := strings.Trim(strings.TrimSpace(virtualPath), "/")
	if p == "" {
		return ""
	}
	parts := strings.Split(p, "/")
	encoded := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		encoded = append(encoded, url.PathEscape(part))
	}
	return strings.Join(encoded, "/")
}

func percentile(sortedSamples []int64, q float64) int64 {
	if len(sortedSamples) == 0 {
		return 0
	}
	if q <= 0 {
		return sortedSamples[0]
	}
	if q >= 1 {
		return sortedSamples[len(sortedSamples)-1]
	}
	idx := int(float64(len(sortedSamples)-1) * q)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sortedSamples) {
		idx = len(sortedSamples) - 1
	}
	return sortedSamples[idx]
}

func printResult(r runResult) {
	fmt.Printf("scenario=%s clients=%d duration=%s total=%d success=%d errors=%d err_rate=%.2f%% ops_s=%.1f mbit_s=%.1f avg_ms=%.2f p50_ms=%.2f p95_ms=%.2f p99_ms=%.2f\n",
		r.Scenario,
		r.Clients,
		r.Duration.Round(time.Millisecond),
		r.Total,
		r.Success,
		r.Errors,
		r.ErrorRatePc,
		r.OpsPerSec,
		r.MbitPerSec,
		r.AvgMs,
		r.P50Ms,
		r.P95Ms,
		r.P99Ms,
	)
}

func appendCSV(path string, r runResult) error {
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
			"scenario", "clients", "duration_ms", "total", "success", "errors", "error_rate_percent",
			"ops_per_sec", "mbit_per_sec", "avg_ms", "p50_ms", "p95_ms", "p99_ms",
		}); err != nil {
			return err
		}
	}
	row := []string{
		r.Scenario,
		strconv.Itoa(r.Clients),
		strconv.FormatInt(r.Duration.Milliseconds(), 10),
		strconv.FormatInt(r.Total, 10),
		strconv.FormatInt(r.Success, 10),
		strconv.FormatInt(r.Errors, 10),
		fmt.Sprintf("%.4f", r.ErrorRatePc),
		fmt.Sprintf("%.4f", r.OpsPerSec),
		fmt.Sprintf("%.4f", r.MbitPerSec),
		fmt.Sprintf("%.4f", r.AvgMs),
		fmt.Sprintf("%.4f", r.P50Ms),
		fmt.Sprintf("%.4f", r.P95Ms),
		fmt.Sprintf("%.4f", r.P99Ms),
	}
	if err := w.Write(row); err != nil {
		return err
	}
	w.Flush()
	return w.Error()
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
