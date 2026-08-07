package s3

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/k2b-dev/filegate/v3/domain"
)

// virtualPathFor builds the filegate virtual-path for a bucket+key
// pair: /{bucket}/{key}. Domain-side validation rejects the key if
// it contains forbidden segments (".", "..", empty, leading "/")
// or matches reserved internal namespaces.
func virtualPathFor(bucket, key string) string {
	if key == "" {
		return "/" + bucket
	}
	return "/" + bucket + "/" + key
}

// handlePutObject implements S3 PutObject. The body has already been
// authenticated (signature + payload-hash verified, or chunked
// payload validated per chunk); we read from verified.BodyReader,
// not r.Body directly.
//
// PutObject and CopyObject share the same wire shape (PUT
// /{bucket}/{key}); CopyObject is signaled by the
// x-amz-copy-source header. We dispatch to handleCopyObject when
// it's present so the byte-write path stays clean.
func (rt *router) handlePutObject(w http.ResponseWriter, r *http.Request, verified *sigV4Result, bucket, key string) {
	if err := rt.acquireWriteSlot(r.Context()); err != nil {
		writeError(w, r, errIncompleteBody, "request canceled before write slot", withBucket(bucket), withKey(key))
		return
	}
	defer rt.releaseWriteSlot()

	if v := strings.TrimSpace(r.Header.Get("x-amz-copy-source")); v != "" {
		rt.handleCopyObject(w, r, verified, bucket, key)
		return
	}
	if err := validateObjectKey(key); err != nil {
		writeError(w, r, errInvalidArgument, err.Error(), withBucket(bucket), withKey(key))
		return
	}
	opts, perr := buildS3WriteOptions(r)
	if perr != nil {
		writeError(w, r, perr.Code, perr.Message, withBucket(bucket), withKey(key))
		return
	}

	// Optional Content-MD5: if the client supplies it, we tee the
	// body through MD5 and reject mismatch with BadDigest. The
	// header is base64-encoded per AWS spec.
	var contentMD5 []byte
	if v := strings.TrimSpace(r.Header.Get("Content-MD5")); v != "" {
		raw, err := base64.StdEncoding.DecodeString(v)
		if err != nil || len(raw) != 16 {
			writeError(w, r, errInvalidDigest, "Content-MD5 must be a 16-byte base64-encoded value", withBucket(bucket), withKey(key))
			return
		}
		contentMD5 = raw
	}

	body := verified.BodyReader
	if contentMD5 != nil {
		body = newMD5VerifyingReader(body, contentMD5)
	}
	defer body.Close()

	meta, created, err := rt.svc.WriteObjectS3(virtualPathFor(bucket, key), body, opts)
	if err != nil {
		// MD5 mismatch surfaces from the verifying reader as a
		// distinguished error type so we can map it to BadDigest.
		var md5Err *md5MismatchError
		if errors.As(err, &md5Err) {
			writeError(w, r, errBadDigest, "Content-MD5 does not match request body", withBucket(bucket), withKey(key))
			return
		}
		// Streaming chunk signature failure → SignatureDoesNotMatch
		// (auth-class error, not internal).
		if errors.Is(err, errChunkSignatureMismatch) {
			writeError(w, r, errSignatureDoesNotMatch, "streaming chunk signature mismatch", withBucket(bucket), withKey(key))
			return
		}
		// Malformed chunk framing or oversize chunk → InvalidArgument.
		if errors.Is(err, errMalformedChunkHeader) ||
			errors.Is(err, errChunkTrailerMismatch) ||
			errors.Is(err, errChunkTooLarge) {
			writeError(w, r, errInvalidArgument, err.Error(), withBucket(bucket), withKey(key))
			return
		}
		// Conditional-write precondition failure → 412. Both
		// IfNoneMatchAny (target existed) and IfMatch (didn't
		// match current ETag) surface as ErrConflict from
		// WriteObjectS3 — which one is responsible is encoded in
		// the request headers.
		if errors.Is(err, domain.ErrConflict) {
			if opts.IfNoneMatchAny || opts.IfMatch != "" {
				writeError(w, r, errPreconditionFailed, "conditional PutObject precondition failed", withBucket(bucket), withKey(key))
				return
			}
		}
		mapDomainError(w, r, err, bucket, key)
		return
	}

	w.Header().Set("ETag", quoteETag(meta.ETag))
	w.Header().Set("Server", "filegate")
	if rt.accessLog {
		action := "overwrite"
		if created {
			action = "create"
		}
		rt.logAccess("PutObject", bucket, key, verified.AccessKeyID, action)
	}
	w.WriteHeader(http.StatusOK)
}

