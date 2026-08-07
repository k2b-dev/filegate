package pebble

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sort"
	"strings"
	"sync"

	"github.com/cockroachdb/pebble"

	"github.com/k2b-dev/filegate/v3/domain"
	"github.com/k2b-dev/filegate/v3/infra/fgbin"
)

const (
	familyEntity byte = 0x01
	familyChild  byte = 0x02
	// 0x03 is intentionally skipped — the legacy inode keyspace lived
	// there before its removal at format version 6. Reusing 0x03 would
	// risk collisions with stale bytes on indexes that didn't get a
	// clean rebuild between bumps.
	familyVersion byte = 0x04
	// 0x05 is reserved for future use.
	//
	// familyMultipartUpload is the durable record per S3 multipart
	// upload. Keyed as
	//   0x07 + uploadId_bytes (16) → fgbin-encoded MultipartUploadIDRecord.
	// Used by CompleteMultipartUpload's 2-phase commit: an entry
	// here is the authoritative "this Complete already succeeded"
	// signal. Retries that find an entry return its stored result
	// idempotently. Records are GC'd after a 24h retention window.
	familyMultipartUpload byte = 0x07
	// familyActiveMultipart is the best-effort state for active S3
	// multipart uploads before Complete has been acknowledged. Keyed as:
	//   0x08 + 'm' + uploadId_hex → JSON ActiveMultipartUpload
	//   0x08 + 'p' + uploadId_hex + 0x00 + partNumber_u16 → JSON ActiveMultipartPart
	//
	// Part bytes stay on the filesystem; Pebble only tracks small state rows.
	familyActiveMultipart byte = 0x08
	// familyUploadSession is the REST resumable upload-session state:
	//   0x09 + 's' + sessionId → JSON UploadSession
	//   0x09 + 'g' + sessionId + 0x00 + segmentIndex_u32 → JSON UploadSegment
	//   0x09 + 'c' + sessionId → JSON UploadCommitRecord
	//
	// Segment bytes stay on the filesystem; Pebble is the metadata source of truth.
	familyUploadSession byte = 0x09

	// familyFlatKey is the bucket+relpath → fileID secondary index,
	// added at format version 9. Keyed as
	//   0x06 + mount_name_bytes + 0x00 + utf8_rel_path → 16-byte fileID.
	// Only files have flat-key entries — directories aren't S3 objects.
	// Mount names cannot contain 0x00 (already a bucket-name rule) so
	// the separator is unambiguous. The index lets S3 ListObjectsV2 do
	// O(log n + result) prefix scans, and gives REST path-lookups and
	// glob-search a free speedup as a bonus.
	familyFlatKey byte = 0x06

	// currentIndexFormatVersion was bumped from 10 to 11 when the
	// canonical SHA-256 fingerprint extension was added to entity rows.
	currentIndexFormatVersion uint16 = 11
)

// ErrUnsupportedIndexFormat is returned when the on-disk index version is incompatible.
var ErrUnsupportedIndexFormat = errors.New("unsupported index format version")

// ErrIndexClosed is returned when operations are attempted on a closed index.
var ErrIndexClosed = errors.New("index closed")

// ErrIndexUnavailable is returned when a fatal Pebble error has been recorded.
var ErrIndexUnavailable = errors.New("index unavailable")

// IsTerminalError reports whether err signals that the index is no longer
// usable and the caller's loop should stop. Recognises ErrIndexClosed,
// ErrIndexUnavailable, and the raw "pebble: closed" string returned by
// background pebble goroutines that bypass the index runOp wrapper.
func IsTerminalError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrIndexClosed) || errors.Is(err, ErrIndexUnavailable) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "pebble: closed")
}

var indexFormatVersionKey = []byte{0x00, 'f', 'g', ':', 'i', 'd', 'x', ':', 'f', 'm', 't'}

// Index is a thread-safe metadata store backed by a Pebble database.
type Index struct {
	mu     sync.RWMutex
	db     *pebble.DB
	closed bool

	// fatalErr has its own mutex because markFatal is called from
	// recoverIntoError while the calling goroutine still holds i.mu
	// (read-locked), and from Pebble's logger goroutines. Taking i.mu
	// here would self-deadlock.
	fatalMu  sync.Mutex
	fatalErr error
}

// Open creates or opens a Pebble-backed index at the given path.
func Open(path string, blockCacheBytes int64) (*Index, error) {
	cache := pebble.NewCache(blockCacheBytes)
	// pebble.Open takes its own reference; release ours so the cache
	// memory is freed when the DB closes.
	defer cache.Unref()
	pebbleLogger := &nonFatalPebbleLogger{}
	opts := &pebble.Options{
		Cache: cache,
		Levels: []pebble.LevelOptions{{
			Compression: pebble.ZstdCompression,
		}},
		Logger: pebbleLogger,
	}
	db, err := pebble.Open(path, opts)
	if err != nil {
		return nil, err
	}
	idx := &Index{db: db}
	pebbleLogger.onFatal = idx.markFatal
	if err := ensureIndexFormatVersion(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return idx, nil
}

type nonFatalPebbleLogger struct {
	onFatal func(error)
}

func (l *nonFatalPebbleLogger) Infof(format string, args ...interface{}) {
	log.Printf(format, args...)
}

func (l *nonFatalPebbleLogger) Errorf(format string, args ...interface{}) {
	log.Printf(format, args...)
}

// Fatalf is called by Pebble on unrecoverable internal errors. The Pebble
// Logger contract requires Fatalf to not return (like log.Fatalf). We mark the
// index as fatally failed via onFatal so subsequent operations return errors
// gracefully, then panic to satisfy the contract. The panic is recoverable by
// callers if needed, which is safer than os.Exit(1).
func (l *nonFatalPebbleLogger) Fatalf(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	err := fmt.Errorf("%w: %s", ErrIndexUnavailable, msg)
	if l.onFatal != nil {
		l.onFatal(err)
	}
	panic(err)
}

func (i *Index) markFatal(err error) {
	i.fatalMu.Lock()
	defer i.fatalMu.Unlock()
	if i.fatalErr == nil {
		i.fatalErr = err
	}
}

func (i *Index) loadFatal() error {
	i.fatalMu.Lock()
	defer i.fatalMu.Unlock()
	return i.fatalErr
}

func (i *Index) currentStateLocked() error {
	if i.closed {
		return ErrIndexClosed
	}
	return i.loadFatal()
}

func normalizeIndexErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrIndexClosed) || errors.Is(err, ErrIndexUnavailable) {
		return err
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "pebble: closed") {
		return ErrIndexClosed
	}
	if strings.Contains(msg, "fatal commit error") {
		return fmt.Errorf("%w: %v", ErrIndexUnavailable, err)
	}
	return err
}

func encodeFormatVersion(v uint16) []byte {
	var out [2]byte
	binary.BigEndian.PutUint16(out[:], v)
	return out[:]
}

func decodeFormatVersion(v []byte) (uint16, error) {
	if len(v) != 2 {
		return 0, fmt.Errorf("invalid index format version payload")
	}
	return binary.BigEndian.Uint16(v), nil
}

func ensureIndexFormatVersion(db *pebble.DB) error {
	value, closer, err := db.Get(indexFormatVersionKey)
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			return db.Set(indexFormatVersionKey, encodeFormatVersion(currentIndexFormatVersion), pebble.Sync)
		}
		return err
	}
	defer closer.Close()
	found, err := decodeFormatVersion(value)
	if err != nil {
		return err
	}
	if found != currentIndexFormatVersion {
		return fmt.Errorf("%w: %d (expected %d)", ErrUnsupportedIndexFormat, found, currentIndexFormatVersion)
	}
	return nil
}

func entityKey(id domain.FileID) []byte {
	key := make([]byte, 1+16)
	key[0] = familyEntity
	copy(key[1:], id[:])
	return key
}

func childPrefix(parentID domain.FileID) []byte {
	prefix := make([]byte, 1+16)
	prefix[0] = familyChild
	copy(prefix[1:], parentID[:])
	return prefix
}

func childTypeByte(isDir bool) byte {
	if isDir {
		return 0
	}
	return 1
}

func childKey(parentID domain.FileID, isDir bool, name string) []byte {
	p := childPrefix(parentID)
	p = append(p, childTypeByte(isDir), 0)
	return append(p, []byte(name)...)
}

// multipartUploadKey builds the Pebble key for a multipart-upload
// record. uploadID is 16 bytes (we hex-encode for the manifest dir
// name; the raw bytes go into the key).
func multipartUploadKey(uploadID [16]byte) []byte {
	out := make([]byte, 1+16)
	out[0] = familyMultipartUpload
	copy(out[1:], uploadID[:])
	return out
}

