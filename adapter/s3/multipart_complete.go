package s3

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/k2b-dev/filegate/v3/domain"
)

// handleCompleteMultipartUpload finalizes a multipart upload using
// the 2-phase commit protocol described in domain/service_s3_multipart.go:
//
//  1. Look up the upload's staging dir + active state.
//  2. Validate the client-supplied parts list (XML body) against the
//     active part rows: every PartNumber present, ETags match, ascending
//     order, ≥5 MiB on every non-final part.
//  3. Concat parts in order into <stage>/complete.tmp while computing
//     the composite ETag — hex(MD5(concat-of-part-MD5-bytes)) + "-N".
//  4. Mark active state phase=committing (so a crash mid-install can be
//     reconciled by the recovery sweep).
//  5. Call domain.CompleteMultipartUpload, which atomically renames
//     complete.tmp into place and writes the durable uploadId record
//     in the same Pebble batch. The domain layer owns the path-lock
//     + idempotency check.
//  6. Mark active state phase=done with the result snapshot. The cleanup
//     loop GCs done active rows on a retention timer.
//
// Idempotency: the durable record (Pebble) is the source of truth;
// a retried Complete that finds an existing record returns the historical result.
func (rt *router) handleCompleteMultipartUpload(w http.ResponseWriter, r *http.Request, verified *sigV4Result, bucket, key string) {
	uploadID := r.URL.Query().Get("uploadId")
	if uploadID == "" {
		writeError(w, r, errInvalidArgument, "uploadId required", withBucket(bucket), withKey(key))
		return
	}
	if err := validateObjectKey(key); err != nil {
		writeError(w, r, errInvalidArgument, err.Error(), withBucket(bucket), withKey(key))
		return
	}
	releaseUpload := rt.uploadLocks.acquire(uploadID)
	defer releaseUpload()

	// Decode the 32-hex-char on-disk uploadId into the 16-byte form
	// the durable record expects. We pin this here so a malformed
	// dirname (shouldn't happen — we generate them) surfaces as
	// InternalError rather than corrupting the keyspace.
	var uploadIDBytes [16]byte
	if raw, decErr := hex.DecodeString(uploadID); decErr != nil || len(raw) != 16 {
		writeError(w, r, errInternalError, "uploadId is not 16-byte hex", withBucket(bucket), withKey(key))
		return
	} else {
		copy(uploadIDBytes[:], raw)
	}

	if record, err := rt.svc.LookupMultipartUploadRecord(uploadIDBytes); err == nil && record != nil {
		if record.Bucket != bucket || record.Key != key {
			writeError(w, r, errInvalidArgument, "uploadId belongs to a different bucket/key", withBucket(bucket), withKey(key))
			return
		}
		writeCompleteResultFromRecord(w, r, bucket, key, record)
		return
	} else if err != nil && !errors.Is(err, domain.ErrNotFound) {
		writeError(w, r, errInternalError, "could not read multipart upload record", withBucket(bucket), withKey(key))
		return
	}

	upload, err := rt.svc.LookupActiveMultipartUpload(uploadID)
	if err != nil {
		writeError(w, r, errNoSuchUpload, "upload not found", withBucket(bucket), withKey(key))
		return
	}
	if upload.Bucket != bucket || upload.Key != key {
		writeError(w, r, errInvalidArgument, "uploadId belongs to a different bucket/key", withBucket(bucket), withKey(key))
		return
	}
	if upload.Phase == domain.MultipartUploadDone {
		writeError(w, r, errNoSuchUpload, "upload has no durable completion record", withBucket(bucket), withKey(key))
		return
	}
	if upload.Phase == domain.MultipartUploadAborted {
		writeError(w, r, errNoSuchUpload, "upload was aborted", withBucket(bucket), withKey(key))
		return
	}

	// Parse the request body XML.
	bodyRaw, readErr := io.ReadAll(verified.BodyReader)
	if readErr != nil {
		writeError(w, r, errIncompleteBody, "could not read request body", withBucket(bucket), withKey(key))
		return
	}
	var req completeMultipartUploadRequest
	if err := xml.Unmarshal(bodyRaw, &req); err != nil {
		writeError(w, r, errMalformedXML, fmt.Sprintf("body must be a CompleteMultipartUpload XML document: %s", err), withBucket(bucket), withKey(key))
		return
	}
	if len(req.Parts) == 0 {
		writeError(w, r, errMalformedXML, "CompleteMultipartUpload requires at least one part", withBucket(bucket), withKey(key))
		return
	}

	if upload.Phase == domain.MultipartUploadInProgress {
		upload.Phase = domain.MultipartUploadCommitting
		if err := rt.svc.UpdateActiveMultipartUpload(*upload); err != nil {
			writeError(w, r, errInternalError, "could not mark upload committing", withBucket(bucket), withKey(key))
			return
		}
	}

	activeParts, err := rt.svc.ListActiveMultipartParts(uploadID)
	if err != nil {
		writeError(w, r, errInternalError, "could not list uploaded parts", withBucket(bucket), withKey(key))
		return
	}
	validatedParts, validateErr := validateCompleteParts(req.Parts, activeParts)
	if validateErr != nil {
		writeError(w, r, validateErr.Code, validateErr.Message, withBucket(bucket), withKey(key))
		return
	}

	// Concat parts → complete.tmp while computing the composite ETag.
	// This is the adapter-measured phase of the trace-substitute
	// histogram; the domain returns the other three (lock/hash/batch).
	completeTmp := filepath.Join(upload.StageDir, multipartCompleteTmp)
	concatStart := time.Now()
	composite, concatErr := concatPartsAndComputeCompositeETag(upload.StageDir, validatedParts, completeTmp)
	concatDur := time.Since(concatStart)
	if concatErr != nil {
		_ = os.Remove(completeTmp)
		writeError(w, r, errInternalError, fmt.Sprintf("could not assemble parts: %s", concatErr), withBucket(bucket), withKey(key))
		return
	}

	upload.CompositeETag = composite
	if err := rt.svc.UpdateActiveMultipartUpload(*upload); err != nil {
		_ = os.Remove(completeTmp)
		writeError(w, r, errInternalError, "could not update upload state", withBucket(bucket), withKey(key))
		return
	}

	// Build the S3 write options from the captured CreateMultipartUpload fields.
	// Note: IfMatch / IfNoneMatchAny are NOT honored on Complete by
	// AWS — the domain layer ignores them on the multipart path.
	var userMeta []byte
	if len(upload.UserMetadata) > 0 {
		blob, jerr := json.Marshal(upload.UserMetadata)
		if jerr != nil {
			writeError(w, r, errInternalError, "could not encode user metadata", withBucket(bucket), withKey(key))
			return
		}
		userMeta = blob
	}
	opts := domain.S3WriteOptions{
		ContentType:        upload.ContentType,
		ContentEncoding:    upload.ContentEncoding,
		ContentDisposition: upload.ContentDisposition,
		UserMetadata:       userMeta,
	}

	// Hand off to the domain layer. It owns the path-lock, the
	// rename, and the durable Pebble batch.
	result, domErr := rt.svc.CompleteMultipartUpload(domain.MultipartCompleteArgs{
		VirtualPath:   virtualPathFor(bucket, key),
		SrcPath:       completeTmp,
		UploadID:      uploadIDBytes,
		CompositeETag: composite,
		Opts:          opts,
	})
	if domErr != nil {
		// complete.tmp is left behind on failure — the recovery sweep
		// (or an Abort) will clean the staging dir. Don't remove it inline.
		mapDomainError(w, r, domErr, bucket, key)
		return
	}

	// Observe the four Complete sub-phases. Skipped on the Replayed
	// fast path (no install happened, timings are zero). nil-safe.
	if !result.Replayed {
		rt.metrics.ObserveCompletePhase("concat", concatDur.Seconds())
		rt.metrics.ObserveCompletePhase("lock_wait", result.Timings.LockWait.Seconds())
		rt.metrics.ObserveCompletePhase("hash", result.Timings.Hash.Seconds())
		rt.metrics.ObserveCompletePhase("pebble_batch", result.Timings.PebbleBatch.Seconds())
	}

	upload.Phase = domain.MultipartUploadDone
	upload.WholeBodyMD5 = ""
	if result.Meta != nil {
		upload.WholeBodyMD5 = result.Meta.ETag
		upload.CompletedFileID = result.Meta.ID.String()
	}
	upload.CompletedAt = time.Now().UnixMilli()
	if err := rt.svc.UpdateActiveMultipartUpload(*upload); err != nil {
		fmt.Printf("[filegate-s3] CompleteMultipartUpload: post-commit active state write failed: %s\n", err)
	}

	// Drop the parts/ directory and the assembled complete.tmp now
	// that the durable commit has landed. The active done row stays
	// for retention/cleanup bookkeeping; retry-Complete is served by
	// the durable Pebble record.
	cleanupCompletedStaging(upload.StageDir)

	// Build the response.
	res := completeMultipartUploadResult{
		Location: locationFor(r, bucket, key),
		Bucket:   bucket,
		Key:      key,
		ETag:     quoteETag(composite),
	}
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Server", "filegate")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(res)

	if rt.accessLog {
		extra := fmt.Sprintf("uploadId=%s parts=%d etag=%s replayed=%t", uploadID, len(validatedParts), composite, result.Replayed)
		rt.logAccess("CompleteMultipartUpload", bucket, key, verified.AccessKeyID, extra)
	}
}