// handleGetObject implements S3 GetObject. Streams the file body
// with ETag, Last-Modified, Content-Length, Content-Type headers.
// Range requests are honoured.
func (rt *router) handleGetObject(w http.ResponseWriter, r *http.Request, verified *sigV4Result, bucket, key string) {
	if err := validateObjectKey(key); err != nil {
		writeError(w, r, errInvalidArgument, err.Error(), withBucket(bucket), withKey(key))
		return
	}
	id, err := rt.svc.ResolvePath(virtualPathFor(bucket, key))
	if err != nil {
		mapDomainError(w, r, err, bucket, key)
		return
	}
	meta, err := rt.svc.GetFile(id)
	if err != nil {
		mapDomainError(w, r, err, bucket, key)
		return
	}
	if meta.Type != "file" {
		writeError(w, r, errNoSuchKey, "object is not a file", withBucket(bucket), withKey(key))
		return
	}

	view, _ := rt.svc.GetS3Metadata(id)
	etag := effectiveETag(meta, view)
	mtime := time.UnixMilli(meta.Mtime)
	if status, short := applyConditionalRequest(r, etag, mtime); short {
		if status == http.StatusNotModified {
			w.Header().Set("ETag", quoteETag(etag))
			w.WriteHeader(status)
			return
		}
		writeError(w, r, errPreconditionFailed, "conditional request precondition failed", withBucket(bucket), withKey(key))
		return
	}

	rc, size, isDir, err := rt.svc.OpenContent(id)
	if err != nil {
		mapDomainError(w, r, err, bucket, key)
		return
	}
	if isDir {
		_ = rc
		writeError(w, r, errNoSuchKey, "object is not a file", withBucket(bucket), withKey(key))
		return
	}
	defer rc.Close()

	headers := w.Header()
	applyResponseS3Headers(headers, meta, view)
	headers.Set("Accept-Ranges", "bytes")

	if rangeHdr := r.Header.Get("Range"); rangeHdr != "" {
		start, end, ok := parseSingleByteRange(rangeHdr, size)
		if !ok {
			// HTTP-spec response for an unsatisfiable range:
			// 416 with Content-Range: bytes */<size>. Setting
			// the header on `headers` (= w.Header()) before the
			// status write — must be before w.WriteHeader, which
			// writeError calls below.
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			w.Header().Set("Content-Type", "application/xml")
			w.Header().Set("Server", "filegate")
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+"\n")
			_, _ = io.WriteString(w, `<Error><Code>InvalidRange</Code><Message>The requested range is not satisfiable</Message></Error>`)
			return
		}
		// Seek the body to start. For os.File we use Seek; for any
		// other reader we discard bytes.
		if seeker, sok := rc.(io.Seeker); sok {
			if _, err := seeker.Seek(start, io.SeekStart); err != nil {
				writeError(w, r, errInternalError, "could not seek to range start", withBucket(bucket), withKey(key))
				return
			}
		} else {
			if _, err := io.CopyN(io.Discard, rc, start); err != nil {
				writeError(w, r, errInternalError, "could not advance to range start", withBucket(bucket), withKey(key))
				return
			}
		}
		length := end - start + 1
		headers.Set("Content-Length", strconv.FormatInt(length, 10))
		headers.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.CopyN(w, rc, length)
		if rt.accessLog {
			rt.logAccess("GetObject", bucket, key, verified.AccessKeyID, fmt.Sprintf("range=%d-%d/%d", start, end, size))
		}
		return
	}

	headers.Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
	if rt.accessLog {
		rt.logAccess("GetObject", bucket, key, verified.AccessKeyID, fmt.Sprintf("size=%d", size))
	}
}

