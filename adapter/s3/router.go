package s3

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/k2b-dev/filegate/v3/domain"
	"github.com/k2b-dev/filegate/v3/infra/activity"
	"github.com/k2b-dev/filegate/v3/infra/metrics"
)

// Options configures the S3 listener.
type Options struct {
	// Region is the AWS region the listener identifies as in SigV4.
	// Single-tenant deployments can keep the AWS default "us-east-1";
	// the value only needs to match what the client signs with.
	Region string
	// Keys is the multi-tenant key store. Each entry maps an access
	// key to its secret + per-key bucket whitelist. NewHandler folds
	// the legacy AccessKey/SecretKey fields below into this list at
	// construction time, so handlers consult exactly one source.
	Keys []KeyEntry
	// AccessKey + SecretKey are the legacy single-tenant convenience
	// knobs from M1. When set, NewHandler synthesizes a Keys entry
	// with full bucket access ("*"). Operators using Keys directly
	// can leave these empty.
	AccessKey string
	SecretKey string
	// AccessLogEnabled mirrors the REST adapter's setting.
	AccessLogEnabled bool
	// Metrics, when non-nil, receives the dedicated rate-limit
	// rejection counter. The generic HTTP RED metrics (which count
	// the 503 in the 5xx class) are recorded by the middleware in
	// cli/serve.go; this is the specific signal that distinguishes
	// rate-limit 503s from other 503s. nil-safe: a nil Registry's
	// methods are no-ops.
	Metrics *metrics.Registry
	// MaxConcurrentWrites bounds concurrent S3 object/part writes.
	// 0 means use the default. The limiter protects file descriptors
	// and multipart staging space when clients upload large folders.
	MaxConcurrentWrites int
	ActivityLog         *activity.Ring
}

// KeyEntry is the in-memory shape of one entry in the multi-tenant
// key store. Buckets is the per-key whitelist; the special wildcard
// "*" grants access to every configured mount. An empty slice
// denies all bucket access (the key authenticates but every
// operation returns AccessDenied).
//
// RequestsPerSecond + Burst configure a per-key token-bucket rate
// limit. RPS=0 (default) means unlimited — the limiter skips the
// bucket entirely. When RPS>0 and Burst is unset, Burst defaults
// to RPS (allowing one second's worth of bursty traffic). Over-
// limit requests get 503 SlowDown (the AWS-spec back-off code).
type KeyEntry struct {
	AccessKey         string
	SecretKey         string
	Buckets           []string
	RequestsPerSecond int
	Burst             int
}

// keyStore is the resolved-and-indexed view of Options.Keys used by
// the request path. Lookup is O(log n) via the map; the per-key
// whitelist is materialized into a set so bucket-membership checks
// don't allocate.
type keyStore struct {
	byAccessKey map[string]keyRecord
}

// keyRecord is the per-access-key bundle stored in keyStore. The
// secret is kept as the raw string for ConstantTimeCompare. Buckets
// is the set form of the whitelist; AllBuckets is true when the
// "*" wildcard was present (in which case Buckets is nil — no
// membership lookup needed).
type keyRecord struct {
	Secret     string
	Buckets    map[string]struct{}
	AllBuckets bool
}

// allowedBuckets returns the explicit list of buckets accessible to
// this key, restricted to what actually exists in mounts. When the
// key has the "*" wildcard, every mount name in mounts is returned.
// Used by ListBuckets to surface only the per-key-permitted set.
func (kr keyRecord) allowedBuckets(mounts []string) []string {
	out := make([]string, 0, len(mounts))
	for _, m := range mounts {
		if kr.AllBuckets {
			out = append(out, m)
			continue
		}
		if _, ok := kr.Buckets[m]; ok {
			out = append(out, m)
		}
	}
	return out
}

// canAccess reports whether this key may operate on the named
// bucket. The bucket is NOT checked for existence here — that's a
// separate concern; canAccess only cares about authorization.
func (kr keyRecord) canAccess(bucket string) bool {
	if kr.AllBuckets {
		return true
	}
	_, ok := kr.Buckets[bucket]
	return ok
}

