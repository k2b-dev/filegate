package s3

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/k2b-dev/filegate/v3/domain"
	"github.com/k2b-dev/filegate/v3/infra/filesystem"
)

// handleCreateMultipartUpload implements the POST /{bucket}/{key}?uploads
// flow. Generates a fresh uploadId, captures the per-PUT object
// metadata in active Pebble state, and returns the InitiateMultipartUpload
// XML response.
func (rt *router) handleCreateMultipartUpload(w http.ResponseWriter, r *http.Request, verified *sigV4Result, bucket, key string) {
	if err := validateObjectKey(key); err != nil {
		writeError(w, r, errInvalidArgument, err.Error(), withBucket(bucket), withKey(key))
		return
	}

	// Enforce the same 2 KiB user-metadata budget the single-PUT
	// path enforces. Without this, clients could smuggle arbitrarily
	// large x-amz-meta-* blobs through CreateMultipartUpload —
	// inconsistent with PutObject and an unbounded active-state cost.
	userMeta := collectUserMetadata(r)
	if len(userMeta) > 0 {
		blob, jerr := json.Marshal(userMeta)
		if jerr != nil {
			writeError(w, r, errInvalidArgument, "could not encode x-amz-meta-* headers: "+jerr.Error(), withBucket(bucket), withKey(key))
			return
		}
		if len(blob) > userMetadataMaxBytes {
			writeError(w, r, errInvalidArgument, "x-amz-meta-* headers exceed 2 KiB user-metadata budget", withBucket(bucket), withKey(key))
			return
		}
	}

	uploadID, err := generateUploadID()
	if err != nil {
		writeError(w, r, errInternalError, "could not generate uploadId", withBucket(bucket), withKey(key))
		return
	}
	loc, err := rt.stageDirForBucket(bucket, uploadID)
	if err != nil {
		writeError(w, r, errNoSuchBucket, "bucket does not exist", withBucket(bucket), withKey(key))
		return
	}

	upload := domain.ActiveMultipartUpload{
		UploadID:           uploadID,
		Bucket:             bucket,
		Key:                key,
		StageDir:           loc.StageDir,
		Initiated:          time.Now().UnixMilli(),
		ContentType:        r.Header.Get("Content-Type"),
		ContentEncoding:    r.Header.Get("Content-Encoding"),
		ContentDisposition: r.Header.Get("Content-Disposition"),
		UserMetadata:       userMeta,
		Phase:              domain.MultipartUploadInProgress,
	}
	if err := os.MkdirAll(filepath.Join(loc.StageDir, multipartPartsDirName), 0o755); err != nil {
		writeError(w, r, errInternalError, "could not prepare staging dir", withBucket(bucket), withKey(key))
		return
	}
	if err := rt.svc.CreateActiveMultipartUpload(upload); err != nil {
		_ = os.RemoveAll(loc.StageDir)
		writeError(w, r, errInternalError, "could not record multipart upload", withBucket(bucket), withKey(key))
		return
	}

	res := initiateMultipartUploadResult{
		Bucket:   bucket,
		Key:      key,
		UploadID: uploadID,
	}
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Server", "filegate")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(res)
	if rt.accessLog {
		rt.logAccess("CreateMultipartUpload", bucket, key, verified.AccessKeyID, "uploadId="+uploadID)
	}
}