// handleHeadObject is HEAD = GET sans body. Conditional headers are
// honoured the same way as GET (If-None-Match → 304, If-Match
// failure → 412, etc.).
func (rt *router) handleHeadObject(w http.ResponseWriter, r *http.Request, verified *sigV4Result, bucket, key string) {
	if err := validateObjectKey(key); err != nil {
		writeError(w, r, errInvalidArgument, err.Error(), withBucket(bucket), withKey(key))
		return
	}
	id, err := rt.svc.ResolvePath(virtualPathFor(bucket, key))
	if err != nil {
		mapDomainError(w, r, err, bucket, key)
		return
	}
	meta, err := rt.svc.GetFile(id)
	if err != nil {
		mapDomainError(w, r, err, bucket, key)
		return
	}
	if meta.Type != "file" {
		writeError(w, r, errNoSuchKey, "object is not a file", withBucket(bucket), withKey(key))
		return
	}

	view, _ := rt.svc.GetS3Metadata(id)
	etag := effectiveETag(meta, view)
	mtime := time.UnixMilli(meta.Mtime)
	if status, short := applyConditionalRequest(r, etag, mtime); short {
		if status == http.StatusNotModified {
			w.Header().Set("ETag", quoteETag(etag))
			w.WriteHeader(status)
			return
		}
		writeError(w, r, errPreconditionFailed, "conditional request precondition failed", withBucket(bucket), withKey(key))
		return
	}

	headers := w.Header()
	applyResponseS3Headers(headers, meta, view)
	headers.Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	headers.Set("Accept-Ranges", "bytes")
	w.WriteHeader(http.StatusOK)
	if rt.accessLog {
		rt.logAccess("HeadObject", bucket, key, verified.AccessKeyID, fmt.Sprintf("size=%d", meta.Size))
	}
}