// NewHandler returns the http.Handler for the S3 listener. Returns
// an error when Options is misconfigured: missing credentials, a
// duplicated access key, or a Keys entry whose bucket whitelist
// references a mount that doesn't exist.
// NewHandler builds the S3 adapter. The returned Handler exposes SetKeys so
// access keys can be changed while the service runs.
func NewHandler(svc *domain.Service, opts Options) (*Handler, error) {
	if opts.Region == "" {
		opts.Region = "us-east-1"
	}

	store, err := buildKeyStore(opts, svc)
	if err != nil {
		return nil, err
	}

	r := &router{
		svc:        svc,
		limiter:    newRateLimiter(opts.Keys),
		accessLog:  opts.AccessLogEnabled,
		metrics:    opts.Metrics,
		activity:   opts.ActivityLog,
		writeSlots: make(chan struct{}, resolveMaxConcurrentWrites(opts.MaxConcurrentWrites)),
	}
	r.keys.Store(store)

	r.auth = authConfig{
		Region: opts.Region,
		// Reads through the atomic pointer, so a key deleted a moment ago is
		// already gone for the request being signed right now.
		SecretForKeyID: func(keyID string) (string, bool) {
			rec, ok := r.currentKeys().byAccessKey[keyID]
			if !ok {
				return "", false
			}
			// We don't compare keyID with ConstantTimeCompare here:
			// the map lookup already happened in O(1) on the exact
			// key, and the secret IS what gets ConstantTimeCompare'd
			// inside the verifier. Returning the secret as the raw
			// string is what the verifier expects.
			return rec.Secret, true
		},
	}

	// Sweep any active multipart uploads left in phase=committing across
	// crashes. Rows whose durable record exists are promoted to phase=done;
	// the rest are left for client-driven retry of Complete.
	recoverPendingMultipartUploads(svc)
	return &Handler{router: r, svc: svc, region: opts.Region}, nil
}

// Handler serves the S3 API and owns the live access-key set.
type Handler struct {
	router *router
	svc    *domain.Service
	region string
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	h.router.serve(w, req)
}

// SetKeys replaces the access-key set atomically.
//
// Validation happens before the swap, so a rejected update leaves the running
// key set untouched rather than half-applied.
func (h *Handler) SetKeys(keys []KeyEntry) error {
	// An empty set is legitimate here, unlike at startup: deleting the last key
	// must mean nothing authenticates, not that the previous set stays live.
	if len(keys) == 0 {
		h.router.keys.Store(&keyStore{byAccessKey: map[string]keyRecord{}})
		h.router.limiter = newRateLimiter(nil)
		return nil
	}

	store, err := buildKeyStore(Options{Keys: keys}, h.svc)
	if err != nil {
		return err
	}
	h.router.keys.Store(store)
	h.router.limiter = newRateLimiter(keys)
	return nil
}

func (r *router) currentKeys() *keyStore {
	return r.keys.Load()
}