// handleUploadPart implements PUT /{bucket}/{key}?partNumber=N&uploadId=X.
// Body is the raw part bytes; we tee through MD5 while writing,
// store the part-MD5 in active Pebble state, and return ETag.
//
// AWS spec rules:
//   - partNumber range 1-10000 (errInvalidArgument otherwise)
//   - duplicate UploadPart for the same partNumber overwrites
//   - 5 MiB minimum is enforced at Complete time, not here (since
//     UploadPart doesn't know yet whether this is the final part)
func (rt *router) handleUploadPart(w http.ResponseWriter, r *http.Request, verified *sigV4Result, bucket, key string) {
	q := r.URL.Query()
	partRaw := q.Get("partNumber")
	uploadID := q.Get("uploadId")
	if partRaw == "" || uploadID == "" {
		writeError(w, r, errInvalidArgument, "partNumber and uploadId required", withBucket(bucket), withKey(key))
		return
	}
	partNumber, err := strconv.Atoi(partRaw)
	if err != nil || partNumber < 1 || partNumber > multipartMaxPartCount {
		writeError(w, r, errInvalidArgument, fmt.Sprintf("partNumber must be 1-%d", multipartMaxPartCount), withBucket(bucket), withKey(key))
		return
	}
	upload, err := rt.svc.LookupActiveMultipartUpload(uploadID)
	if err != nil {
		writeError(w, r, errNoSuchUpload, "upload not found", withBucket(bucket), withKey(key))
		return
	}
	if upload.Phase != domain.MultipartUploadInProgress {
		writeError(w, r, errInvalidRequest, "upload is not in_progress", withBucket(bucket), withKey(key))
		return
	}
	if upload.Bucket != bucket || upload.Key != key {
		// Defense against client confusion or replay across keys.
		writeError(w, r, errInvalidArgument, "uploadId belongs to a different bucket/key", withBucket(bucket), withKey(key))
		return
	}

	if err := rt.acquireWriteSlot(r.Context()); err != nil {
		writeError(w, r, errIncompleteBody, "request canceled before write slot", withBucket(bucket), withKey(key))
		return
	}
	defer rt.releaseWriteSlot()

	expectedPartETag := ""
	if v := strings.TrimSpace(r.Header.Get("Content-MD5")); v != "" {
		raw, decErr := base64.StdEncoding.DecodeString(v)
		if decErr != nil || len(raw) != 16 {
			writeError(w, r, errInvalidDigest, "Content-MD5 must be a 16-byte base64-encoded value", withBucket(bucket), withKey(key))
			return
		}
		expectedPartETag = hex.EncodeToString(raw)
	}

	releasePart := rt.partLocks.acquire(fmt.Sprintf("%s:%d", uploadID, partNumber))
	defer releasePart()

	partPath := partPathFor(upload.StageDir, partNumber)
	tmpPath, written, partETag, sigErrV := writePartTempTeeMD5(verified.BodyReader, partPath, expectedPartETag)
	if sigErrV != nil {
		writeError(w, r, sigErrV.Code, sigErrV.Message, withBucket(bucket), withKey(key))
		return
	}
	committedPart := false
	defer func() {
		if !committedPart {
			_ = os.Remove(tmpPath)
		}
	}()

	releaseUpload := rt.uploadLocks.acquire(uploadID)
	defer releaseUpload()
	latest, err := rt.svc.LookupActiveMultipartUpload(uploadID)
	if err != nil {
		writeError(w, r, errNoSuchUpload, "upload not found", withBucket(bucket), withKey(key))
		return
	}
	if latest.Phase != domain.MultipartUploadInProgress {
		writeError(w, r, errInvalidRequest, "upload is not in_progress", withBucket(bucket), withKey(key))
		return
	}
	if latest.Bucket != bucket || latest.Key != key {
		writeError(w, r, errInvalidArgument, "uploadId belongs to a different bucket/key", withBucket(bucket), withKey(key))
		return
	}
	if err := os.Rename(tmpPath, partPath); err != nil {
		writeError(w, r, errInternalError, "rename part: "+err.Error(), withBucket(bucket), withKey(key))
		return
	}
	committedPart = true
	if err := rt.svc.PutActiveMultipartPart(domain.ActiveMultipartPart{
		UploadID:   uploadID,
		PartNumber: partNumber,
		Size:       written,
		ETag:       partETag,
		UpdatedAt:  time.Now().UnixMilli(),
	}); err != nil {
		writeError(w, r, errInternalError, "could not record part", withBucket(bucket), withKey(key))
		return
	}

	w.Header().Set("ETag", quoteETag(partETag))
	w.Header().Set("Server", "filegate")
	w.WriteHeader(http.StatusOK)
	if rt.accessLog {
		rt.logAccess("UploadPart", bucket, key, verified.AccessKeyID, fmt.Sprintf("uploadId=%s part=%d size=%d", uploadID, partNumber, written))
	}
}

// handleAbortMultipartUpload deletes the staging dir for the given
// uploadId. Idempotent: a missing upload returns 204.
func (rt *router) handleAbortMultipartUpload(w http.ResponseWriter, r *http.Request, verified *sigV4Result, bucket, key string) {
	uploadID := r.URL.Query().Get("uploadId")
	if uploadID == "" {
		writeError(w, r, errInvalidArgument, "uploadId required", withBucket(bucket), withKey(key))
		return
	}
	releaseUpload := rt.uploadLocks.acquire(uploadID)
	defer releaseUpload()
	upload, err := rt.svc.LookupActiveMultipartUpload(uploadID)
	if err != nil {
		// Idempotent: already gone.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := rt.svc.DeleteActiveMultipartUpload(uploadID); err != nil {
		writeError(w, r, errInternalError, "could not remove upload state", withBucket(bucket), withKey(key))
		return
	}
	if err := os.RemoveAll(upload.StageDir); err != nil {
		writeError(w, r, errInternalError, "could not remove staging dir", withBucket(bucket), withKey(key))
		return
	}
	w.WriteHeader(http.StatusNoContent)
	if rt.accessLog {
		rt.logAccess("AbortMultipartUpload", bucket, key, verified.AccessKeyID, "uploadId="+uploadID)
	}
}