func activeMultipartUploadPrefix() []byte {
	return []byte{familyActiveMultipart, 'm'}
}

func activeMultipartUploadKey(uploadID string) []byte {
	prefix := activeMultipartUploadPrefix()
	out := make([]byte, len(prefix)+len(uploadID))
	copy(out, prefix)
	copy(out[len(prefix):], uploadID)
	return out
}

func activeMultipartPartPrefix(uploadID string) []byte {
	out := make([]byte, 0, 1+1+len(uploadID)+1)
	out = append(out, familyActiveMultipart, 'p')
	out = append(out, []byte(uploadID)...)
	out = append(out, 0)
	return out
}

func activeMultipartPartKey(uploadID string, partNumber int) []byte {
	prefix := activeMultipartPartPrefix(uploadID)
	out := make([]byte, len(prefix)+2)
	copy(out, prefix)
	binary.BigEndian.PutUint16(out[len(prefix):], uint16(partNumber))
	return out
}

func uploadSessionPrefix() []byte {
	return []byte{familyUploadSession, 's'}
}

func uploadSessionKey(sessionID string) []byte {
	prefix := uploadSessionPrefix()
	out := make([]byte, len(prefix)+len(sessionID))
	copy(out, prefix)
	copy(out[len(prefix):], sessionID)
	return out
}

func uploadSegmentPrefix(sessionID string) []byte {
	out := make([]byte, 0, 1+1+len(sessionID)+1)
	out = append(out, familyUploadSession, 'g')
	out = append(out, []byte(sessionID)...)
	out = append(out, 0)
	return out
}

func uploadSegmentKey(sessionID string, index int) []byte {
	prefix := uploadSegmentPrefix(sessionID)
	out := make([]byte, len(prefix)+4)
	copy(out, prefix)
	binary.BigEndian.PutUint32(out[len(prefix):], uint32(index))
	return out
}

func uploadCommitRecordKey(sessionID string) []byte {
	out := make([]byte, 0, 1+1+len(sessionID))
	out = append(out, familyUploadSession, 'c')
	out = append(out, []byte(sessionID)...)
	return out
}

// flatKeyMountPrefix returns the byte prefix that bounds all flat-key
// entries under the named mount: 0x06 + mountName + 0x00.
func flatKeyMountPrefix(mountName string) []byte {
	out := make([]byte, 0, 1+len(mountName)+1)
	out = append(out, familyFlatKey)
	out = append(out, []byte(mountName)...)
	out = append(out, 0)
	return out
}

// flatKeyForPath returns the byte key for (mountName, relPath).
// relPath is the file's path within the mount, separated by "/", with
// no leading slash. Caller is responsible for using a sanitized
// relPath (no "."/".." segments, no trailing slash).
func flatKeyForPath(mountName, relPath string) []byte {
	prefix := flatKeyMountPrefix(mountName)
	out := make([]byte, len(prefix)+len(relPath))
	copy(out, prefix)
	copy(out[len(prefix):], relPath)
	return out
}

// flatKeySplit decodes a stored key back into (mountName, relPath).
// Returns ok=false if the bytes don't have the expected shape.
func flatKeySplit(key []byte) (mountName, relPath string, ok bool) {
	if len(key) < 2 || key[0] != familyFlatKey {
		return "", "", false
	}
	sep := -1
	for i := 1; i < len(key); i++ {
		if key[i] == 0 {
			sep = i
			break
		}
	}
	if sep < 0 {
		return "", "", false
	}
	return string(key[1:sep]), string(key[sep+1:]), true
}

func prefixUpperBound(prefix []byte) []byte {
	if len(prefix) == 0 {
		return nil
	}
	upper := append([]byte(nil), prefix...)
	for i := len(upper) - 1; i >= 0; i-- {
		if upper[i] != 0xFF {
			upper[i]++
			return upper[:i+1]
		}
	}
	return nil
}

func encodeEntity(e domain.Entity) ([]byte, error) {
	exifJSON, _ := json.Marshal(e.Exif)
	rec := fgbin.Entity{
		ID:       [16]byte(e.ID),
		ParentID: [16]byte(e.ParentID),
		IsDir:    e.IsDir,
		Size:     e.Size,
		MtimeNs:  e.Mtime * int64(1_000_000),
		UID:      e.UID,
		GID:      e.GID,
		Mode:     e.Mode,
		Device:   e.Device,
		Inode:    e.Inode,
		Nlink:    e.Nlink,
		Name:     e.Name,
		MimeType: e.MimeType,
	}
	if len(exifJSON) > 0 && string(exifJSON) != "{}" {
		rec.Extensions = append(rec.Extensions, fgbin.Extension{
			FieldID: fgbin.FieldEXIF,
			Value:   exifJSON,
		})
	}
	// Optional fields: only emitted when set, so legacy rows and entities
	// that never went through an S3 write encode to the same bytes as
	// before. Encoder sorts by FieldID and rejects duplicates, so order
	// here doesn't matter — but keeping it monotonic helps reviewers.
	if e.ETagMD5 != "" {
		rec.Extensions = append(rec.Extensions, fgbin.Extension{
			FieldID: fgbin.FieldETagMD5,
			Value:   []byte(e.ETagMD5),
		})
	}
	if e.SHA256 != "" {
		rec.Extensions = append(rec.Extensions, fgbin.Extension{
			FieldID: fgbin.FieldSHA256,
			Value:   []byte(e.SHA256),
		})
	}
	if e.MultipartETag != "" {
		rec.Extensions = append(rec.Extensions, fgbin.Extension{
			FieldID: fgbin.FieldMultipartETag,
			Value:   []byte(e.MultipartETag),
		})
	}
	if e.ContentType != "" {
		rec.Extensions = append(rec.Extensions, fgbin.Extension{
			FieldID: fgbin.FieldContentType,
			Value:   []byte(e.ContentType),
		})
	}
	if e.ContentEncoding != "" {
		rec.Extensions = append(rec.Extensions, fgbin.Extension{
			FieldID: fgbin.FieldContentEncoding,
			Value:   []byte(e.ContentEncoding),
		})
	}
	if e.ContentDisposition != "" {
		rec.Extensions = append(rec.Extensions, fgbin.Extension{
			FieldID: fgbin.FieldContentDisposition,
			Value:   []byte(e.ContentDisposition),
		})
	}
	if len(e.S3UserMetadata) > 0 {
		rec.Extensions = append(rec.Extensions, fgbin.Extension{
			FieldID: fgbin.FieldS3UserMetadata,
			Value:   append([]byte(nil), e.S3UserMetadata...),
		})
	}
	return fgbin.EncodeEntity(rec)
}

func decodeEntity(id domain.FileID, value []byte) (domain.Entity, error) {
	rec, err := fgbin.DecodeEntity(value)
	if err != nil {
		return domain.Entity{}, err
	}
	if domain.FileID(rec.ID) != id {
		return domain.Entity{}, fmt.Errorf("entity id mismatch")
	}
	var exif map[string]string
	if raw, ok := fgbin.ExtensionByID(rec.Extensions, fgbin.FieldEXIF); ok && len(raw) > 0 {
		exif = make(map[string]string)
		if err := json.Unmarshal(raw, &exif); err != nil {
			// Corrupt EXIF blob — keep the entity readable but surface the
			// fact that on-disk metadata is malformed so it can be repaired.
			log.Printf("[filegate] index: dropping malformed EXIF for %s: %v", id, err)
			exif = nil
		}
	}

	// Optional S3-related fields. ExtensionByID returns (nil, false) for
	// missing IDs, so legacy rows decode with empty strings/slices for
	// all of these — semantically the same as "never set".
	var etagMD5, sha256, multipartETag, contentType, contentEncoding, contentDisposition string
	if raw, ok := fgbin.ExtensionByID(rec.Extensions, fgbin.FieldETagMD5); ok {
		etagMD5 = string(raw)
	}
	if raw, ok := fgbin.ExtensionByID(rec.Extensions, fgbin.FieldSHA256); ok {
		sha256 = string(raw)
	}
	if raw, ok := fgbin.ExtensionByID(rec.Extensions, fgbin.FieldMultipartETag); ok {
		multipartETag = string(raw)
	}
	if raw, ok := fgbin.ExtensionByID(rec.Extensions, fgbin.FieldContentType); ok {
		contentType = string(raw)
	}
	if raw, ok := fgbin.ExtensionByID(rec.Extensions, fgbin.FieldContentEncoding); ok {
		contentEncoding = string(raw)
	}
	if raw, ok := fgbin.ExtensionByID(rec.Extensions, fgbin.FieldContentDisposition); ok {
		contentDisposition = string(raw)
	}
	var s3Meta []byte
	if raw, ok := fgbin.ExtensionByID(rec.Extensions, fgbin.FieldS3UserMetadata); ok {
		s3Meta = raw // ExtensionByID already returns a fresh copy
	}

	return domain.Entity{
		ID:                 id,
		ParentID:           domain.FileID(rec.ParentID),
		Name:               rec.Name,
		IsDir:              rec.IsDir,
		Size:               rec.Size,
		Mtime:              rec.MtimeNs / int64(1_000_000),
		UID:                rec.UID,
		GID:                rec.GID,
		Mode:               rec.Mode,
		Device:             rec.Device,
		Inode:              rec.Inode,
		Nlink:              rec.Nlink,
		MimeType:           rec.MimeType,
		Exif:               exif,
		ETagMD5:            etagMD5,
		SHA256:             sha256,
		MultipartETag:      multipartETag,
		ContentType:        contentType,
		ContentEncoding:    contentEncoding,
		ContentDisposition: contentDisposition,
		S3UserMetadata:     s3Meta,
	}, nil
}