// buildKeyStore folds the multi-tenant Keys list and the legacy
// AccessKey/SecretKey single-tenant fields into the indexed
// keyStore. Validates: at least one entry exists, no duplicate
// access keys, every whitelist refers to a real mount.
func buildKeyStore(opts Options, svc *domain.Service) (*keyStore, error) {
	mountSet := map[string]struct{}{}
	for _, root := range svc.ListRoot() {
		mountSet[root.Name] = struct{}{}
	}

	store := &keyStore{byAccessKey: map[string]keyRecord{}}

	// Legacy single-tenant: synthesize a "*" entry. When both Keys
	// and the legacy fields are set, the legacy entry is added too
	// — the operator gets exactly what they configured. A duplicate
	// access-key collision still surfaces as an error.
	if opts.AccessKey != "" || opts.SecretKey != "" {
		if opts.AccessKey == "" || opts.SecretKey == "" {
			return nil, errors.New("s3: AccessKey and SecretKey must be set together (legacy single-tenant)")
		}
		if _, dup := store.byAccessKey[opts.AccessKey]; dup {
			return nil, fmt.Errorf("s3: access key %q is duplicated between Keys list and legacy AccessKey", opts.AccessKey)
		}
		store.byAccessKey[opts.AccessKey] = keyRecord{
			Secret:     opts.SecretKey,
			AllBuckets: true,
		}
	}

	for i, k := range opts.Keys {
		if k.AccessKey == "" || k.SecretKey == "" {
			return nil, fmt.Errorf("s3: keys[%d]: access_key and secret_key must be non-empty", i)
		}
		if _, dup := store.byAccessKey[k.AccessKey]; dup {
			return nil, fmt.Errorf("s3: keys[%d]: access key %q is duplicated", i, k.AccessKey)
		}
		// Rate-limit sanity. 0 = unlimited, positive = throttle,
		// negative = operator typo. Silently treating negative as
		// unlimited would remove the intended limit without warning.
		if k.RequestsPerSecond < 0 {
			return nil, fmt.Errorf("s3: keys[%d]: requests_per_second must be >= 0 (0 disables throttling), got %d", i, k.RequestsPerSecond)
		}
		if k.Burst < 0 {
			return nil, fmt.Errorf("s3: keys[%d]: burst must be >= 0, got %d", i, k.Burst)
		}
		all := false
		set := map[string]struct{}{}
		for _, b := range k.Buckets {
			b = strings.TrimSpace(b)
			if b == "" {
				continue
			}
			if b == "*" {
				all = true
				continue
			}
			if _, ok := mountSet[b]; !ok {
				return nil, fmt.Errorf("s3: keys[%d]: bucket %q is not a configured mount", i, b)
			}
			set[b] = struct{}{}
		}
		rec := keyRecord{Secret: k.SecretKey, AllBuckets: all}
		if !all {
			rec.Buckets = set
		}
		store.byAccessKey[k.AccessKey] = rec
	}

	if len(store.byAccessKey) == 0 {
		return nil, errors.New("s3: at least one key must be configured (Keys list or legacy AccessKey/SecretKey)")
	}
	return store, nil
}

type router struct {
	svc  *domain.Service
	auth authConfig
	// keys is swapped atomically when access keys are created, rotated or
	// deleted, so a revoked key stops working on the next request rather than
	// at the next restart.
	keys        atomic.Pointer[keyStore]
	limiter     *rateLimiter // nil when no key has a configured limit
	accessLog   bool
	metrics     *metrics.Registry // nil-safe; receives the rate-limit reject counter
	activity    *activity.Ring
	writeSlots  chan struct{}
	uploadLocks uploadLockSet
	partLocks   uploadLockSet
}

func resolveMaxConcurrentWrites(v int) int {
	if v > 0 {
		return v
	}
	n := runtime.NumCPU() * 8
	if n < 32 {
		return 32
	}
	if n > 512 {
		return 512
	}
	return n
}

func (r *router) acquireWriteSlot(ctx context.Context) error {
	if r == nil || r.writeSlots == nil {
		return nil
	}
	select {
	case r.writeSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *router) releaseWriteSlot() {
	if r == nil || r.writeSlots == nil {
		return
	}
	select {
	case <-r.writeSlots:
	default:
	}
}

type uploadLockSet struct {
	mu    sync.Mutex
	locks map[string]*uploadLock
}

type uploadLock struct {
	mu   sync.Mutex
	refs int
}

func (s *uploadLockSet) acquire(uploadID string) func() {
	if strings.TrimSpace(uploadID) == "" {
		return func() {}
	}
	s.mu.Lock()
	if s.locks == nil {
		s.locks = make(map[string]*uploadLock)
	}
	lock := s.locks[uploadID]
	if lock == nil {
		lock = &uploadLock{}
		s.locks[uploadID] = lock
	}
	lock.refs++
	s.mu.Unlock()

	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		s.mu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(s.locks, uploadID)
		}
		s.mu.Unlock()
	}
}

// keyForRequest returns the verified key's record. The verifier has
// already proven the request was signed with this access key, so
// any present key in the store is authoritative — the lookup is
// just to retrieve the bucket whitelist.
func (r *router) keyForRequest(verified *sigV4Result) (keyRecord, bool) {
	rec, ok := r.currentKeys().byAccessKey[verified.AccessKeyID]
	return rec, ok
}

