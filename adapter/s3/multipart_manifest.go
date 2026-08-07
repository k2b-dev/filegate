package s3

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/k2b-dev/filegate/v3/infra/filesystem"
)

// Multipart upload staging layout.
//
// On disk under each mount we use the existing .fg-uploads staging
// dir; multipart uploads live under their own subprefix so the
// Filegate upload-session cleanup and S3 multipart cleanup don't trip over
// each other:
//
//	<mountAbs>/.fg-uploads/s3-<uploadId>/parts/00001.bin
//	<mountAbs>/.fg-uploads/s3-<uploadId>/parts/00002.bin
//	...
//	<mountAbs>/.fg-uploads/s3-<uploadId>/complete.tmp   (only during Complete)
//
// Active upload metadata and part rows live in Pebble. The manifest
// helpers below are kept for legacy cleanup/recovery tests and old
// staging dirs from pre-Pebble-active builds.
//
// uploadId is 32 lowercase-hex chars (16 random bytes). We pick our
// own format rather than emit AWS-shape uploadIds (long opaque
// base64) because clients treat them as opaque anyway and this
// keeps the on-disk dirname easy to grep/inspect.
const (
	multipartDirPrefix      = "s3-"
	multipartManifestFile   = "manifest.json"
	multipartPartsDirName   = "parts"
	multipartCompleteTmp    = "complete.tmp"
	multipartManifestKind   = "s3-multipart"
	multipartManifestFormat = 1

	// AWS S3 limits.
	multipartMinPartSize  = 5 * 1024 * 1024 // 5 MiB on non-final parts
	multipartMaxPartCount = 10000
)

// Multipart upload phases. Kept in the adapter for legacy manifest
// cleanup; active uploads use domain.MultipartUploadPhase with the
// same string values.
type multipartPhase string

const (
	phaseInProgress multipartPhase = "in_progress" // accepting UploadPart
	phaseCommitting multipartPhase = "committing"  // Complete in flight; recovery needs work
	phaseDone       multipartPhase = "done"        // Complete succeeded; state kept for retention
	phaseAborted    multipartPhase = "aborted"     // AbortMultipartUpload called (or recovery decided abort)
)

// multipartManifest is the legacy manifest.json shape.
type multipartManifest struct {
	Format    int    `json:"format"`
	Kind      string `json:"kind"` // "s3-multipart" — distinguishes old S3 manifests from other staging data
	UploadID  string `json:"upload_id"`
	Bucket    string `json:"bucket"`
	Key       string `json:"key"`
	Initiated int64  `json:"initiated_unix_ms"`

	// Per-PUT user-supplied object metadata that would normally go
	// on the resulting object. Captured at CreateMultipartUpload
	// time and applied by Complete.
	ContentType        string            `json:"content_type,omitempty"`
	ContentEncoding    string            `json:"content_encoding,omitempty"`
	ContentDisposition string            `json:"content_disposition,omitempty"`
	UserMetadata       map[string]string `json:"user_metadata,omitempty"`

	// Per-part state. Keyed by partNumber. Entry exists once the
	// part has been UploadPart'd.
	Parts map[int]multipartPart `json:"parts"`

	Phase multipartPhase `json:"phase"`

	// Push 3 fields — the 2-phase Complete protocol fills these
	// in to support crash recovery. Empty on a fresh upload.
	CompositeETag    string `json:"composite_etag,omitempty"`
	WholeBodyMD5     string `json:"whole_body_md5,omitempty"`
	PreInstallExists bool   `json:"pre_install_exists,omitempty"`
	PreInstallRaw    []byte `json:"pre_install_raw,omitempty"` // base64 bytes of pre-install fgbin entity record
	CompletedFileID  string `json:"completed_file_id,omitempty"`
	CompletedAt      int64  `json:"completed_at_unix_ms,omitempty"`
}

// multipartPart is one part's recorded state.
type multipartPart struct {
	PartNumber int    `json:"part_number"`
	Size       int64  `json:"size"`
	ETag       string `json:"etag"` // hex MD5 of part bytes
	UpdatedAt  int64  `json:"updated_unix_ms"`
}

// multipartLocator names where a given upload's staging dir lives.
type multipartLocator struct {
	MountAbs string // absolute path of the bucket's mount root
	StageDir string // <mountAbs>/.fg-uploads/s3-<uploadId>
	UploadID string
}

// stageDirFor returns the staging directory path for a given mount
// + upload ID. Caller is responsible for ensuring the mount path is
// the real one (we don't validate here).
func stageDirFor(mountAbs, uploadID string) string {
	return filepath.Join(mountAbs, ".fg-uploads", multipartDirPrefix+uploadID)
}

// manifestPathFor returns the manifest.json path for a given
// staging dir.
func manifestPathFor(stageDir string) string {
	return filepath.Join(stageDir, multipartManifestFile)
}

// partPathFor returns the per-part file path for partNumber N.
// Padded so directory listing sorts correctly.
func partPathFor(stageDir string, partNumber int) string {
	return filepath.Join(stageDir, multipartPartsDirName, fmt.Sprintf("%05d.bin", partNumber))
}

// readManifest loads + decodes the manifest at the staging dir.
// Returns ErrNotFound when the dir or manifest is gone (the caller
// surfaces NoSuchUpload to the client).
func readManifest(stageDir string) (*multipartManifest, error) {
	raw, err := os.ReadFile(manifestPathFor(stageDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errMultipartNotFound
		}
		return nil, err
	}
	var m multipartManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("manifest decode: %w", err)
	}
	if m.Kind != multipartManifestKind {
		// Foreign manifest in our subprefix — defensive only;
		// shouldn't happen since we use a unique prefix.
		return nil, fmt.Errorf("manifest kind %q is not %q", m.Kind, multipartManifestKind)
	}
	return &m, nil
}

// writeManifest atomically replaces the manifest at the staging
// dir. Uses the shared infra/filesystem WriteFileAtomic helper so
// crash semantics match the rest of filegate's small-blob writes.
func writeManifest(stageDir string, m *multipartManifest) error {
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return filesystem.WriteFileAtomic(manifestPathFor(stageDir), raw)
}

// errMultipartNotFound is returned when an uploadId doesn't have a
// staging dir / manifest. The router maps this to NoSuchUpload.
var errMultipartNotFound = errors.New("s3: multipart upload not found")

// stageDirForBucket: the inverse — given a bucket name, find its
// mount-abs and synthesize the staging dir for a NEW uploadId.
// Used by CreateMultipartUpload.
func (rt *router) stageDirForBucket(bucket, uploadID string) (multipartLocator, error) {
	for _, root := range rt.svc.ListRoot() {
		if root.Name != bucket {
			continue
		}
		mountAbs, err := rt.svc.ResolveAbsPath(root.ID)
		if err != nil {
			return multipartLocator{}, err
		}
		return multipartLocator{
			MountAbs: mountAbs,
			StageDir: stageDirFor(mountAbs, uploadID),
			UploadID: uploadID,
		}, nil
	}
	return multipartLocator{}, errMultipartNotFound
}
