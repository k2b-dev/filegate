package s3

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/k2b-dev/filegate/v3/domain"
)

// deleteObjectsMaxKeys is the AWS-spec ceiling: 1000 keys per call.
// Going above bloats the response, holds locks across many objects,
// and mirrors what every S3 client expects. Requests that exceed
// the limit are rejected with MalformedXML — the caller batches.
const deleteObjectsMaxKeys = 1000

// deleteObjectsParallelism caps how many delete operations run
// concurrently per request. Filegate's path-lock layer serializes
// per-leaf, so going wider than this just increases lock churn
// without throughput benefit. 16 is enough to amortize syscall
// latency on rotational disks while staying well under the daemon
// goroutine budget.
const deleteObjectsParallelism = 16

// handleDeleteObjects implements POST /{bucket}?delete (the bulk
// DeleteObjects op). The body is a <Delete> XML document listing
// up to 1000 keys; we delete each, returning one <Deleted> per
// success and one <Error> per failure.
//
// Quiet mode (<Quiet>true</Quiet>) suppresses the <Deleted> entries
// — errors still surface, matching AWS behaviour.
//
// Authorization is handled by the bucket dispatcher in router.go;
// by the time we land here the requesting key is allowed to access
// the bucket. Per-key whitelist already guarantees no cross-bucket
// leakage in this op (one bucket per call by design).
func (rt *router) handleDeleteObjects(w http.ResponseWriter, r *http.Request, verified *sigV4Result, bucket string) {
	// Bound the body — a malicious client shouldn't be able to OOM
	// us with a giant XML document. The 1000-key cap puts the
	// theoretical maximum under ~2 MB even with long keys; we cap
	// at 4 MB defensively.
	bodyRaw, err := io.ReadAll(io.LimitReader(verified.BodyReader, 4*1024*1024))
	if err != nil {
		writeError(w, r, errIncompleteBody, "could not read request body", withBucket(bucket))
		return
	}
	var req deleteObjectsRequest
	if err := xml.Unmarshal(bodyRaw, &req); err != nil {
		writeError(w, r, errMalformedXML, fmt.Sprintf("body must be a Delete XML document: %s", err), withBucket(bucket))
		return
	}
	if len(req.Objects) == 0 {
		writeError(w, r, errMalformedXML, "DeleteObjects requires at least one Object", withBucket(bucket))
		return
	}
	if len(req.Objects) > deleteObjectsMaxKeys {
		writeError(w, r, errMalformedXML, fmt.Sprintf("DeleteObjects accepts at most %d keys per call", deleteObjectsMaxKeys), withBucket(bucket))
		return
	}

	// Per-object processing in a bounded worker pool. Path-lock
	// inside DeleteIfMatch serializes per-leaf so worker contention
	// only matters across distinct keys.
	type jobResult struct {
		index   int
		deleted *deletedResultEntry
		errEnt  *deleteResultError
	}
	results := make([]jobResult, len(req.Objects))
	var wg sync.WaitGroup
	sem := make(chan struct{}, deleteObjectsParallelism)
	for i, obj := range req.Objects {
		i, obj := i, obj
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = jobResult{index: i}
			res, errEnt := deleteOneForBulk(rt, bucket, obj)
			results[i].deleted = res
			results[i].errEnt = errEnt
		}()
	}
	wg.Wait()

	// Build the result. Quiet mode suppresses Deleted entries but
	// keeps Errors (per AWS behaviour: errors are never quiet).
	res := deleteObjectsResult{}
	for _, r := range results {
		if r.errEnt != nil {
			res.Errors = append(res.Errors, *r.errEnt)
			continue
		}
		if !req.Quiet && r.deleted != nil {
			res.Deleted = append(res.Deleted, *r.deleted)
		}
	}
	// Stable order so tests + clients can compare easily.
	sort.SliceStable(res.Deleted, func(i, j int) bool { return res.Deleted[i].Key < res.Deleted[j].Key })
	sort.SliceStable(res.Errors, func(i, j int) bool { return res.Errors[i].Key < res.Errors[j].Key })

	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Server", "filegate")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(res)

	if rt.accessLog {
		extra := fmt.Sprintf("requested=%d deleted=%d errors=%d quiet=%t",
			len(req.Objects), len(res.Deleted), len(res.Errors), req.Quiet)
		rt.logAccess("DeleteObjects", bucket, "", verified.AccessKeyID, extra)
	}
}

// deleteOneForBulk performs the per-object delete + builds the
// result/error entry for the bulk op. Mirrors handleDeleteObject
// semantics: missing keys are idempotent successes (NOT errors —
// matches AWS behaviour for bulk delete), validation failures map
// to per-entry Error entries. Object-key validation runs first so
// a malformed key doesn't waste an index lookup.
func deleteOneForBulk(rt *router, bucket string, obj deleteRequestObject) (*deletedResultEntry, *deleteResultError) {
	key := obj.Key
	if obj.VersionID != "" {
		// Filegate doesn't expose per-object versions on the S3
		// surface (the versioning feature is REST-only). Reject
		// upfront so misconfigured clients catch this rather than
		// silently dropping VersionId.
		return nil, &deleteResultError{
			Key:     key,
			Code:    string(errInvalidArgument),
			Message: "VersionId is not supported on this S3 endpoint",
		}
	}
	if err := validateObjectKey(key); err != nil {
		return nil, &deleteResultError{
			Key:     key,
			Code:    string(errInvalidArgument),
			Message: err.Error(),
		}
	}

	id, err := rt.svc.ResolvePath(virtualPathFor(bucket, key))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// AWS-spec: a missing key is a success in DeleteObjects
			// (idempotent). The Deleted entry is emitted.
			return &deletedResultEntry{Key: key}, nil
		}
		return nil, &deleteResultError{
			Key:     key,
			Code:    string(errInternalError),
			Message: err.Error(),
		}
	}
	meta, err := rt.svc.GetFile(id)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return &deletedResultEntry{Key: key}, nil
		}
		return nil, &deleteResultError{
			Key:     key,
			Code:    string(errInternalError),
			Message: err.Error(),
		}
	}
	if meta.Type != "file" {
		// Same precaution as single-object DELETE: directories
		// silently succeed in S3-shape but never recursively delete.
		return &deletedResultEntry{Key: key}, nil
	}

	if err := rt.svc.DeleteIfMatch(id, ""); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return &deletedResultEntry{Key: key}, nil
		}
		return nil, &deleteResultError{
			Key:     key,
			Code:    mapDomainErrorCode(err),
			Message: strings.TrimSpace(err.Error()),
		}
	}
	return &deletedResultEntry{Key: key}, nil
}

// mapDomainErrorCode is the bulk-delete-flavored version of
// mapDomainError — instead of writing an HTTP response, it returns
// the symbolic <Code> string per-entry. Unknown errors fall back
// to InternalError.
func mapDomainErrorCode(err error) string {
	switch {
	case errors.Is(err, domain.ErrForbidden):
		return string(errAccessDenied)
	case errors.Is(err, domain.ErrConflict):
		return string(errPreconditionFailed)
	case errors.Is(err, domain.ErrInvalidArgument):
		return string(errInvalidArgument)
	default:
		return string(errInternalError)
	}
}