// authorizeBucket is the central authorization check. Returns true
// when the verified key may access the named bucket; on false,
// writes an AccessDenied response — bucket existence is NOT
// revealed (see §10 of the plan: forbidden buckets answer 403, not
// 404). Handlers call this before any operation that touches a
// specific bucket.
func (r *router) authorizeBucket(w http.ResponseWriter, req *http.Request, verified *sigV4Result, bucket, key string) bool {
	rec, ok := r.keyForRequest(verified)
	if !ok {
		// Should never happen — the verifier matched the secret
		// against this access key. Surface as a clean Forbidden
		// rather than a 500.
		writeError(w, req, errAccessDenied, "access denied", withBucket(bucket), withKey(key))
		return false
	}
	if rec.canAccess(bucket) {
		return true
	}
	writeError(w, req, errAccessDenied, "access denied", withBucket(bucket), withKey(key))
	return false
}

func (r *router) serve(w http.ResponseWriter, req *http.Request) {
	start := time.Now()
	sw := &s3StatusWriter{ResponseWriter: w, status: http.StatusOK}
	w = sw
	var actor activity.Actor
	var activityOp string
	var activityTarget *activity.Target
	defer func() {
		if r.activity == nil || actor.Kind == "" || !logS3Activity(activityOp) {
			return
		}
		r.activity.Record(activity.Event{
			Actor:      actor,
			Operation:  "s3." + activityOp,
			Outcome:    s3OutcomeForStatus(sw.status),
			Target:     activityTarget,
			DurationMS: time.Since(start).Milliseconds(),
			RequestID:  strings.TrimSpace(req.Header.Get("X-Request-Id")),
			Error:      s3ErrorForStatus(sw.status),
		})
	}()

	// Rate-limit BEFORE verifyRequest. verifyRequest binds (and
	// for hex-mode signed bodies, reads) the request body, which
	// we don't want to do for a throttled key — a flooded key
	// would otherwise force per-request body buffering even after
	// it should be paying nothing. The access-key extraction is
	// a header-only parse; signature/body are still verified by
	// verifyRequest below. See peekAccessKey for the trust-model
	// rationale.
	if accessKey := peekAccessKey(req); accessKey != "" && !r.limiter.allow(accessKey) {
		r.metrics.RatelimitRejected()
		w.Header().Set("Retry-After", "1")
		writeError(w, req, errSlowDown, "request rate limit exceeded for this access key")
		return
	}

	// Authenticate. The verifier returns a sigV4Result whose
	// BodyReader is what handlers must read (might be a chunked
	// decoder). Handlers MUST NOT read req.Body directly.
	verified, sigErr := verifyRequest(req, r.auth)
	if sigErr != nil {
		writeError(w, req, sigErr.Code, sigErr.Message)
		return
	}
	actor = activity.Actor{
		Kind:           activity.ActorS3Key,
		ID:             verified.AccessKeyID,
		DelegatedActor: activity.CleanActorLabel(req.Header.Get("X-Filegate-Actor")),
	}

	bucket, key := parsePathStyle(req.URL.Path)
	activityOp = s3ActivityOperation(req, bucket, key)
	if bucket != "" {
		activityTarget = &activity.Target{Kind: "s3_object", Path: bucket}
		if key != "" {
			activityTarget.Path = bucket + "/" + key
		}
	}

	switch {
	case bucket == "":
		// "/" — root operations. Only ListBuckets in M1.
		switch req.Method {
		case http.MethodGet:
			metrics.SetOp(req.Context(), "ListBuckets")
			r.handleListBuckets(w, req, verified)
		default:
			writeError(w, req, errMethodNotAllowed, "only GET / is supported at the root")
		}
		return
	case key == "":
		// Bucket-level operations.
		r.handleBucketOp(w, req, verified, bucket)
		return
	default:
		// Object-level operations.
		r.handleObjectOp(w, req, verified, bucket, key)
	}
}

type s3StatusWriter struct {
	http.ResponseWriter
	status int
}