// handleDeleteObject implements S3 DeleteObject. AWS returns 204
// even if the object doesn't exist (idempotent delete). Crucially
// we ONLY delete file-typed entities — a path-style key that
// happens to resolve to a filegate directory is treated as
// no-such-key (204), NOT as a recursive subtree delete. S3
// objects are leaf files; recursively wiping a prefix because a
// client typed "DELETE /bucket/some-folder" would be a
// destructive surprise.
func (rt *router) handleDeleteObject(w http.ResponseWriter, r *http.Request, verified *sigV4Result, bucket, key string) {
	if err := validateObjectKey(key); err != nil {
		writeError(w, r, errInvalidArgument, err.Error(), withBucket(bucket), withKey(key))
		return
	}
	id, err := rt.svc.ResolvePath(virtualPathFor(bucket, key))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			// Idempotent: no-such-key is success.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		mapDomainError(w, r, err, bucket, key)
		return
	}
	meta, err := rt.svc.GetFile(id)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		mapDomainError(w, r, err, bucket, key)
		return
	}
	if meta.Type != "file" {
		// Directory at this path: pretend it doesn't exist as an
		// object. S3 has no concept of directories in path-style,
		// and recursively deleting a prefix from a single DELETE
		// would be catastrophic for clients that expected leaf
		// semantics.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// S3 conditional DELETE: If-Match must match the current ETag,
	// otherwise reject with PreconditionFailed.
	ifMatch := strings.TrimSpace(r.Header.Get("If-Match"))
	if err := rt.svc.DeleteIfMatch(id, ifMatch); err != nil {
		// Same idempotency: a delete that races with another delete
		// shouldn't surface as 404 to S3 clients.
		if errors.Is(err, domain.ErrNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// If-Match precondition failure → 412.
		if errors.Is(err, domain.ErrConflict) && ifMatch != "" {
			writeError(w, r, errPreconditionFailed, "If-Match precondition failed", withBucket(bucket), withKey(key))
			return
		}
		mapDomainError(w, r, err, bucket, key)
		return
	}
	if rt.accessLog {
		rt.logAccess("DeleteObject", bucket, key, verified.AccessKeyID, "")
	}
	w.WriteHeader(http.StatusNoContent)
}

// userMetadataMaxBytes is the AWS-spec ceiling on the total
// x-amz-meta-* user-metadata blob (2 KiB after JSON encoding).
// Both single-PUT and CreateMultipartUpload enforce it so the
// budget is consistent across the two write paths.
const userMetadataMaxBytes = 2 * 1024

// validateObjectKey rejects keys outside the M0-declared S3-key
// compatibility subset. This is the wire-side check; domain layer
// enforces filesystem-side rules separately. Keys MUST be valid
// UTF-8 — both because S3 spec mandates it and because our
// continuation-token "+0xff suffix" trick depends on UTF-8 keys
// not containing 0xff.
func validateObjectKey(key string) error {
	if key == "" {
		return errors.New("object key must not be empty")
	}
	if len(key) > 1024 {
		return errors.New("object key exceeds 1024 bytes")
	}
	if !utf8.ValidString(key) {
		return errors.New("object key is not valid UTF-8")
	}
	if strings.Contains(key, "\x00") {
		return errors.New("object key contains NUL byte")
	}
	if strings.HasSuffix(key, "/") {
		return errors.New("object key must not end with /")
	}
	for _, segment := range strings.Split(key, "/") {
		if segment == "" {
			return errors.New("object key must not contain empty segments")
		}
		if segment == "." || segment == ".." {
			return errors.New("object key must not contain . or .. segments")
		}
		if segment == ".fg-versions" || segment == ".fg-uploads" || isFilegateInternalTempObjectName(segment) {
			return errors.New("object key uses a filegate-internal reserved namespace")
		}
	}
	return nil
}

func isFilegateInternalTempObjectName(name string) bool {
	return strings.HasPrefix(name, ".") && strings.Contains(name, ".filegate-tmp-")
}

// buildS3WriteOptions translates request headers into the domain
// S3WriteOptions struct. Validates at the same time — invalid headers
// surface as InvalidArgument before the body is touched.
func buildS3WriteOptions(r *http.Request) (domain.S3WriteOptions, *sigV4VerifyError) {
	opts := domain.S3WriteOptions{
		ContentType:        r.Header.Get("Content-Type"),
		ContentEncoding:    r.Header.Get("Content-Encoding"),
		ContentDisposition: r.Header.Get("Content-Disposition"),
	}
	// "If-None-Match: *" is the only S3-conditional create form
	// (any other value is invalid for PutObject per AWS spec).
	if cond := r.Header.Get("If-None-Match"); cond != "" {
		if strings.TrimSpace(cond) != "*" {
			return opts, sigErr(errInvalidArgument, "If-None-Match must be \"*\" on PutObject")
		}
		opts.IfNoneMatchAny = true
	}
	// "If-Match: <etag>" enables compare-and-swap PUT — the write
	// only succeeds if the existing object's ETag matches one of
	// the supplied candidates. Multiple candidates allowed
	// (comma-separated). The check happens under the path-lock
	// inside WriteObjectS3.
	if cond := strings.TrimSpace(r.Header.Get("If-Match")); cond != "" {
		opts.IfMatch = cond
	}
	// User metadata: collect every x-amz-meta-* header into a map
	// and serialize to JSON for storage. AWS limits the total to
	// ~2KB; we enforce an 8KB ceiling defensively (under their
	// per-header limit for the SignedHeaders block).
	userMeta := make(map[string]string)
	for k, vs := range r.Header {
		if !strings.HasPrefix(strings.ToLower(k), "x-amz-meta-") {
			continue
		}
		name := strings.TrimPrefix(strings.ToLower(k), "x-amz-meta-")
		if name == "" {
			continue
		}
		// Multiple values comma-join (HTTP rule).
		userMeta[name] = strings.Join(vs, ",")
	}
	if len(userMeta) > 0 {
		// AWS S3 user-metadata budget is 2 KiB across all
		// x-amz-meta-* headers (counted as the raw header bytes:
		// "name + value + ': '" per header). We approximate by
		// summing the encoded JSON length, which is a tighter
		// representation but caps at the same order of magnitude.
		// Clients that hit this limit are doing something unusual
		// — typical metadata is a handful of small key/value pairs.
		blob, err := json.Marshal(userMeta)
		if err != nil {
			return opts, sigErr(errInvalidArgument, "could not encode x-amz-meta-* headers: %s", err)
		}
		if len(blob) > userMetadataMaxBytes {
			return opts, sigErr(errInvalidArgument, "x-amz-meta-* headers exceed 2 KiB user-metadata budget")
		}
		opts.UserMetadata = blob
	}
	return opts, nil
}

// readUserMetadata decodes the JSON blob stored in entity.S3UserMetadata
// back to a map[string]string. Returns nil/nil for files without
// stored metadata.
func readUserMetadata(view *domain.S3MetadataView) (map[string]string, error) {
	if view == nil || len(view.UserMetadata) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	if err := json.Unmarshal(view.UserMetadata, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// applyResponseS3Headers writes the response headers derived from
// the S3-only entity fields: ETag (multipart wins over single MD5),
// Content-Type override, Content-Encoding, Content-Disposition,
// and x-amz-meta-* expansion. Used by both GET and HEAD.
func applyResponseS3Headers(headers http.Header, meta *domain.FileMeta, view *domain.S3MetadataView) {
	// ETag selection: multipart composite ETag wins when set;
	// otherwise single-MD5 from FileMeta.
	etag := meta.ETag
	if view != nil && view.MultipartETag != "" {
		etag = view.MultipartETag
	}
	headers.Set("ETag", quoteETag(etag))
	headers.Set("Last-Modified", formatHTTPDate(time.UnixMilli(meta.Mtime)))
	headers.Set("Server", "filegate")
	if ct := contentTypeFor(meta, view); ct != "" {
		headers.Set("Content-Type", ct)
	}
	if view != nil {
		if view.ContentEncoding != "" {
			headers.Set("Content-Encoding", view.ContentEncoding)
		}
		if view.ContentDisposition != "" {
			headers.Set("Content-Disposition", view.ContentDisposition)
		}
	}
	if userMeta, _ := readUserMetadata(view); userMeta != nil {
		for k, v := range userMeta {
			// Bypass http.Header.Set's MIME canonicalization
			// (which would Title-Case "x-amz-meta-author" into
			// "X-Amz-Meta-Author"). Real AWS S3 returns these
			// headers lowercase, and SDK parsers (boto3/awscli)
			// preserve the case when stripping the prefix —
			// title-cased headers surface as ".Metadata.Author"
			// instead of ".Metadata.author", breaking client
			// code that round-trips metadata. Direct map access
			// preserves the lowercase key on the wire.
			name := strings.ToLower("x-amz-meta-" + k)
			headers[name] = []string{v}
		}
	}
}

// contentTypeFor returns the MIME type to put in the response. S3-set
// ContentType wins over filename-derived MimeType per the §7 rule.
func contentTypeFor(meta *domain.FileMeta, view *domain.S3MetadataView) string {
	if view != nil && view.ContentType != "" {
		return view.ContentType
	}
	return meta.MimeType
}

// mapDomainError translates a domain error into the appropriate S3
// XML response. Centralised so handlers don't drift in their
// error mappings.
func mapDomainError(w http.ResponseWriter, r *http.Request, err error, bucket, key string) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		writeError(w, r, errNoSuchKey, "object does not exist", withBucket(bucket), withKey(key))
	case errors.Is(err, domain.ErrConflict):
		writeError(w, r, errPreconditionFailed, "object already exists", withBucket(bucket), withKey(key))
	case errors.Is(err, domain.ErrInvalidArgument):
		writeError(w, r, errInvalidArgument, err.Error(), withBucket(bucket), withKey(key))
	case errors.Is(err, domain.ErrForbidden):
		writeError(w, r, errAccessDenied, "operation forbidden", withBucket(bucket), withKey(key))
	case errors.Is(err, domain.ErrInsufficientStorage), isNoSpaceError(err):
		writeError(w, r, errInsufficientStorage, "storage is full", withBucket(bucket), withKey(key))
	default:
		writeError(w, r, errInternalError, err.Error(), withBucket(bucket), withKey(key))
	}
}

// etagMatch reports whether condition matches etag. Two modes:
//
//   - strong: a weak validator (W/...) NEVER matches. Used by
//     If-Match (RFC 7232 §3.1).
//   - weak: weak validators match if the opaque part is equal.
//     Used by If-None-Match (RFC 7232 §3.2). For S3 we don't
//     emit weak ETags, so this is effectively the same as strong
//     match — but we still strip the W/ prefix so a client that
//     sends one isn't unfairly rejected.
//
// "*" matches any non-empty ETag.
type etagMatchMode int

const (
	strongMatch etagMatchMode = iota
	weakMatch
)

func etagMatch(condition, etag string, mode etagMatchMode) bool {
	condition = strings.TrimSpace(condition)
	if condition == "*" {
		return etag != ""
	}
	target := strings.Trim(etag, `"`)
	for _, candidate := range strings.Split(condition, ",") {
		candidate = strings.TrimSpace(candidate)
		isWeak := strings.HasPrefix(candidate, "W/")
		if isWeak {
			candidate = strings.TrimPrefix(candidate, "W/")
		}
		candidate = strings.Trim(candidate, `"`)
		if mode == strongMatch && isWeak {
			continue
		}
		if candidate == target {
			return true
		}
	}
	return false
}

// effectiveETag returns the ETag value the client sees on the wire —
// composite multipart ETag wins over single-MD5. All conditional
// header comparisons use this value, NOT the raw meta.ETag, so a
// multipart-uploaded file evaluates conditions against its
// composite identity.
func effectiveETag(meta *domain.FileMeta, view *domain.S3MetadataView) string {
	if view != nil && view.MultipartETag != "" {
		return view.MultipartETag
	}
	return meta.ETag
}

// applyConditionalRequest evaluates the four S3 conditional headers
// in RFC 7232 §6 precedence order. Returns:
//
//   - (status, true) when the condition triggers a short-circuit
//     response — caller must write the status and stop further
//     processing.
//   - (0, false) when conditions pass and the request should
//     continue normally.
//
// AWS S3 implements only If-Match / If-None-Match / If-Modified-Since
// / If-Unmodified-Since (no If-Range). Order per RFC:
//  1. If-Match           → 412 if no match
//  2. If-Unmodified-Since → 412 if modified after
//  3. If-None-Match       → 304 (GET/HEAD) or 412 (other)
//  4. If-Modified-Since  → 304 (GET/HEAD only) if not modified
func applyConditionalRequest(r *http.Request, etag string, mtime time.Time) (status int, shortCircuit bool) {
	if cond := r.Header.Get("If-Match"); cond != "" {
		if !etagMatch(cond, etag, strongMatch) {
			return http.StatusPreconditionFailed, true
		}
	}
	if cond := r.Header.Get("If-Unmodified-Since"); cond != "" {
		if t, err := http.ParseTime(cond); err == nil {
			if mtime.After(t) {
				return http.StatusPreconditionFailed, true
			}
		}
	}
	if cond := r.Header.Get("If-None-Match"); cond != "" {
		if etagMatch(cond, etag, weakMatch) {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				return http.StatusNotModified, true
			}
			return http.StatusPreconditionFailed, true
		}
	}
	if cond := r.Header.Get("If-Modified-Since"); cond != "" {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			if t, err := http.ParseTime(cond); err == nil {
				if !mtime.After(t) {
					return http.StatusNotModified, true
				}
			}
		}
	}
	return 0, false
}

