package s3

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/k2b-dev/filegate/v3/domain"
)

// MultipartCleanupResult is the per-pass summary the cleanup loop
// returns. The CLI logs this when any field is non-zero.
type MultipartCleanupResult struct {
	StageDirsScanned int
	DoneRetired      int // phaseDone uploads removed (with their durable record)
	AbortedRetired   int // phaseAborted uploads removed
	StuckAborted     int // phaseInProgress / phaseCommitting older than maxAge → forcibly aborted
	Errors           int
}

// MultipartCleanupConfig controls the retention policy.
//
//   - DoneRetention: how long a successfully-Completed active upload
//     row stays around for cleanup bookkeeping. Complete retries are
//     served by the durable 0x07 record. After this, the active row,
//     staging dir, and durable record are GC'd. Default: 24 hours — generous enough
//     that any sane client retry has happened, short enough that
//     a misbehaving client can't pin storage forever.
//
//   - AbortedRetention: how long an explicitly-aborted upload's
//     active row sticks around. AbortMultipartUpload currently removes
//     active rows immediately; this retention remains for legacy manifest
//     cleanup and future explicit aborted-state rows.
//
//   - StuckUploadMaxAge: how long an in_progress / committing
//     active row can stay open before the cleanup loop forcibly
//     aborts it. Catches clients that started a multipart upload
//     and never came back (network died, process crashed, user
//     ctrl-C'd). Default: 7 days, matching AWS's default lifecycle
//     rule for incomplete uploads.
//
//   - Interval: how often the loop runs. Default: 1 hour.
type MultipartCleanupConfig struct {
	DoneRetention     time.Duration
	AbortedRetention  time.Duration
	StuckUploadMaxAge time.Duration
	Interval          time.Duration
}

// DefaultMultipartCleanupConfig returns the defaults documented on
// MultipartCleanupConfig.
func DefaultMultipartCleanupConfig() MultipartCleanupConfig {
	return MultipartCleanupConfig{
		DoneRetention:     24 * time.Hour,
		AbortedRetention:  1 * time.Hour,
		StuckUploadMaxAge: 7 * 24 * time.Hour,
		Interval:          1 * time.Hour,
	}
}

// SweepMultipartCleanup runs ONE cleanup pass: retires active Pebble
// multipart rows past their retention threshold, then walks legacy
// manifest dirs for older deployments. Returns the per-pass summary.
//
// Used by the CLI's recurring loop (cli/multipart_cleanup_loop.go)
// AND by tests that want to drive a single pass deterministically.
func SweepMultipartCleanup(svc *domain.Service, cfg MultipartCleanupConfig) MultipartCleanupResult {
	now := time.Now()
	var res MultipartCleanupResult

	activeUploads, err := svc.ListActiveMultipartUploads("")
	if err != nil {
		res.Errors++
	} else {
		for _, upload := range activeUploads {
			res.StageDirsScanned++
			manifest := manifestFromActive(upload, nil)
			if action := decideRetention(manifest, now, cfg); action != cleanupKeep {
				if err := applyActiveCleanupAction(svc, upload, action); err != nil {
					res.Errors++
					continue
				}
				switch action {
				case cleanupRetireDone:
					res.DoneRetired++
				case cleanupRetireAborted:
					res.AbortedRetired++
				case cleanupForceAbort:
					res.StuckAborted++
				}
			}
		}
	}

	for _, root := range svc.ListRoot() {
		mountAbs, err := svc.ResolveAbsPath(root.ID)
		if err != nil {
			continue
		}
		stageRoot := filepath.Join(mountAbs, ".fg-uploads")
		entries, err := os.ReadDir(stageRoot)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			res.Errors++
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || !strings.HasPrefix(e.Name(), multipartDirPrefix) {
				continue
			}
			res.StageDirsScanned++
			stageDir := filepath.Join(stageRoot, e.Name())
			manifest, err := readManifest(stageDir)
			if err != nil {
				// Unparseable manifest — leave it alone. An operator
				// can investigate; auto-deleting it would mask bugs.
				res.Errors++
				continue
			}
			if action := decideRetention(manifest, now, cfg); action != cleanupKeep {
				if err := applyCleanupAction(svc, stageDir, manifest, action); err != nil {
					res.Errors++
					continue
				}
				switch action {
				case cleanupRetireDone:
					res.DoneRetired++
				case cleanupRetireAborted:
					res.AbortedRetired++
				case cleanupForceAbort:
					res.StuckAborted++
				}
			}
		}
	}
	return res
}

func manifestFromActive(upload domain.ActiveMultipartUpload, parts []domain.ActiveMultipartPart) *multipartManifest {
	m := &multipartManifest{
		Format:             multipartManifestFormat,
		Kind:               multipartManifestKind,
		UploadID:           upload.UploadID,
		Bucket:             upload.Bucket,
		Key:                upload.Key,
		Initiated:          upload.Initiated,
		ContentType:        upload.ContentType,
		ContentEncoding:    upload.ContentEncoding,
		ContentDisposition: upload.ContentDisposition,
		UserMetadata:       upload.UserMetadata,
		Parts:              map[int]multipartPart{},
		Phase:              multipartPhase(upload.Phase),
		CompositeETag:      upload.CompositeETag,
		WholeBodyMD5:       upload.WholeBodyMD5,
		CompletedFileID:    upload.CompletedFileID,
		CompletedAt:        upload.CompletedAt,
	}
	for _, part := range parts {
		m.Parts[part.PartNumber] = multipartPart{
			PartNumber: part.PartNumber,
			Size:       part.Size,
			ETag:       part.ETag,
			UpdatedAt:  part.UpdatedAt,
		}
	}
	return m
}