func encodeChild(entry domain.DirEntry) ([]byte, error) {
	return fgbin.EncodeChild(fgbin.Child{
		ID:      [16]byte(entry.ID),
		IsDir:   entry.IsDir,
		Size:    entry.Size,
		MtimeNs: entry.Mtime * int64(1_000_000),
		Name:    entry.Name,
	})
}

func decodeChild(name string, value []byte) (domain.DirEntry, error) {
	rec, err := fgbin.DecodeChild(value)
	if err != nil {
		return domain.DirEntry{}, err
	}
	if rec.Name != name {
		return domain.DirEntry{}, fmt.Errorf("child name mismatch")
	}
	return domain.DirEntry{
		ID:    domain.FileID(rec.ID),
		Name:  rec.Name,
		IsDir: rec.IsDir,
		Size:  rec.Size,
		Mtime: rec.MtimeNs / int64(1_000_000),
	}, nil
}

// versionPrefix returns the key prefix for all versions of fileID. Used for
// "list all versions of file X" iterations.
func versionPrefix(fileID domain.FileID) []byte {
	prefix := make([]byte, 1+16)
	prefix[0] = familyVersion
	copy(prefix[1:], fileID[:])
	return prefix
}

// versionKey returns the full Pebble key for a single version. The
// VersionID is a UUIDv7, whose first 6 bytes are the BigEndian unix-ms
// timestamp — appending it to the file prefix gives natural chronological
// sort within a file with the random tail breaking ms collisions.
func versionKey(fileID domain.FileID, versionID domain.VersionID) []byte {
	key := make([]byte, 1+16+16)
	key[0] = familyVersion
	copy(key[1:17], fileID[:])
	copy(key[17:], versionID[:])
	return key
}

func encodeVersion(meta domain.VersionMeta) ([]byte, error) {
	return fgbin.EncodeVersion(fgbin.Version{
		VersionID: [16]byte(meta.VersionID),
		FileID:    [16]byte(meta.FileID),
		Timestamp: meta.Timestamp,
		Size:      meta.Size,
		Mode:      meta.Mode,
		DeletedAt: meta.DeletedAt,
		Pinned:    meta.Pinned,
		Label:     []byte(meta.Label),
		MountName: []byte(meta.MountName),
	})
}

func decodeVersion(value []byte) (domain.VersionMeta, error) {
	rec, err := fgbin.DecodeVersion(value)
	if err != nil {
		return domain.VersionMeta{}, err
	}
	return domain.VersionMeta{
		VersionID: domain.VersionID(rec.VersionID),
		FileID:    domain.FileID(rec.FileID),
		Timestamp: rec.Timestamp,
		Size:      rec.Size,
		Mode:      rec.Mode,
		Pinned:    rec.Pinned,
		Label:     string(rec.Label),
		DeletedAt: rec.DeletedAt,
		MountName: string(rec.MountName),
	}, nil
}

func (i *Index) GetEntity(id domain.FileID) (out *domain.Entity, err error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if stateErr := i.currentStateLocked(); stateErr != nil {
		return nil, normalizeIndexErr(stateErr)
	}
	defer i.recoverIntoError(&err)
	v, closer, err := i.db.Get(entityKey(id))
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			return nil, domain.ErrNotFound
		}
		return nil, normalizeIndexErr(err)
	}
	e, err := decodeEntity(id, v)
	closeErr := closer.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, normalizeIndexErr(closeErr)
	}
	return &e, nil
}

func (i *Index) LookupChild(parentID domain.FileID, name string) (out *domain.DirEntry, err error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if stateErr := i.currentStateLocked(); stateErr != nil {
		return nil, normalizeIndexErr(stateErr)
	}
	defer i.recoverIntoError(&err)
	for _, isDir := range []bool{true, false} {
		v, closer, err := i.db.Get(childKey(parentID, isDir, name))
		if err != nil {
			if errors.Is(err, pebble.ErrNotFound) {
				continue
			}
			return nil, normalizeIndexErr(err)
		}
		entry, err := decodeChild(name, v)
		closeErr := closer.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, normalizeIndexErr(closeErr)
		}
		return &entry, nil
	}
	return nil, domain.ErrNotFound
}

func (i *Index) ListChildren(parentID domain.FileID, after domain.ChildCursor, limit int) (entries []domain.DirEntry, err error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if stateErr := i.currentStateLocked(); stateErr != nil {
		return nil, normalizeIndexErr(stateErr)
	}
	defer i.recoverIntoError(&err)
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	prefix := childPrefix(parentID)
	start := prefix
	if after.Name != "" {
		// Strict-greater seek past the cursor's exact key position.
		// Names cannot contain 0x00, so key+0x00 sorts between the
		// cursor and every later sibling. The cursor entry does not
		// need to exist anymore — no lookup, no special casing for
		// entries deleted between pages.
		startKey := childKey(parentID, after.IsDir, after.Name)
		start = append(append([]byte(nil), startKey...), 0x00)
	}

	iterOpts := &pebble.IterOptions{LowerBound: prefix}
	if upper := prefixUpperBound(prefix); upper != nil {
		iterOpts.UpperBound = upper
	}
	iter, err := i.db.NewIter(iterOpts)
	if err != nil {
		return nil, normalizeIndexErr(err)
	}
	defer iter.Close()

	entries = make([]domain.DirEntry, 0, limit)
	for ok := iter.SeekGE(start); ok && iter.Valid(); ok = iter.Next() {
		k := iter.Key()
		if !bytes.HasPrefix(k, prefix) {
			break
		}
		if len(k) <= len(prefix)+2 {
			continue
		}
		name := string(k[len(prefix)+2:])
		entry, err := decodeChild(name, iter.Value())
		if err != nil {
			continue
		}
		entries = append(entries, entry)
		if len(entries) >= limit {
			break
		}
	}
	return entries, nil
}

// LookupMultipartUploadRecord returns the durable upload record
// for uploadID, or domain.ErrNotFound. Records are JSON-encoded
// blobs — fixed schema, small enough that binary encoding wouldn't
// pay for itself.
func (i *Index) LookupMultipartUploadRecord(uploadID [16]byte) (out *domain.MultipartUploadRecord, err error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if stateErr := i.currentStateLocked(); stateErr != nil {
		return nil, normalizeIndexErr(stateErr)
	}
	defer i.recoverIntoError(&err)
	v, closer, err := i.db.Get(multipartUploadKey(uploadID))
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			return nil, domain.ErrNotFound
		}
		return nil, normalizeIndexErr(err)
	}
	defer closer.Close()
	var record domain.MultipartUploadRecord
	if err := json.Unmarshal(v, &record); err != nil {
		return nil, fmt.Errorf("decode multipart record: %w", err)
	}
	return &record, nil
}

func (i *Index) LookupActiveMultipartUpload(uploadID string) (out *domain.ActiveMultipartUpload, err error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if stateErr := i.currentStateLocked(); stateErr != nil {
		return nil, normalizeIndexErr(stateErr)
	}
	defer i.recoverIntoError(&err)
	v, closer, err := i.db.Get(activeMultipartUploadKey(uploadID))
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			return nil, domain.ErrNotFound
		}
		return nil, normalizeIndexErr(err)
	}
	defer closer.Close()
	var upload domain.ActiveMultipartUpload
	if err := json.Unmarshal(v, &upload); err != nil {
		return nil, fmt.Errorf("decode active multipart upload: %w", err)
	}
	return &upload, nil
}