// validateCompleteParts checks the client-supplied parts list against
// active part rows. AWS rules (errors mirror real S3):
//
//   - Each PartNumber must exist in active state (InvalidPart).
//   - Per-part ETag must match the stored MD5 (InvalidPart).
//   - The list must be in strictly ascending PartNumber order
//     (InvalidPartOrder). Duplicates are also InvalidPartOrder.
//   - Every part except the LAST must be ≥ 5 MiB (EntityTooSmall).
//
// On success returns the list as resolved parts in order.
func validateCompleteParts(reqParts []completeRequestPart, storedParts []domain.ActiveMultipartPart) ([]multipartPart, *sigV4VerifyError) {
	byNumber := make(map[int]domain.ActiveMultipartPart, len(storedParts))
	for _, part := range storedParts {
		byNumber[part.PartNumber] = part
	}
	out := make([]multipartPart, 0, len(reqParts))
	prev := 0
	for _, rp := range reqParts {
		if rp.PartNumber < 1 || rp.PartNumber > multipartMaxPartCount {
			return nil, sigErr(errInvalidPart, "partNumber %d out of range 1-%d", rp.PartNumber, multipartMaxPartCount)
		}
		if rp.PartNumber <= prev {
			return nil, sigErr(errInvalidPartOrder, "parts must appear in strictly ascending PartNumber order")
		}
		prev = rp.PartNumber
		stored, ok := byNumber[rp.PartNumber]
		if !ok {
			return nil, sigErr(errInvalidPart, "partNumber %d was not uploaded", rp.PartNumber)
		}
		// Strip surrounding quotes — clients quote ETags on the wire,
		// but rclone and a few SDKs send unquoted. Be liberal.
		clientETag := strings.Trim(strings.TrimSpace(rp.ETag), `"`)
		if !strings.EqualFold(clientETag, stored.ETag) {
			return nil, sigErr(errInvalidPart, "partNumber %d ETag does not match upload (expected %q, got %q)", rp.PartNumber, stored.ETag, clientETag)
		}
		out = append(out, multipartPart{
			PartNumber: stored.PartNumber,
			Size:       stored.Size,
			ETag:       stored.ETag,
			UpdatedAt:  stored.UpdatedAt,
		})
	}
	// 5 MiB minimum on every part except the last. The last part can
	// be any size (including very small) — that's how S3 handles
	// small final chunks.
	for i := 0; i < len(out)-1; i++ {
		if out[i].Size < multipartMinPartSize {
			return nil, sigErr(errEntityTooSmall, "partNumber %d is %d bytes; minimum is %d on non-final parts", out[i].PartNumber, out[i].Size, multipartMinPartSize)
		}
	}
	return out, nil
}

