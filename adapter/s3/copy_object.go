package s3

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/k2b-dev/filegate/v3/domain"
	"github.com/k2b-dev/filegate/v3/infra/metrics"
)

// handleCopyObject implements S3 CopyObject. The wire shape is
// PUT /{destBucket}/{destKey} with the source identified by the
// x-amz-copy-source header. The op is dispatched here from
// handlePutObject when that header is present.
//
// Authorization: the destination bucket whitelist is checked by
// the dispatcher in router.go (handleObjectOp → authorizeBucket).
// We add the source-bucket check here so a key without source
// access can't smuggle bytes out of a forbidden bucket. Both
// sides return 403 AccessDenied without revealing existence.
//
// AWS-spec compliance:
//   - Source size > 5 GiB → EntityTooLarge (multipart-copy is
//     out of scope for M3; oversized sources must use it).
//   - x-amz-copy-source-if-{match,none-match,modified-since,
//     unmodified-since} → 412 PreconditionFailed when the source
//     fails the check.
//   - dest If-Match / If-None-Match → 412 PreconditionFailed.
//   - x-amz-metadata-directive: COPY (default) or REPLACE.
//   - Single-object copy: dest etag_md5 == source's; multipart_etag
//     CLEARED on the destination.
func (rt *router) handleCopyObject(w http.ResponseWriter, r *http.Request, verified *sigV4Result, destBucket, destKey string) {
	// Re-label the op: the dispatcher set "PutObject" before
	// detecting the x-amz-copy-source header. SetOp overwrites the
	// holder so the metric attributes this to CopyObject.
	metrics.SetOp(r.Context(), "CopyObject")
	if err := validateObjectKey(destKey); err != nil {
		writeError(w, r, errInvalidArgument, err.Error(), withBucket(destBucket), withKey(destKey))
		return
	}

	srcBucket, srcKey, parseErr := parseCopySourceHeader(r.Header.Get("x-amz-copy-source"))
	if parseErr != nil {
		writeError(w, r, errInvalidArgument, parseErr.Error(), withBucket(destBucket), withKey(destKey))
		return
	}
	if err := validateObjectKey(srcKey); err != nil {
		writeError(w, r, errInvalidArgument, fmt.Sprintf("invalid copy source key: %s", err), withBucket(destBucket), withKey(destKey))
		return
	}

	// Source authorization. The dispatcher already verified destBucket;
	// we still need srcBucket. canAccess returns false for both
	// "forbidden" and "nonexistent" — the same AccessDenied response.
	rec, ok := rt.keyForRequest(verified)
	if !ok || !rec.canAccess(srcBucket) {
		writeError(w, r, errAccessDenied, "access denied", withBucket(destBucket), withKey(destKey))
		return
	}

	directive := strings.ToUpper(strings.TrimSpace(r.Header.Get("x-amz-metadata-directive")))
	if directive == "" {
		directive = "COPY"
	}
	if directive != "COPY" && directive != "REPLACE" {
		writeError(w, r, errInvalidArgument, "x-amz-metadata-directive must be COPY or REPLACE", withBucket(destBucket), withKey(destKey))
		return
	}

	// AWS-spec rule: a CopyObject that names the SAME source and
	// destination AND uses metadata-directive=COPY is rejected as
	// InvalidRequest. Without this guard, the call is a guaranteed
	// no-op (no bytes change, no metadata changes) — clients that
	// send it have either a bug or a broken assumption, so failing
	// loudly is the friendlier behavior. The valid in-place
	// metadata-update workflow uses metadata-directive=REPLACE,
	// which is allowed and exercised by the existing self-copy
	// REPLACE tests.
	if srcBucket == destBucket && srcKey == destKey && directive == "COPY" {
		writeError(w, r, errInvalidRequest, "self-copy requires metadata-directive=REPLACE (otherwise the call is a no-op)", withBucket(destBucket), withKey(destKey))
		return
	}

	// REPLACE pulls metadata from the request headers (same builder
	// used by PutObject). COPY ignores DestOpts entirely; we still
	// build a zero-value struct to keep the API symmetric.
	var destOpts domain.S3WriteOptions
	if directive == "REPLACE" {
		opts, perr := buildS3WriteOptions(r)
		if perr != nil {
			writeError(w, r, perr.Code, perr.Message, withBucket(destBucket), withKey(destKey))
			return
		}
		// IfMatch / IfNoneMatchAny on the dest live in dedicated
		// CopyObjectArgs fields below; clear them off the embedded
		// opts so the domain method's invariant ("DestOpts.IfMatch
		// not honored on copy") holds.
		opts.IfMatch = ""
		opts.IfNoneMatchAny = false
		destOpts = opts
	}

	// Parse copy-source preconditions. Empty values disable the
	// check; invalid time formats surface as InvalidArgument.
	srcIfModifiedSince, err := parseCopySourceTime(r.Header.Get("x-amz-copy-source-if-modified-since"))
	if err != nil {
		writeError(w, r, errInvalidArgument, "x-amz-copy-source-if-modified-since: "+err.Error(), withBucket(destBucket), withKey(destKey))
		return
	}
	srcIfUnmodifiedSince, err := parseCopySourceTime(r.Header.Get("x-amz-copy-source-if-unmodified-since"))
	if err != nil {
		writeError(w, r, errInvalidArgument, "x-amz-copy-source-if-unmodified-since: "+err.Error(), withBucket(destBucket), withKey(destKey))
		return
	}

	// Destination preconditions (PutObject-shaped).
	destIfNoneMatchAny := false
	if cond := strings.TrimSpace(r.Header.Get("If-None-Match")); cond != "" {
		if cond != "*" {
			writeError(w, r, errInvalidArgument, "If-None-Match must be \"*\" on CopyObject", withBucket(destBucket), withKey(destKey))
			return
		}
		destIfNoneMatchAny = true
	}
	destIfMatch := strings.TrimSpace(r.Header.Get("If-Match"))

	args := domain.CopyObjectArgs{
		SourceVP:                virtualPathFor(srcBucket, srcKey),
		DestVP:                  virtualPathFor(destBucket, destKey),
		SourceIfMatch:           strings.Trim(strings.TrimSpace(r.Header.Get("x-amz-copy-source-if-match")), `"`),
		SourceIfNoneMatch:       strings.Trim(strings.TrimSpace(r.Header.Get("x-amz-copy-source-if-none-match")), `"`),
		SourceIfModifiedSince:   srcIfModifiedSince,
		SourceIfUnmodifiedSince: srcIfUnmodifiedSince,
		DestIfMatch:             destIfMatch,
		DestIfNoneMatchAny:      destIfNoneMatchAny,
		MetadataDirective:       directive,
		DestOpts:                destOpts,
	}

	res, err := rt.svc.CopyObjectS3(args)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrCopySourceTooLarge):
			writeError(w, r, errEntityTooLarge, "copy source exceeds 5 GiB single-copy limit", withBucket(destBucket), withKey(destKey))
		case errors.Is(err, domain.ErrConflict):
			writeError(w, r, errPreconditionFailed, "conditional copy precondition failed", withBucket(destBucket), withKey(destKey))
		case errors.Is(err, domain.ErrInvalidArgument):
			writeError(w, r, errInvalidArgument, "invalid copy argument", withBucket(destBucket), withKey(destKey))
		case errors.Is(err, domain.ErrNotFound):
			writeError(w, r, errNoSuchKey, "copy source does not exist", withBucket(destBucket), withKey(destKey))
		default:
			mapDomainError(w, r, err, destBucket, destKey)
		}
		return
	}

	// Build the response. AWS specifies CopyObjectResult with ETag +
	// LastModified. The status is always 200 (NOT 201, even on
	// create — quirky AWS behaviour).
	body := copyObjectResult{
		ETag:         quoteETag(res.ETag),
		LastModified: res.LastModified.UTC().Format("2006-01-02T15:04:05.000Z"),
	}
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Server", "filegate")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(body)

	if rt.accessLog {
		extra := fmt.Sprintf("src=%s/%s reflinked=%t directive=%s",
			srcBucket, srcKey, res.Reflinked, directive)
		rt.logAccess("CopyObject", destBucket, destKey, verified.AccessKeyID, extra)
	}
}