func (i *Index) ListActiveMultipartUploads(bucket string) (uploads []domain.ActiveMultipartUpload, err error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if stateErr := i.currentStateLocked(); stateErr != nil {
		return nil, normalizeIndexErr(stateErr)
	}
	defer i.recoverIntoError(&err)

	prefix := activeMultipartUploadPrefix()
	iterOpts := &pebble.IterOptions{LowerBound: prefix}
	if upper := prefixUpperBound(prefix); upper != nil {
		iterOpts.UpperBound = upper
	}
	iter, err := i.db.NewIter(iterOpts)
	if err != nil {
		return nil, normalizeIndexErr(err)
	}
	defer iter.Close()

	for ok := iter.SeekGE(prefix); ok && iter.Valid(); ok = iter.Next() {
		if !bytes.HasPrefix(iter.Key(), prefix) {
			break
		}
		var upload domain.ActiveMultipartUpload
		if err := json.Unmarshal(iter.Value(), &upload); err != nil {
			continue
		}
		if bucket == "" || upload.Bucket == bucket {
			uploads = append(uploads, upload)
		}
	}
	sort.Slice(uploads, func(i, j int) bool {
		if uploads[i].Initiated == uploads[j].Initiated {
			return uploads[i].UploadID < uploads[j].UploadID
		}
		return uploads[i].Initiated < uploads[j].Initiated
	})
	return uploads, nil
}

func (i *Index) ListActiveMultipartParts(uploadID string) (parts []domain.ActiveMultipartPart, err error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if stateErr := i.currentStateLocked(); stateErr != nil {
		return nil, normalizeIndexErr(stateErr)
	}
	defer i.recoverIntoError(&err)

	prefix := activeMultipartPartPrefix(uploadID)
	iterOpts := &pebble.IterOptions{LowerBound: prefix}
	if upper := prefixUpperBound(prefix); upper != nil {
		iterOpts.UpperBound = upper
	}
	iter, err := i.db.NewIter(iterOpts)
	if err != nil {
		return nil, normalizeIndexErr(err)
	}
	defer iter.Close()

	for ok := iter.SeekGE(prefix); ok && iter.Valid(); ok = iter.Next() {
		if !bytes.HasPrefix(iter.Key(), prefix) {
			break
		}
		var part domain.ActiveMultipartPart
		if err := json.Unmarshal(iter.Value(), &part); err != nil {
			continue
		}
		parts = append(parts, part)
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].PartNumber < parts[j].PartNumber })
	return parts, nil
}

func (i *Index) LookupUploadSession(sessionID string) (out *domain.UploadSession, err error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if stateErr := i.currentStateLocked(); stateErr != nil {
		return nil, normalizeIndexErr(stateErr)
	}
	defer i.recoverIntoError(&err)
	v, closer, err := i.db.Get(uploadSessionKey(sessionID))
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			return nil, domain.ErrNotFound
		}
		return nil, normalizeIndexErr(err)
	}
	defer closer.Close()
	var session domain.UploadSession
	if err := json.Unmarshal(v, &session); err != nil {
		return nil, fmt.Errorf("decode upload session: %w", err)
	}
	return &session, nil
}

func (i *Index) ListUploadSessions(phase domain.UploadSessionPhase) (sessions []domain.UploadSession, err error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if stateErr := i.currentStateLocked(); stateErr != nil {
		return nil, normalizeIndexErr(stateErr)
	}
	defer i.recoverIntoError(&err)

	prefix := uploadSessionPrefix()
	iterOpts := &pebble.IterOptions{LowerBound: prefix}
	if upper := prefixUpperBound(prefix); upper != nil {
		iterOpts.UpperBound = upper
	}
	iter, err := i.db.NewIter(iterOpts)
	if err != nil {
		return nil, normalizeIndexErr(err)
	}
	defer iter.Close()

	for ok := iter.SeekGE(prefix); ok && iter.Valid(); ok = iter.Next() {
		if !bytes.HasPrefix(iter.Key(), prefix) {
			break
		}
		var session domain.UploadSession
		if err := json.Unmarshal(iter.Value(), &session); err != nil {
			continue
		}
		if phase == "" || session.Phase == phase {
			sessions = append(sessions, session)
		}
	}
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].CreatedAt == sessions[j].CreatedAt {
			return sessions[i].ID < sessions[j].ID
		}
		return sessions[i].CreatedAt < sessions[j].CreatedAt
	})
	return sessions, nil
}

func (i *Index) ListUploadSegments(sessionID string) (segments []domain.UploadSegment, err error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if stateErr := i.currentStateLocked(); stateErr != nil {
		return nil, normalizeIndexErr(stateErr)
	}
	defer i.recoverIntoError(&err)

	prefix := uploadSegmentPrefix(sessionID)
	iterOpts := &pebble.IterOptions{LowerBound: prefix}
	if upper := prefixUpperBound(prefix); upper != nil {
		iterOpts.UpperBound = upper
	}
	iter, err := i.db.NewIter(iterOpts)
	if err != nil {
		return nil, normalizeIndexErr(err)
	}
	defer iter.Close()

	for ok := iter.SeekGE(prefix); ok && iter.Valid(); ok = iter.Next() {
		if !bytes.HasPrefix(iter.Key(), prefix) {
			break
		}
		var segment domain.UploadSegment
		if err := json.Unmarshal(iter.Value(), &segment); err != nil {
			continue
		}
		segments = append(segments, segment)
	}
	sort.Slice(segments, func(i, j int) bool { return segments[i].Index < segments[j].Index })
	return segments, nil
}

func (i *Index) LookupUploadCommitRecord(sessionID string) (out *domain.UploadCommitRecord, err error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if stateErr := i.currentStateLocked(); stateErr != nil {
		return nil, normalizeIndexErr(stateErr)
	}
	defer i.recoverIntoError(&err)
	v, closer, err := i.db.Get(uploadCommitRecordKey(sessionID))
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			return nil, domain.ErrNotFound
		}
		return nil, normalizeIndexErr(err)
	}
	defer closer.Close()
	var record domain.UploadCommitRecord
	if err := json.Unmarshal(v, &record); err != nil {
		return nil, fmt.Errorf("decode upload commit record: %w", err)
	}
	return &record, nil
}

func (i *Index) LookupByFlatKey(mountName, relPath string) (id domain.FileID, err error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if stateErr := i.currentStateLocked(); stateErr != nil {
		return domain.FileID{}, normalizeIndexErr(stateErr)
	}
	defer i.recoverIntoError(&err)
	v, closer, err := i.db.Get(flatKeyForPath(mountName, relPath))
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			return domain.FileID{}, domain.ErrNotFound
		}
		return domain.FileID{}, normalizeIndexErr(err)
	}
	defer closer.Close()
	if len(v) != 16 {
		return domain.FileID{}, fmt.Errorf("flat-key value length=%d, want 16", len(v))
	}
	copy(id[:], v)
	return id, nil
}

// IterateFlatKeys walks flat-key entries under mountName whose
// relPath starts with prefix, in lexical order. after is a strict-
// greater bound on relPath (empty disables). limit caps fn invocations
// (zero = unlimited). fn returns (continue, error) — return false to
// stop iteration without an error.
func (i *Index) IterateFlatKeys(mountName, prefix, after string, limit int, fn func(relPath string, id domain.FileID) (bool, error)) (err error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if stateErr := i.currentStateLocked(); stateErr != nil {
		return normalizeIndexErr(stateErr)
	}
	defer i.recoverIntoError(&err)

	mountPrefix := flatKeyMountPrefix(mountName)
	scanPrefix := append(append([]byte(nil), mountPrefix...), []byte(prefix)...)
	iterOpts := &pebble.IterOptions{LowerBound: scanPrefix}
	if upper := prefixUpperBound(mountPrefix); upper != nil {
		iterOpts.UpperBound = upper
	}
	iter, err := i.db.NewIter(iterOpts)
	if err != nil {
		return normalizeIndexErr(err)
	}
	defer iter.Close()

	start := scanPrefix
	if after != "" {
		afterKey := flatKeyForPath(mountName, after)
		// SeekGT for strict-greater semantics.
		start = append(append([]byte(nil), afterKey...), 0x00)
	}

	count := 0
	for ok := iter.SeekGE(start); ok && iter.Valid(); ok = iter.Next() {
		k := iter.Key()
		if !bytes.HasPrefix(k, scanPrefix) {
			break
		}
		_, relPath, parsed := flatKeySplit(k)
		if !parsed {
			continue
		}
		v := iter.Value()
		if len(v) != 16 {
			continue
		}
		var id domain.FileID
		copy(id[:], v)
		cont, err := fn(relPath, id)
		if err != nil {
			return err
		}
		if !cont {
			break
		}
		count++
		if limit > 0 && count >= limit {
			break
		}
	}
	return nil
}