// parseSingleByteRange parses a "Range: bytes=N-M" header (single
// range only — multi-range responses are out of scope for M1).
// Returns ok=false for malformed input or out-of-bounds. Empty
// objects (size==0) reject all ranges, since "bytes=N-M" can't
// satisfy any byte from a zero-byte file.
func parseSingleByteRange(header string, size int64) (start, end int64, ok bool) {
	if size == 0 {
		return 0, 0, false
	}
	const prefix = "bytes="
	if !strings.HasPrefix(header, prefix) {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(header, prefix)
	if strings.Contains(spec, ",") {
		// Multi-range — not supported here.
		return 0, 0, false
	}
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, 0, false
	}
	startStr := spec[:dash]
	endStr := spec[dash+1:]
	if startStr == "" {
		// Suffix range: "-N" means last N bytes.
		n, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true
	}
	s, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil || s < 0 || s >= size {
		return 0, 0, false
	}
	if endStr == "" {
		return s, size - 1, true
	}
	e, err := strconv.ParseInt(endStr, 10, 64)
	if err != nil || e < s {
		return 0, 0, false
	}
	if e >= size {
		e = size - 1
	}
	return s, e, true
}

// md5MismatchError surfaces from the verifying reader when the body's
// actual MD5 doesn't match the declared Content-MD5. The PutObject
// handler maps it to BadDigest.
type md5MismatchError struct {
	expected []byte
	actual   []byte
}