// cleanupAction is the decision the policy makes for one upload.
type cleanupAction int

const (
	cleanupKeep cleanupAction = iota
	cleanupRetireDone
	cleanupRetireAborted
	cleanupForceAbort
)

// decideRetention is the policy split out so it can be unit-tested
// without filesystem state.
//
// IMPORTANT: phase=committing is NOT eligible for force-abort,
// even past StuckUploadMaxAge. A request that's actively being
// committed sets phase=committing right before doing the rename
// + Pebble batch — racing the cleanup loop against an in-flight
// CompleteMultipartUpload would let us delete the staging dir
// AND the durable record out from under it. The startup recovery
// sweep is the only thing that touches phase=committing, and it
// uses the durable-record check for safety. If a phase=committing
// upload is genuinely stuck post-restart, the recovery sweep will
// see it and decide what to do.
func decideRetention(m *multipartManifest, now time.Time, cfg MultipartCleanupConfig) cleanupAction {
	switch m.Phase {
	case phaseDone:
		if m.CompletedAt > 0 && now.Sub(time.UnixMilli(m.CompletedAt)) >= cfg.DoneRetention {
			return cleanupRetireDone
		}
	case phaseAborted:
		// Aborted manifests don't track a CompletedAt; fall back
		// to Initiated. AbortMultipartUpload already removed the
		// parts/ dir, so there's nothing heavy left.
		ts := m.CompletedAt
		if ts == 0 {
			ts = m.Initiated
		}
		if ts > 0 && now.Sub(time.UnixMilli(ts)) >= cfg.AbortedRetention {
			return cleanupRetireAborted
		}
	case phaseInProgress:
		if m.Initiated > 0 && now.Sub(time.UnixMilli(m.Initiated)) >= cfg.StuckUploadMaxAge {
			return cleanupForceAbort
		}
	case phaseCommitting:
		// Deliberately do nothing. See type comment.
	}
	return cleanupKeep
}

// applyCleanupAction performs the side effects: removing the
// staging dir, deleting the durable Pebble record. Errors are
// returned so the caller can count them.
func applyCleanupAction(svc *domain.Service, stageDir string, m *multipartManifest, action cleanupAction) error {
	switch action {
	case cleanupRetireDone, cleanupForceAbort:
		// Both delete the durable record (if present) AND the
		// staging dir. cleanupForceAbort is essentially a "no
		// retry possible from here on" abort applied to a stuck
		// upload — same on-disk effect as cleanupRetireDone.
		if uploadID, ok := decodeUploadID(m.UploadID); ok {
			if err := svc.DeleteMultipartUploadRecord(uploadID); err != nil {
				return fmt.Errorf("delete pebble record %s: %w", m.UploadID, err)
			}
		}
		if err := os.RemoveAll(stageDir); err != nil {
			return fmt.Errorf("remove stage dir %s: %w", stageDir, err)
		}
	case cleanupRetireAborted:
		// Aborted has no durable record (Abort removes the
		// staging dir; a record would only exist after a
		// successful Complete). Just remove whatever's left.
		if err := os.RemoveAll(stageDir); err != nil {
			return fmt.Errorf("remove aborted stage dir %s: %w", stageDir, err)
		}
	}
	return nil
}

func applyActiveCleanupAction(svc *domain.Service, upload domain.ActiveMultipartUpload, action cleanupAction) error {
	switch action {
	case cleanupRetireDone, cleanupForceAbort:
		if uploadID, ok := decodeUploadID(upload.UploadID); ok {
			if err := svc.DeleteMultipartUploadRecord(uploadID); err != nil {
				return fmt.Errorf("delete pebble record %s: %w", upload.UploadID, err)
			}
		}
		if err := svc.DeleteActiveMultipartUpload(upload.UploadID); err != nil {
			return fmt.Errorf("delete active upload %s: %w", upload.UploadID, err)
		}
		if err := os.RemoveAll(upload.StageDir); err != nil {
			return fmt.Errorf("remove stage dir %s: %w", upload.StageDir, err)
		}
	case cleanupRetireAborted:
		if err := svc.DeleteActiveMultipartUpload(upload.UploadID); err != nil {
			return fmt.Errorf("delete active upload %s: %w", upload.UploadID, err)
		}
		if err := os.RemoveAll(upload.StageDir); err != nil {
			return fmt.Errorf("remove aborted stage dir %s: %w", upload.StageDir, err)
		}
	}
	return nil
}

// decodeUploadID parses the 32-hex-char on-disk uploadId string
// into the 16-byte form the durable Pebble record uses. Returns
// false on a malformed id (cleanup just skips the record-delete
// rather than crash).
func decodeUploadID(s string) ([16]byte, bool) {
	var out [16]byte
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 16 {
		return out, false
	}
	copy(out[:], raw)
	return out, true
}