// concatPartsAndComputeCompositeETag streams every part's bytes
// into completeTmp (atomically via tmp+rename) while accumulating
// the per-part MD5s into the composite digest:
//
//	composite = hex(MD5(md5_part1_bytes ‖ md5_part2_bytes ‖ ...)) + "-N"
//
// The per-part MD5 bytes are the RAW 16-byte digests, NOT the hex
// strings — this is what AWS specifies and what every S3 client
// computes locally to verify.
func concatPartsAndComputeCompositeETag(stageDir string, parts []multipartPart, completeTmp string) (string, error) {
	tmp := completeTmp + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", fmt.Errorf("open complete.tmp: %w", err)
	}
	composite := md5.New()
	defer func() {
		if out != nil {
			_ = out.Close()
		}
		_ = os.Remove(tmp)
	}()

	// Sort defensively — validateCompleteParts already returns in
	// ascending order, but the cost is negligible.
	sort.Slice(parts, func(i, j int) bool { return parts[i].PartNumber < parts[j].PartNumber })

	for _, p := range parts {
		raw, decErr := hex.DecodeString(p.ETag)
		if decErr != nil || len(raw) != 16 {
			return "", fmt.Errorf("part %d has malformed stored ETag %q", p.PartNumber, p.ETag)
		}
		composite.Write(raw)

		partPath := partPathFor(stageDir, p.PartNumber)
		f, openErr := os.Open(partPath)
		if openErr != nil {
			return "", fmt.Errorf("open part %d: %w", p.PartNumber, openErr)
		}
		_, copyErr := io.Copy(out, f)
		_ = f.Close()
		if copyErr != nil {
			return "", fmt.Errorf("copy part %d: %w", p.PartNumber, copyErr)
		}
	}

	if err := out.Sync(); err != nil {
		return "", fmt.Errorf("sync complete.tmp: %w", err)
	}
	if err := out.Close(); err != nil {
		return "", fmt.Errorf("close complete.tmp: %w", err)
	}
	out = nil
	if err := os.Rename(tmp, completeTmp); err != nil {
		return "", fmt.Errorf("rename complete.tmp: %w", err)
	}

	digest := hex.EncodeToString(composite.Sum(nil))
	return fmt.Sprintf("%s-%d", digest, len(parts)), nil
}