// handleListParts implements GET /{bucket}/{key}?uploadId=X.
// Returns the parts of an in-progress upload, sorted by partNumber.
func (rt *router) handleListParts(w http.ResponseWriter, r *http.Request, verified *sigV4Result, bucket, key string) {
	uploadID := r.URL.Query().Get("uploadId")
	if uploadID == "" {
		writeError(w, r, errInvalidArgument, "uploadId required", withBucket(bucket), withKey(key))
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
	parts, err := rt.svc.ListActiveMultipartParts(uploadID)
	if err != nil {
		writeError(w, r, errInternalError, "could not list parts", withBucket(bucket), withKey(key))
		return
	}
	res := listPartsResult{
		Bucket:   bucket,
		Key:      key,
		UploadID: uploadID,
	}
	for _, p := range parts {
		res.Parts = append(res.Parts, partXML{
			PartNumber:   p.PartNumber,
			LastModified: time.UnixMilli(p.UpdatedAt).UTC().Format("2006-01-02T15:04:05.000Z"),
			ETag:         quoteETag(p.ETag),
			Size:         p.Size,
		})
	}
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Server", "filegate")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(res)
	if rt.accessLog {
		rt.logAccess("ListParts", bucket, key, verified.AccessKeyID, fmt.Sprintf("uploadId=%s parts=%d", uploadID, len(parts)))
	}
}

// handleListMultipartUploads implements GET /{bucket}?uploads.
// Returns in-progress (and committing) uploads in the bucket.
func (rt *router) handleListMultipartUploads(w http.ResponseWriter, r *http.Request, verified *sigV4Result, bucket string) {
	if _, err := rt.stageDirForBucket(bucket, "" /* uploadId not relevant here */); err != nil {
		writeError(w, r, errNoSuchBucket, "bucket does not exist", withBucket(bucket))
		return
	}
	uploads, err := rt.svc.ListActiveMultipartUploads(bucket)
	if err != nil {
		writeError(w, r, errInternalError, fmt.Sprintf("listing failed: %s", err), withBucket(bucket))
		return
	}
	res := listMultipartUploadsResult{
		Bucket: bucket,
	}
	for _, upload := range uploads {
		if upload.Phase != domain.MultipartUploadInProgress && upload.Phase != domain.MultipartUploadCommitting {
			continue
		}
		res.Uploads = append(res.Uploads, uploadXML{
			Key:       upload.Key,
			UploadID:  upload.UploadID,
			Initiated: time.UnixMilli(upload.Initiated).UTC().Format("2006-01-02T15:04:05.000Z"),
		})
	}
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Server", "filegate")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(res)
	if rt.accessLog {
		rt.logAccess("ListMultipartUploads", bucket, "", verified.AccessKeyID, fmt.Sprintf("count=%d", len(res.Uploads)))
	}
}

// generateUploadID returns a fresh 32-hex-char uploadId. We use 16
// bytes of crypto/rand which is plenty for collision avoidance
// (2^128 keyspace).
func generateUploadID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

// writePartTempTeeMD5 writes body bytes to a unique temp file beside
// partPath while teeing through MD5. The caller publishes the temp file
// with os.Rename only after re-checking active upload phase.
func writePartTempTeeMD5(body io.Reader, partPath string, expectedETag string) (string, int64, string, *sigV4VerifyError) {
	if err := os.MkdirAll(filepath.Dir(partPath), 0o755); err != nil {
		return "", 0, "", sigErr(errInternalError, "could not prepare parts dir: %s", err)
	}
	dir := filepath.Dir(partPath)
	f, err := os.CreateTemp(dir, filepath.Base(partPath)+".*.tmp")
	if err != nil {
		return "", 0, "", sigErr(errInternalError, "could not open part file: %s", err)
	}
	tmp := f.Name()
	hasher := md5.New()
	tee := io.MultiWriter(f, hasher)
	written, err := io.Copy(tee, body)
	// fsync the data file so the rename'd part survives a crash —
	// otherwise a client that uploaded parts over hours could lose
	// them right before Complete.
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		if isNoSpaceError(err) {
			return "", 0, "", sigErr(errInsufficientStorage, "part write: storage is full")
		}
		return "", 0, "", sigErr(errIncompleteBody, "part write: %s", err)
	}
	partETag := hex.EncodeToString(hasher.Sum(nil))
	if expectedETag != "" && !strings.EqualFold(expectedETag, partETag) {
		_ = os.Remove(tmp)
		return "", 0, "", sigErr(errBadDigest, "Content-MD5 does not match part body")
	}
	// Best-effort dir-sync so the temp entry itself is durable before
	// the caller's rename publishes it.
	_ = filesystem.SyncDir(filepath.Dir(tmp))
	return tmp, written, partETag, nil
}

func isNoSpaceError(err error) bool {
	return errors.Is(err, syscall.ENOSPC)
}

// collectUserMetadata extracts x-amz-meta-* headers from the
// request into a flat map. Same shape as buildS3WriteOptions but
// returns the map directly; we serialize to JSON only at Complete
// time (push 3) when we know the final entity slot.
func collectUserMetadata(r *http.Request) map[string]string {
	out := map[string]string{}
	for k, v := range r.Header {
		lk := strings.ToLower(k)
		if !strings.HasPrefix(lk, "x-amz-meta-") {
			continue
		}
		name := strings.TrimPrefix(lk, "x-amz-meta-")
		if name == "" {
			continue
		}
		out[name] = strings.Join(v, ",")
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