func (e *md5MismatchError) Error() string {
	return fmt.Sprintf("Content-MD5 mismatch: expected %s, got %s",
		hex.EncodeToString(e.expected), hex.EncodeToString(e.actual))
}

// newMD5VerifyingReader wraps a body reader so that EOF returns an
// error if the cumulative MD5 doesn't match expected. Any user of the
// returned ReadCloser MUST consume to EOF for the verification to
// trigger; partial reads are caught only on Close.
func newMD5VerifyingReader(src io.ReadCloser, expected []byte) io.ReadCloser {
	return &md5VerifyingReader{src: src, expected: expected, hasher: newMD5Hasher()}
}

// logAccess writes a single-line entry mirroring the REST adapter's
// access-log format. accessLog is gated on cfg.Server.AccessLogEnabled
// at construction time.
func (rt *router) logAccess(op, bucket, key, accessKey, extra string) {
	if extra == "" {
		_ = rt
		// Keep call shape stable; logger lives in router.go's Println
		// chain.
		fmt.Printf("[filegate-s3] %s bucket=%s key=%s by=%s\n", op, bucket, key, accessKey)
		return
	}
	fmt.Printf("[filegate-s3] %s bucket=%s key=%s by=%s %s\n", op, bucket, key, accessKey, extra)
}