func writeCompleteResultFromRecord(w http.ResponseWriter, r *http.Request, bucket, key string, record *domain.MultipartUploadRecord) {
	res := completeMultipartUploadResult{
		Location: locationFor(r, bucket, key),
		Bucket:   bucket,
		Key:      key,
		ETag:     quoteETag(record.CompositeETag),
	}
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Server", "filegate")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(res)
}

// cleanupCompletedStaging removes the bulky staging artifacts of a
// finished multipart upload (parts/ and complete.tmp). Active done
// state remains in Pebble until the cleanup retention window retires
// it with the durable record. Errors are logged but don't fail the
// request — the final object and commit witness are already durable.
func cleanupCompletedStaging(stageDir string) {
	partsDir := filepath.Join(stageDir, multipartPartsDirName)
	if err := os.RemoveAll(partsDir); err != nil {
		fmt.Printf("[filegate-s3] CompleteMultipartUpload: remove parts dir %s: %s\n", partsDir, err)
	}
	completeTmp := filepath.Join(stageDir, multipartCompleteTmp)
	if err := os.Remove(completeTmp); err != nil && !os.IsNotExist(err) {
		fmt.Printf("[filegate-s3] CompleteMultipartUpload: remove complete.tmp %s: %s\n", completeTmp, err)
	}
}

// locationFor synthesizes the Location header value AWS clients
// echo back. It's best-effort — clients overwhelmingly read Bucket
// + Key and ignore Location, so a slightly off scheme doesn't break
// anything. We pick https when the request was TLS-terminated,
// otherwise http.
func locationFor(r *http.Request, bucket, key string) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = "filegate"
	}
	return fmt.Sprintf("%s://%s/%s/%s", scheme, host, bucket, key)
}