func (w *s3StatusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func s3OutcomeForStatus(status int) activity.Outcome {
	if status >= 200 && status < 400 {
		return activity.OutcomeSucceeded
	}
	return activity.OutcomeFailed
}

func s3ErrorForStatus(status int) string {
	if status >= 400 {
		return http.StatusText(status)
	}
	return ""
}

func s3ActivityOperation(req *http.Request, bucket, key string) string {
	q := req.URL.Query()
	uploadID := q.Get("uploadId")
	switch req.Method {
	case http.MethodGet:
		if key != "" && uploadID == "" {
			return "GetObject"
		}
	case http.MethodPut:
		if key != "" && uploadID == "" {
			if strings.TrimSpace(req.Header.Get("x-amz-copy-source")) != "" {
				return "CopyObject"
			}
			return "PutObject"
		}
	case http.MethodPost:
		if key == "" {
			if _, ok := q["delete"]; ok {
				return "DeleteObjects"
			}
			return ""
		}
		if _, ok := q["uploads"]; ok {
			return "CreateMultipartUpload"
		}
		if uploadID != "" {
			return "CompleteMultipartUpload"
		}
	case http.MethodDelete:
		if key != "" && uploadID != "" {
			return "AbortMultipartUpload"
		}
		if key != "" {
			return "DeleteObject"
		}
	}
	_ = bucket
	return ""
}

func logS3Activity(op string) bool {
	switch op {
	case "GetObject", "PutObject", "CopyObject", "DeleteObject", "DeleteObjects",
		"CreateMultipartUpload", "CompleteMultipartUpload", "AbortMultipartUpload":
		return true
	default:
		return false
	}
}

// parsePathStyle splits "/{bucket}/{key}" form. Empty path-segments
// are tolerated (the spec allows "/" for root, "/bucket" for bucket
// op, "/bucket/key/with/slashes" for object).
func parsePathStyle(path string) (bucket, key string) {
	// Trim the leading "/" — without it everything is a no-op
	// special case.
	rest := strings.TrimPrefix(path, "/")
	if rest == "" {
		return "", ""
	}
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return rest, ""
	}
	return rest[:slash], rest[slash+1:]
}

// handleListBuckets returns the per-key-filtered list of buckets.
// Real S3 returns every bucket the requesting account owns; we
// model "account" as the access key and apply the key's whitelist.
// The full mount list is NOT exposed — a key without a "*" wildcard
// only sees its explicitly-permitted buckets, matching the
// existence-vs-authorization separation the rest of the API
// preserves.
func (r *router) handleListBuckets(w http.ResponseWriter, _ *http.Request, verified *sigV4Result) {
	rec, ok := r.keyForRequest(verified)
	if !ok {
		// Same defensive branch as authorizeBucket. An unknown key
		// shouldn't have passed the verifier; if it did, treat it
		// as having no buckets rather than 500-ing.
		writeListAllMyBuckets(w, nil)
		return
	}
	mountNames := make([]string, 0, len(r.svc.ListRoot()))
	for _, root := range r.svc.ListRoot() {
		mountNames = append(mountNames, root.Name)
	}
	allowed := rec.allowedBuckets(mountNames)
	writeListAllMyBuckets(w, allowed)
	if r.accessLog {
		log.Printf("[filegate-s3] ListBuckets key=%s returned %d bucket(s)", verified.AccessKeyID, len(allowed))
	}
}

// handleBucketOp dispatches bucket-level methods. Authorization is
// checked BEFORE the bucket-existence probe so a key without
// permission cannot distinguish "no such bucket" from "exists but
// forbidden" — both surface as AccessDenied per the §10 contract.
func (r *router) handleBucketOp(w http.ResponseWriter, req *http.Request, verified *sigV4Result, bucket string) {
	if !r.authorizeBucket(w, req, verified, bucket, "") {
		return
	}
	if !r.bucketExists(bucket) {
		// Whitelist check passed but the mount really doesn't
		// exist — at this point bucket-existence leakage is fine,
		// the key is authorized to know about the bucket. The
		// alternative is a misleading 403 for genuinely-typo'd
		// bucket names, which makes operator debugging painful.
		writeError(w, req, errNoSuchBucket, "bucket does not exist", withBucket(bucket))
		return
	}
	switch req.Method {
	case http.MethodGet:
		// Bucket-level GET sub-resources:
		//   ?uploads          → ListMultipartUploads
		//   (default)         → ListObjectsV2 (list-type=2 expected)
		if _, ok := req.URL.Query()["uploads"]; ok {
			metrics.SetOp(req.Context(), "ListMultipartUploads")
			r.handleListMultipartUploads(w, req, verified, bucket)
			return
		}
		metrics.SetOp(req.Context(), "ListObjectsV2")
		r.handleListObjectsV2(w, req, verified, bucket)
	case http.MethodPost:
		// Bucket-level POST sub-resource:
		//   ?delete           → DeleteObjects (bulk delete, XML body)
		if _, ok := req.URL.Query()["delete"]; ok {
			metrics.SetOp(req.Context(), "DeleteObjects")
			r.handleDeleteObjects(w, req, verified, bucket)
			return
		}
		writeError(w, req, errMethodNotAllowed, "POST on a bucket requires ?delete")
	case http.MethodPut, http.MethodDelete:
		writeError(w, req, errMethodNotAllowed, "buckets come from filegate config; CreateBucket/DeleteBucket are rejected")
	case http.MethodHead:
		// HEAD on a bucket = bucket existence check (HEAD bucket).
		metrics.SetOp(req.Context(), "HeadBucket")
		w.WriteHeader(http.StatusOK)
	default:
		writeError(w, req, errMethodNotAllowed, "method not supported")
	}
}