// parseCopySourceHeader parses the x-amz-copy-source header. AWS
// accepts both forms:
//   - "/bucket/key" (with leading slash, the AWS-recommended form)
//   - "bucket/key" (without the leading slash)
//
// The bucket part is plain ASCII; the key is URL-encoded so
// special characters survive transit. We URL-decode the key
// (NOT the bucket — bucket names are ASCII-only by S3 rule).
func parseCopySourceHeader(raw string) (bucket, key string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", errors.New("x-amz-copy-source must be set")
	}
	raw = strings.TrimPrefix(raw, "/")
	slash := strings.IndexByte(raw, '/')
	if slash <= 0 || slash == len(raw)-1 {
		return "", "", errors.New("x-amz-copy-source must be /bucket/key")
	}
	bucket = raw[:slash]
	encKey := raw[slash+1:]
	// Strip a versionId fragment (?versionId=...) — we don't
	// support versioned source references on the S3 surface.
	if q := strings.IndexByte(encKey, '?'); q >= 0 {
		query := encKey[q+1:]
		encKey = encKey[:q]
		if strings.Contains(query, "versionId=") {
			return "", "", errors.New("versioned copy sources are not supported")
		}
	}
	decKey, decErr := url.PathUnescape(encKey)
	if decErr != nil {
		return "", "", fmt.Errorf("could not URL-decode source key: %w", decErr)
	}
	return bucket, decKey, nil
}

// parseCopySourceTime parses an RFC1123 / RFC3339 / ISO8601 time
// for the x-amz-copy-source-if-{modified,unmodified}-since headers.
// Empty input returns the zero time (== precondition disabled).
func parseCopySourceTime(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, nil
	}
	// AWS prefers RFC1123/HTTP-date; accept both that and RFC3339
	// for tolerance.
	for _, layout := range []string{
		http.TimeFormat, // RFC1123 GMT
		time.RFC3339,
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000Z", // ISO8601 millisecond
	} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized time format: %q", v)
}