func (i *Index) ListEntities() ([]domain.Entity, error) {
	out := make([]domain.Entity, 0, 1024)
	if err := i.ForEachEntity(func(e domain.Entity) error {
		out = append(out, e)
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, nil
}

func (i *Index) ForEachEntity(fn func(domain.Entity) error) (err error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if stateErr := i.currentStateLocked(); stateErr != nil {
		return normalizeIndexErr(stateErr)
	}
	defer i.recoverIntoError(&err)
	prefix := []byte{familyEntity}
	iterOpts := &pebble.IterOptions{LowerBound: prefix}
	if upper := prefixUpperBound(prefix); upper != nil {
		iterOpts.UpperBound = upper
	}
	iter, err := i.db.NewIter(iterOpts)
	if err != nil {
		return normalizeIndexErr(err)
	}
	defer iter.Close()

	for ok := iter.SeekGE(prefix); ok && iter.Valid(); ok = iter.Next() {
		k := iter.Key()
		if len(k) == 0 || k[0] != familyEntity {
			break
		}
		if len(k) != 17 {
			continue
		}
		var id domain.FileID
		copy(id[:], k[1:17])
		e, err := decodeEntity(id, iter.Value())
		if err != nil {
			continue
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return nil
}

// GetVersion returns the metadata for a single version, or domain.ErrNotFound.
func (i *Index) GetVersion(fileID domain.FileID, versionID domain.VersionID) (out *domain.VersionMeta, err error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if stateErr := i.currentStateLocked(); stateErr != nil {
		return nil, normalizeIndexErr(stateErr)
	}
	defer i.recoverIntoError(&err)
	v, closer, err := i.db.Get(versionKey(fileID, versionID))
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			return nil, domain.ErrNotFound
		}
		return nil, normalizeIndexErr(err)
	}
	defer closer.Close()
	meta, err := decodeVersion(v)
	if err != nil {
		return nil, err
	}
	return &meta, nil
}

// ListVersions returns versions of fileID, ordered by Timestamp ascending
// (oldest first). The `after` cursor is the VersionID returned at the end
// of the previous page; pass the zero VersionID to start from the beginning.
// limit ≤ 0 defaults to 100, limit > 1000 caps to 1000.
func (i *Index) ListVersions(fileID domain.FileID, after domain.VersionID, limit int) (out []domain.VersionMeta, err error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if stateErr := i.currentStateLocked(); stateErr != nil {
		return nil, normalizeIndexErr(stateErr)
	}
	defer i.recoverIntoError(&err)
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	prefix := versionPrefix(fileID)
	start := prefix
	if !after.IsZero() {
		// Strict-greater-than: skip past the cursor's exact key. SeekGE
		// to the cursor and skip it on the first iteration would also
		// work but adds a branch in the hot loop.
		startKey := versionKey(fileID, after)
		start = append(append([]byte(nil), startKey...), 0x00)
	}
	iterOpts := &pebble.IterOptions{LowerBound: prefix}
	if upper := prefixUpperBound(prefix); upper != nil {
		iterOpts.UpperBound = upper
	}
	iter, err := i.db.NewIter(iterOpts)
	if err != nil {
		return nil, normalizeIndexErr(err)
	}
	defer iter.Close()

	out = make([]domain.VersionMeta, 0, limit)
	for ok := iter.SeekGE(start); ok && iter.Valid() && len(out) < limit; ok = iter.Next() {
		k := iter.Key()
		if !bytes.HasPrefix(k, prefix) || len(k) != 1+16+16 {
			break
		}
		meta, err := decodeVersion(iter.Value())
		if err != nil {
			continue
		}
		out = append(out, meta)
	}
	return out, nil
}

// LatestVersionTimestamp returns the Timestamp of the newest version of
// fileID, or 0 if no versions exist. Used by the cooldown check on the
// auto-capture path. The Pebble keys are sorted by VersionID ascending,
// which (because UUIDv7's leading bytes are BigEndian unix-ms) is the
// same as Timestamp ascending — the last key in the prefix is the newest.
func (i *Index) LatestVersionTimestamp(fileID domain.FileID) (ts int64, err error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if stateErr := i.currentStateLocked(); stateErr != nil {
		return 0, normalizeIndexErr(stateErr)
	}
	defer i.recoverIntoError(&err)
	prefix := versionPrefix(fileID)
	iterOpts := &pebble.IterOptions{LowerBound: prefix}
	if upper := prefixUpperBound(prefix); upper != nil {
		iterOpts.UpperBound = upper
	}
	iter, ierr := i.db.NewIter(iterOpts)
	if ierr != nil {
		return 0, normalizeIndexErr(ierr)
	}
	defer iter.Close()
	if !iter.SeekLT(iterOpts.UpperBound) {
		return 0, nil
	}
	k := iter.Key()
	if !bytes.HasPrefix(k, prefix) || len(k) != 1+16+16 {
		return 0, nil
	}
	meta, derr := decodeVersion(iter.Value())
	if derr != nil {
		return 0, derr
	}
	return meta.Timestamp, nil
}

// ForEachFileVersions iterates the versions keyspace and groups
// adjacent rows by FileID. fn is invoked once per file with all its
// versions in ascending Timestamp order. Memory is bounded to one
// file's versions at a time, which makes the pass safe even on indexes
// with millions of versions across many files.
func (i *Index) ForEachFileVersions(fn func(fileID domain.FileID, versions []domain.VersionMeta) error) (err error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if stateErr := i.currentStateLocked(); stateErr != nil {
		return normalizeIndexErr(stateErr)
	}
	defer i.recoverIntoError(&err)

	prefix := []byte{familyVersion}
	iterOpts := &pebble.IterOptions{LowerBound: prefix}
	if upper := prefixUpperBound(prefix); upper != nil {
		iterOpts.UpperBound = upper
	}
	iter, err := i.db.NewIter(iterOpts)
	if err != nil {
		return normalizeIndexErr(err)
	}
	defer iter.Close()

	var current domain.FileID
	var batch []domain.VersionMeta
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := fn(current, batch); err != nil {
			return err
		}
		batch = batch[:0]
		return nil
	}
	for ok := iter.SeekGE(prefix); ok && iter.Valid(); ok = iter.Next() {
		k := iter.Key()
		if !bytes.HasPrefix(k, prefix) || len(k) != 1+16+16 {
			break
		}
		var fid domain.FileID
		copy(fid[:], k[1:17])
		if len(batch) == 0 {
			current = fid
		} else if fid != current {
			if err := flush(); err != nil {
				return err
			}
			current = fid
		}
		meta, derr := decodeVersion(iter.Value())
		if derr != nil {
			continue
		}
		batch = append(batch, meta)
	}
	if err := flush(); err != nil {
		return err
	}
	return nil
}

// MarkVersionsDeleted sets DeletedAt = deletedAt on every version of
// fileID whose DeletedAt is currently zero. Idempotent: re-running with a
// later timestamp does not bump already-marked entries (the original
// transition time wins, so the grace period is measured from when the
// file actually went away, not from a re-mark). Returns the number of
// rows updated.
//
// Pages through ListVersions in 1000-entry chunks. The pagination
// cursor is the last VersionID of the prior page; ListVersions's
// strict-greater-than semantics on the cursor advance correctly.
func (i *Index) MarkVersionsDeleted(fileID domain.FileID, deletedAt int64) (n int, err error) {
	if deletedAt <= 0 {
		return 0, domain.ErrInvalidArgument
	}
	const pageSize = 1000
	pending := make([]domain.VersionMeta, 0)
	cursor := domain.VersionID{}
	for {
		page, err := i.ListVersions(fileID, cursor, pageSize)
		if err != nil {
			return 0, err
		}
		if len(page) == 0 {
			break
		}
		for _, v := range page {
			if v.DeletedAt == 0 {
				v.DeletedAt = deletedAt
				pending = append(pending, v)
			}
		}
		if len(page) < pageSize {
			break
		}
		cursor = page[len(page)-1].VersionID
	}
	if len(pending) == 0 {
		return 0, nil
	}
	if err := i.Batch(func(b domain.Batch) error {
		for _, v := range pending {
			b.PutVersion(v)
		}
		return nil
	}); err != nil {
		return 0, err
	}
	return len(pending), nil
}

type batch struct {
	b   pebbleBatchReadWriter
	err error
}

// pebbleBatchReadWriter is the subset of *pebble.Batch we use during the
// batch lifecycle. Reads are needed so PutEntity can detect renames (old
// child entry to delete) and inode changes (mapping to update). Get reads
// from the batch's own pending writes plus the underlying DB, which is the
// behaviour we want for self-consistency within a single batch.
type pebbleBatchReadWriter interface {
	Get(key []byte) ([]byte, io.Closer, error)
	Set(key, value []byte, opts *pebble.WriteOptions) error
	Delete(key []byte, opts *pebble.WriteOptions) error
}

func (b *batch) setErr(err error) {
	if err == nil || b.err != nil {
		return
	}
	b.err = err
}

func (b *batch) PutEntity(entity domain.Entity) {
	if b.err != nil {
		return
	}
	// Read the previous entity so we can detect a same-id rename
	// (Name or ParentID changed for the same entity ID) and drop the
	// stale child entry under the old parent. This is the common case
	// fast path; ReconcileDirectory in the consumer is the safety net
	// for the cases this misses (e.g. when find-new doesn't emit at all
	// for an in-subvol directory rename).
	//
	// Skipped for hard-link siblings (nlink > 1) because they share an ID
	// across multiple (parent, name) pairs and "the previous Put was
	// stale" is not a valid inference. snapshot/cp-a duplicates can't
	// trigger this path because resolveOrReissueID upstream re-issues a
	// fresh UUID for them — by the time PutEntity sees them they have
	// their own ID with no prior entity record.
	prev, err := b.loadEntityForBatch(entity.ID)
	if err != nil {
		b.setErr(err)
		return
	}
	if prev != nil {
		// nlinkSafe addresses hard-link siblings (Nlink>1) for FILES:
		// they share one ID across multiple (parent, name) pairs, so
		// a Put under a different (parent, name) isn't a rename of
		// the original — it's adoption of another link. Directories
		// always have Nlink>=2 on Linux (the "." entry counts), so
		// the safety check would suppress every legitimate dir
		// rename. Apply it only to non-directories.
		nlinkSafe := entity.IsDir || (prev.Nlink <= 1 && entity.Nlink <= 1)
		if nlinkSafe && (prev.ParentID != entity.ParentID || prev.Name != entity.Name) {
			if err := b.b.Delete(childKey(prev.ParentID, true, prev.Name), nil); err != nil {
				b.setErr(err)
				return
			}
			if err := b.b.Delete(childKey(prev.ParentID, false, prev.Name), nil); err != nil {
				b.setErr(err)
				return
			}
			if entity.IsDir && prev.IsDir {
				// Directory rename: descendant flat-keys live under
				// the OLD path prefix; rewrite them all to the new
				// prefix so detector / rescan / external rename
				// flows behave the same as the API-driven Transfer
				// path. Walks read pre-batch state for prev's
				// ancestors (which haven't been put in this batch)
				// and post-batch state for current's ancestors —
				// both correct because the directory itself is the
				// only thing changing in this batch.
				b.rekeyDirOnRename(*prev, entity)
			} else {
				// File rename: drop the stale leaf flat-key entry.
				// The new flat-key (under new parent/name) is
				// inserted by upsertFlatKeyForEntity below.
				b.maintainFlatKeyOnRename(prev, entity)
			}
		}
	}
	payload, err := encodeEntity(entity)
	if err != nil {
		b.setErr(err)
		return
	}
	if err := b.b.Set(entityKey(entity.ID), payload, nil); err != nil {
		b.setErr(err)
		return
	}
	// Auto-maintain the flat-key entry for files. Directories are not
	// S3 objects and don't get flat-key entries; bulk subtree
	// operations (delete, dir rename) are handled by explicit
	// DelFlatKeysUnder / ReKeyFlatPrefix calls from the domain layer.
	if !entity.IsDir {
		b.upsertFlatKeyForEntity(entity)
	}
}

// upsertFlatKeyForEntity walks the parent chain via batch reads to
// derive the entity's mount + relPath, then inserts the flat-key.
// Silently no-ops on walk failure (orphaned entity, missing parent) —
// rescan will repair on next run.
//
// Hard-link siblings (Nlink > 1) are skipped: a single FileID maps to
// multiple paths, but our flat-key schema has one (mount, relPath) →
// id mapping per file. Maintaining only the most-recently-Put path
// would lie about the others. Skipping all of them means S3
// LookupByFlatKey returns NotFound for hard-linked files — a
// documented limitation that's safer than aliasing.
func (b *batch) upsertFlatKeyForEntity(entity domain.Entity) {
	if entity.Nlink > 1 {
		return
	}
	mount, relPath, ok := b.derivePath(entity)
	if !ok {
		return
	}
	val := make([]byte, 16)
	copy(val, entity.ID[:])
	if err := b.b.Set(flatKeyForPath(mount, relPath), val, nil); err != nil {
		b.setErr(err)
	}
}

// maintainFlatKeyOnRename deletes the old-path flat-key entry when an
// entity's parent or name changed. The new flat-key is inserted
// separately by upsertFlatKeyForEntity.
//
// Hard-link transitions: a file with Nlink>1 in either prev or
// current state has no representable flat-key (see
// upsertFlatKeyForEntity), so there's nothing to clean up.
func (b *batch) maintainFlatKeyOnRename(prev *domain.Entity, current domain.Entity) {
	if prev == nil || prev.IsDir {
		// prev was a directory → no flat-key to delete. (If current is
		// a file with the same ID as a previous directory, that's a
		// type-change which the rest of PutEntity handles via child
		// entry deletion; we don't try to gracefully convert here.)
		return
	}
	if prev.Nlink > 1 || current.Nlink > 1 {
		return
	}
	mount, relPath, ok := b.derivePath(*prev)
	if !ok {
		return
	}
	if err := b.b.Delete(flatKeyForPath(mount, relPath), nil); err != nil {
		b.setErr(err)
	}
}

// rekeyDirOnRename derives the OLD and NEW directory paths for a
// same-id directory rename and rewrites every descendant flat-key
// from old prefix to new prefix. Called from inside PutEntity when
// rename detection fires for a directory entity.
//
// Walks for prev use the directory's PRIOR (parent, name) — the
// ancestors above the renamed directory haven't been touched in this
// batch (they don't appear here), so reads from the batch see their
// stable committed state. Walks for current use the NEW (parent,
// name) — same reasoning. Both walks therefore produce the correct
// before/after paths even when called from inside the batch.
func (b *batch) rekeyDirOnRename(prev, current domain.Entity) {
	oldMount, oldRel, ok1 := b.deriveDirPath(prev)
	newMount, newRel, ok2 := b.deriveDirPath(current)
	if !ok1 || !ok2 {
		return
	}
	// ReKeyFlatPrefix's empty-prefix semantic means "entire mount" —
	// not appropriate for a per-directory rename. If a directory IS a
	// mount root (oldRel == "" or newRel == ""), rekey is a no-op or
	// would over-rewrite; mounts aren't renamed via this path.
	if oldRel == "" || newRel == "" {
		return
	}
	b.ReKeyFlatPrefix(oldMount, oldRel, newMount, newRel)
}

// deriveDirPath walks a directory's parent chain to build (mount,
// relPath). Differs from derivePath in that derivePath rejects
// directories outright (since files are the only flat-key holders);
// this helper is for rekey planning where we DO need a directory's
// path expression.
func (b *batch) deriveDirPath(entity domain.Entity) (mount, relPath string, ok bool) {
	if !entity.IsDir || entity.ParentID.IsZero() {
		return "", "", false
	}
	const maxDepth = 256
	segments := []string{entity.Name}
	cur := entity.ParentID
	for depth := 0; depth < maxDepth; depth++ {
		parent, err := b.loadEntityForBatch(cur)
		if err != nil || parent == nil {
			return "", "", false
		}
		if parent.ParentID.IsZero() {
			for i, j := 0, len(segments)-1; i < j; i, j = i+1, j-1 {
				segments[i], segments[j] = segments[j], segments[i]
			}
			return parent.Name, joinRel(segments), true
		}
		segments = append(segments, parent.Name)
		cur = parent.ParentID
	}
	return "", "", false
}

// derivePath walks the entity's parent chain via batch reads (which
// see committed state plus pending in-batch writes) and returns the
// (mount, relPath) form. Returns ok=false for directories, mount roots,
// orphans, and pathological cycles.
func (b *batch) derivePath(entity domain.Entity) (mount, relPath string, ok bool) {
	if entity.IsDir || entity.ParentID.IsZero() {
		return "", "", false
	}
	// Bounded walk (depth cap) to defend against pathological cycles.
	const maxDepth = 256
	segments := []string{entity.Name}
	cur := entity.ParentID
	for depth := 0; depth < maxDepth; depth++ {
		parent, err := b.loadEntityForBatch(cur)
		if err != nil || parent == nil {
			return "", "", false
		}
		if parent.ParentID.IsZero() {
			// parent is the mount root; its Name is the mount name.
			// segments accumulate leaf-first; reverse for top-down.
			for i, j := 0, len(segments)-1; i < j; i, j = i+1, j-1 {
				segments[i], segments[j] = segments[j], segments[i]
			}
			return parent.Name, joinRel(segments), true
		}
		segments = append(segments, parent.Name)
		cur = parent.ParentID
	}
	return "", "", false
}

// joinRel joins path segments with "/" — kept private so callers can't
// confuse it with virtual-path conventions (no leading slash here).
func joinRel(segments []string) string {
	if len(segments) == 0 {
		return ""
	}
	if len(segments) == 1 {
		return segments[0]
	}
	total := len(segments) - 1
	for _, s := range segments {
		total += len(s)
	}
	out := make([]byte, 0, total)
	for i, s := range segments {
		if i > 0 {
			out = append(out, '/')
		}
		out = append(out, s...)
	}
	return string(out)
}

// loadEntityForBatch returns the entity stored under id, reading from the
// batch's pending writes plus the underlying DB. Returns (nil, nil) when no
// entity is stored.
func (b *batch) loadEntityForBatch(id domain.FileID) (*domain.Entity, error) {
	v, closer, err := b.b.Get(entityKey(id))
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	defer closer.Close()
	e, err := decodeEntity(id, v)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (b *batch) PutChild(parentID domain.FileID, name string, entry domain.DirEntry) {
	if b.err != nil {
		return
	}
	payload, err := encodeChild(entry)
	if err != nil {
		b.setErr(err)
		return
	}
	b.setErr(b.b.Set(childKey(parentID, entry.IsDir, name), payload, nil))
}

func (b *batch) DelChild(parentID domain.FileID, name string) {
	if b.err != nil {
		return
	}
	if err := b.b.Delete(childKey(parentID, true, name), nil); err != nil {
		b.setErr(err)
		return
	}
	b.setErr(b.b.Delete(childKey(parentID, false, name), nil))
}

func (b *batch) DelEntity(id domain.FileID) {
	if b.err != nil {
		return
	}
	// Read prev so we can drop its flat-key entry (files only). If
	// prev is a directory, has Nlink>1, or the walk fails, no
	// flat-key was written for it — skip silently. Subtree deletes
	// use DelFlatKeysUnder from the domain layer to clear descendants
	// in bulk.
	prev, err := b.loadEntityForBatch(id)
	if err != nil {
		b.setErr(err)
		return
	}
	if prev != nil && !prev.IsDir && prev.Nlink <= 1 {
		if mount, relPath, ok := b.derivePath(*prev); ok {
			if err := b.b.Delete(flatKeyForPath(mount, relPath), nil); err != nil {
				b.setErr(err)
				return
			}
		}
	}
	b.setErr(b.b.Delete(entityKey(id), nil))
}

func (b *batch) PutMultipartUploadRecord(uploadID [16]byte, record domain.MultipartUploadRecord) {
	if b.err != nil {
		return
	}
	payload, err := json.Marshal(record)
	if err != nil {
		b.setErr(err)
		return
	}
	b.setErr(b.b.Set(multipartUploadKey(uploadID), payload, nil))
}

func (b *batch) DelMultipartUploadRecord(uploadID [16]byte) {
	if b.err != nil {
		return
	}
	b.setErr(b.b.Delete(multipartUploadKey(uploadID), nil))
}

func (b *batch) PutActiveMultipartUpload(upload domain.ActiveMultipartUpload) {
	if b.err != nil {
		return
	}
	if upload.UploadID == "" {
		b.setErr(domain.ErrInvalidArgument)
		return
	}
	payload, err := json.Marshal(upload)
	if err != nil {
		b.setErr(err)
		return
	}
	b.setErr(b.b.Set(activeMultipartUploadKey(upload.UploadID), payload, nil))
}

func (b *batch) DelActiveMultipartUpload(uploadID string) {
	if b.err != nil {
		return
	}
	b.setErr(b.b.Delete(activeMultipartUploadKey(uploadID), nil))
}

func (b *batch) PutActiveMultipartPart(part domain.ActiveMultipartPart) {
	if b.err != nil {
		return
	}
	if part.UploadID == "" || part.PartNumber < 1 || part.PartNumber > 10000 {
		b.setErr(domain.ErrInvalidArgument)
		return
	}
	payload, err := json.Marshal(part)
	if err != nil {
		b.setErr(err)
		return
	}
	b.setErr(b.b.Set(activeMultipartPartKey(part.UploadID, part.PartNumber), payload, nil))
}

func (b *batch) DelActiveMultipartPart(uploadID string, partNumber int) {
	if b.err != nil {
		return
	}
	b.setErr(b.b.Delete(activeMultipartPartKey(uploadID, partNumber), nil))
}

func (b *batch) DelActiveMultipartParts(uploadID string) {
	if b.err != nil {
		return
	}
	iterable, ok := b.b.(pebbleBatchIterable)
	if !ok {
		b.setErr(fmt.Errorf("batch does not support iteration"))
		return
	}
	prefix := activeMultipartPartPrefix(uploadID)
	iterOpts := &pebble.IterOptions{LowerBound: prefix}
	if upper := prefixUpperBound(prefix); upper != nil {
		iterOpts.UpperBound = upper
	}
	iter, err := iterable.NewIter(iterOpts)
	if err != nil {
		b.setErr(err)
		return
	}
	defer iter.Close()
	for ok := iter.SeekGE(prefix); ok && iter.Valid(); ok = iter.Next() {
		if !bytes.HasPrefix(iter.Key(), prefix) {
			break
		}
		key := append([]byte(nil), iter.Key()...)
		if err := b.b.Delete(key, nil); err != nil {
			b.setErr(err)
			return
		}
	}
}

func (b *batch) PutUploadSession(session domain.UploadSession) {
	if b.err != nil {
		return
	}
	if session.ID == "" {
		b.setErr(domain.ErrInvalidArgument)
		return
	}
	payload, err := json.Marshal(session)
	if err != nil {
		b.setErr(err)
		return
	}
	b.setErr(b.b.Set(uploadSessionKey(session.ID), payload, nil))
}

func (b *batch) DelUploadSession(sessionID string) {
	if b.err != nil {
		return
	}
	b.setErr(b.b.Delete(uploadSessionKey(sessionID), nil))
}

func (b *batch) PutUploadSegment(segment domain.UploadSegment) {
	if b.err != nil {
		return
	}
	if segment.SessionID == "" || segment.Index < 0 {
		b.setErr(domain.ErrInvalidArgument)
		return
	}
	payload, err := json.Marshal(segment)
	if err != nil {
		b.setErr(err)
		return
	}
	b.setErr(b.b.Set(uploadSegmentKey(segment.SessionID, segment.Index), payload, nil))
}

func (b *batch) DelUploadSegment(sessionID string, index int) {
	if b.err != nil {
		return
	}
	b.setErr(b.b.Delete(uploadSegmentKey(sessionID, index), nil))
}

func (b *batch) DelUploadSegments(sessionID string) {
	if b.err != nil {
		return
	}
	iterable, ok := b.b.(pebbleBatchIterable)
	if !ok {
		b.setErr(fmt.Errorf("batch does not support iteration"))
		return
	}
	prefix := uploadSegmentPrefix(sessionID)
	iterOpts := &pebble.IterOptions{LowerBound: prefix}
	if upper := prefixUpperBound(prefix); upper != nil {
		iterOpts.UpperBound = upper
	}
	iter, err := iterable.NewIter(iterOpts)
	if err != nil {
		b.setErr(err)
		return
	}
	defer iter.Close()
	for ok := iter.SeekGE(prefix); ok && iter.Valid(); ok = iter.Next() {
		if !bytes.HasPrefix(iter.Key(), prefix) {
			break
		}
		key := append([]byte(nil), iter.Key()...)
		if err := b.b.Delete(key, nil); err != nil {
			b.setErr(err)
			return
		}
	}
}

func (b *batch) PutUploadCommitRecord(record domain.UploadCommitRecord) {
	if b.err != nil {
		return
	}
	if record.SessionID == "" {
		b.setErr(domain.ErrInvalidArgument)
		return
	}
	payload, err := json.Marshal(record)
	if err != nil {
		b.setErr(err)
		return
	}
	b.setErr(b.b.Set(uploadCommitRecordKey(record.SessionID), payload, nil))
}

func (b *batch) DelUploadCommitRecord(sessionID string) {
	if b.err != nil {
		return
	}
	b.setErr(b.b.Delete(uploadCommitRecordKey(sessionID), nil))
}

func (b *batch) PutFlatKey(mountName, relPath string, id domain.FileID) {
	if b.err != nil {
		return
	}
	if mountName == "" {
		b.setErr(fmt.Errorf("PutFlatKey: empty mount name"))
		return
	}
	val := make([]byte, 16)
	copy(val, id[:])
	b.setErr(b.b.Set(flatKeyForPath(mountName, relPath), val, nil))
}

func (b *batch) DelFlatKey(mountName, relPath string) {
	if b.err != nil {
		return
	}
	if mountName == "" {
		// Empty mount → no-op rather than error; helps keep callers
		// simple (e.g. mount-root entities have no flat-key path to
		// remove).
		return
	}
	b.setErr(b.b.Delete(flatKeyForPath(mountName, relPath), nil))
}

// flatKeyBatchChunkSize bounds how many keys a single iteration pass
// of DelFlatKeysUnder / ReKeyFlatPrefix collects before flushing.
// Pebble batches grow proportionally with the in-memory key list, so
// chunking caps memory for catastrophic-large directory operations
// (a million-descendant rename shouldn't OOM us). Pebble's batch is
// committed atomically by Index.Batch — chunking here only bounds
// the SLICE size, not the eventual commit's atomicity guarantee.
const flatKeyBatchChunkSize = 4096

// DelFlatKeysUnder deletes every flat-key entry whose relPath equals
// relPathPrefix or starts with relPathPrefix + "/". Empty relPathPrefix
// wipes the entire mount.
//
// We can't use Pebble's range-delete here because the batch's reads
// must see the deletions for self-consistency within the same
// operation; range-delete is a tombstone applied at compaction time
// and is invisible to in-batch reads. So we iterate and Delete each,
// chunking the in-memory collection to bound peak memory.
func (b *batch) DelFlatKeysUnder(mountName, relPathPrefix string) {
	if b.err != nil {
		return
	}
	if mountName == "" {
		b.setErr(fmt.Errorf("DelFlatKeysUnder: empty mount name"))
		return
	}
	mountPrefix := flatKeyMountPrefix(mountName)
	scanPrefix := append(append([]byte(nil), mountPrefix...), []byte(relPathPrefix)...)
	upper := prefixUpperBound(mountPrefix)
	iterOpts := &pebble.IterOptions{LowerBound: scanPrefix}
	if upper != nil {
		iterOpts.UpperBound = upper
	}
	iter, err := b.b.(pebbleBatchIterable).NewIter(iterOpts)
	if err != nil {
		b.setErr(err)
		return
	}
	defer iter.Close()

	// Walk + delete in chunks. After each flush we resume the
	// iterator from the next key beyond the last deleted, so we
	// never hold more than chunkSize key copies in memory.
	chunk := make([][]byte, 0, flatKeyBatchChunkSize)
	flush := func() bool {
		for _, k := range chunk {
			if err := b.b.Delete(k, nil); err != nil {
				b.setErr(err)
				return false
			}
		}
		chunk = chunk[:0]
		return true
	}
	for ok := iter.SeekGE(scanPrefix); ok && iter.Valid(); ok = iter.Next() {
		k := iter.Key()
		if !bytes.HasPrefix(k, scanPrefix) {
			break
		}
		// Boundary check: a relPath of "foo" must not match "foo-bar"
		// — only "foo" itself or "foo/...".
		if relPathPrefix != "" && len(k) > len(scanPrefix) {
			next := k[len(scanPrefix)]
			if next != '/' {
				continue
			}
		}
		chunk = append(chunk, append([]byte(nil), k...))
		if len(chunk) >= flatKeyBatchChunkSize {
			if !flush() {
				return
			}
		}
	}
	flush()
}

// ReKeyFlatPrefix moves every flat-key entry from (oldMount,
// oldPrefix or descendant) to (newMount, newPrefix or descendant) by
// stripping oldPrefix and prepending newPrefix from each relPath,
// preserving file IDs. Used by directory rename/move.
func (b *batch) ReKeyFlatPrefix(oldMount, oldPrefix, newMount, newPrefix string) {
	if b.err != nil {
		return
	}
	if oldMount == "" || newMount == "" {
		b.setErr(fmt.Errorf("ReKeyFlatPrefix: empty mount name"))
		return
	}
	oldMountPrefix := flatKeyMountPrefix(oldMount)
	scanPrefix := append(append([]byte(nil), oldMountPrefix...), []byte(oldPrefix)...)
	upper := prefixUpperBound(oldMountPrefix)
	iterOpts := &pebble.IterOptions{LowerBound: scanPrefix}
	if upper != nil {
		iterOpts.UpperBound = upper
	}
	iter, err := b.b.(pebbleBatchIterable).NewIter(iterOpts)
	if err != nil {
		b.setErr(err)
		return
	}
	defer iter.Close()

	type rekey struct {
		oldKey []byte
		newKey []byte
		id     domain.FileID
	}
	chunk := make([]rekey, 0, flatKeyBatchChunkSize)
	flush := func() bool {
		for _, m := range chunk {
			if err := b.b.Delete(m.oldKey, nil); err != nil {
				b.setErr(err)
				return false
			}
			val := make([]byte, 16)
			copy(val, m.id[:])
			if err := b.b.Set(m.newKey, val, nil); err != nil {
				b.setErr(err)
				return false
			}
		}
		chunk = chunk[:0]
		return true
	}
	for ok := iter.SeekGE(scanPrefix); ok && iter.Valid(); ok = iter.Next() {
		k := iter.Key()
		if !bytes.HasPrefix(k, scanPrefix) {
			break
		}
		// Same boundary-check as DelFlatKeysUnder: when oldPrefix is
		// non-empty, the next byte must be "/" (descendant) — "foo"
		// must not match "foobar".
		if oldPrefix != "" && len(k) > len(scanPrefix) {
			next := k[len(scanPrefix)]
			if next != '/' {
				continue
			}
		}
		_, oldRel, parsed := flatKeySplit(k)
		if !parsed {
			continue
		}
		// Strip oldPrefix and prepend newPrefix to derive new relPath.
		// For oldRel="a/b" and oldPrefix="a", suffix is "/b" → new
		// relPath = newPrefix + "/b". For an exact match (oldRel ==
		// oldPrefix), suffix is "" → new relPath = newPrefix.
		var suffix string
		if len(oldRel) > len(oldPrefix) {
			suffix = oldRel[len(oldPrefix):]
		}
		newRel := newPrefix + suffix
		v := iter.Value()
		if len(v) != 16 {
			continue
		}
		var id domain.FileID
		copy(id[:], v)
		chunk = append(chunk, rekey{
			oldKey: append([]byte(nil), k...),
			newKey: flatKeyForPath(newMount, newRel),
			id:     id,
		})
		if len(chunk) >= flatKeyBatchChunkSize {
			if !flush() {
				return
			}
		}
	}
	flush()
}

// pebbleBatchIterable extends pebbleBatchReadWriter with iterator
// support so flat-key range operations can scan within the batch's
// read-view (committed state + pending writes).
type pebbleBatchIterable interface {
	NewIter(opts *pebble.IterOptions) (*pebble.Iterator, error)
}

func (b *batch) PutVersion(meta domain.VersionMeta) {
	if b.err != nil {
		return
	}
	if meta.FileID.IsZero() || meta.VersionID.IsZero() {
		b.setErr(domain.ErrInvalidArgument)
		return
	}
	payload, err := encodeVersion(meta)
	if err != nil {
		b.setErr(err)
		return
	}
	b.setErr(b.b.Set(versionKey(meta.FileID, meta.VersionID), payload, nil))
}

func (b *batch) DelVersion(fileID domain.FileID, versionID domain.VersionID) {
	if b.err != nil {
		return
	}
	b.setErr(b.b.Delete(versionKey(fileID, versionID), nil))
}

func (i *Index) Batch(fn func(domain.Batch) error) (err error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if stateErr := i.currentStateLocked(); stateErr != nil {
		return normalizeIndexErr(stateErr)
	}
	defer i.recoverIntoError(&err)
	// Indexed batch is required because PutEntity reads the previous
	// entity to detect a same-id rename (so it can drop the stale child
	// entry under the old parent in the same atomic write). NewBatch
	// does not support Get inside the batch.
	b := i.db.NewIndexedBatch()
	defer b.Close()
	wrap := &batch{b: b}
	if err := fn(wrap); err != nil {
		return err
	}
	if wrap.err != nil {
		return wrap.err
	}
	return normalizeIndexErr(b.Commit(pebble.Sync))
}

func (i *Index) recoverIntoError(target *error) {
	if rec := recover(); rec != nil {
		err := fmt.Errorf("%w: %v", ErrIndexUnavailable, rec)
		i.markFatal(err)
		*target = err
	}
}

func (i *Index) Close() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return nil
	}
	i.closed = true
	return normalizeIndexErr(i.db.Close())
}