// handleObjectOp dispatches single-object methods to the per-method
// handlers in object.go. Authorization is checked first (see
// handleBucketOp comment); the bucket-existence check follows.
//
// Multipart ops are dispatched here too via query-arg sub-resources:
//
//	POST   ?uploads          → CreateMultipartUpload
//	PUT    ?partNumber=N&uploadId=X → UploadPart
//	POST   ?uploadId=X       → CompleteMultipartUpload
//	DELETE ?uploadId=X       → AbortMultipartUpload
//	GET    ?uploadId=X       → ListParts
func (r *router) handleObjectOp(w http.ResponseWriter, req *http.Request, verified *sigV4Result, bucket, key string) {
	if !r.authorizeBucket(w, req, verified, bucket, key) {
		return
	}
	if !r.bucketExists(bucket) {
		writeError(w, req, errNoSuchBucket, "bucket does not exist", withBucket(bucket), withKey(key))
		return
	}
	q := req.URL.Query()
	hasUploads := func() bool { _, ok := q["uploads"]; return ok }()
	uploadID := q.Get("uploadId")

	switch req.Method {
	case http.MethodGet:
		if uploadID != "" {
			metrics.SetOp(req.Context(), "ListParts")
			r.handleListParts(w, req, verified, bucket, key)
			return
		}
		metrics.SetOp(req.Context(), "GetObject")
		r.handleGetObject(w, req, verified, bucket, key)
	case http.MethodHead:
		metrics.SetOp(req.Context(), "HeadObject")
		r.handleHeadObject(w, req, verified, bucket, key)
	case http.MethodPut:
		if uploadID != "" {
			metrics.SetOp(req.Context(), "UploadPart")
			r.handleUploadPart(w, req, verified, bucket, key)
			return
		}
		// PutObject vs CopyObject: handlePutObject re-labels to
		// CopyObject when x-amz-copy-source is present.
		metrics.SetOp(req.Context(), "PutObject")
		r.handlePutObject(w, req, verified, bucket, key)
	case http.MethodPost:
		if hasUploads {
			metrics.SetOp(req.Context(), "CreateMultipartUpload")
			r.handleCreateMultipartUpload(w, req, verified, bucket, key)
			return
		}
		if uploadID != "" {
			metrics.SetOp(req.Context(), "CompleteMultipartUpload")
			r.handleCompleteMultipartUpload(w, req, verified, bucket, key)
			return
		}
		writeError(w, req, errMethodNotAllowed, "POST requires ?uploads or ?uploadId")
	case http.MethodDelete:
		if uploadID != "" {
			metrics.SetOp(req.Context(), "AbortMultipartUpload")
			r.handleAbortMultipartUpload(w, req, verified, bucket, key)
			return
		}
		metrics.SetOp(req.Context(), "DeleteObject")
		r.handleDeleteObject(w, req, verified, bucket, key)
	default:
		writeError(w, req, errMethodNotAllowed, "method not supported")
	}
}

// bucketExists checks whether a mount with the given name exists.
// The check happens AFTER authorization in the dispatchers above so
// non-permitted buckets cannot be probed for existence.
func (r *router) bucketExists(name string) bool {
	for _, root := range r.svc.ListRoot() {
		if root.Name == name {
			return true
		}
	}
	return false
}
