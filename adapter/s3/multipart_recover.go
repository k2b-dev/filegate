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

// recoverPendingMultipartUploads scans active Pebble multipart rows
// in phase=committing and reconciles them against the durable Pebble
// record. It also keeps a legacy manifest scan for staging dirs created
// by older builds. Two outcomes:
//
//   - Durable record present: the original Complete succeeded; the
//     crash happened between Pebble commit and the done-state write.
//     We backfill active state to phase=done so ListMultipartUploads
//     does not surface the upload as in-progress.
//
//   - Durable record absent: the crash happened before the Pebble
//     commit. We leave phase=committing untouched — a client retry
//     will redrive the Complete flow safely (parts are still on
//     disk; the domain CompleteMultipartUpload is idempotent under
//     lock + record-lookup).
//
// The sweep is best-effort: errors on individual uploads are logged
// but don't abort the loop.
func recoverPendingMultipartUploads(svc *domain.Service) {
	uploads, err := svc.ListActiveMultipartUploads("")
	if err == nil {
		for _, upload := range uploads {
			if upload.Phase == domain.MultipartUploadCommitting {
				reconcileCommittingActiveUpload(svc, upload)
			}
		}
	} else {
		fmt.Printf("[filegate-s3] recover: list active multipart uploads: %s\n", err)
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
			fmt.Printf("[filegate-s3] recover: read .fg-uploads in %s: %s\n", root.Name, err)
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || !strings.HasPrefix(e.Name(), multipartDirPrefix) {
				continue
			}
			stageDir := filepath.Join(stageRoot, e.Name())
			manifest, err := readManifest(stageDir)
			if err != nil {
				continue
			}
			if manifest.Phase != phaseCommitting {
				continue
			}
			reconcileCommittingManifest(svc, stageDir, manifest)
		}
	}
}

func reconcileCommittingActiveUpload(svc *domain.Service, upload domain.ActiveMultipartUpload) {
	uploadID, ok := decodeUploadID(upload.UploadID)
	if !ok {
		fmt.Printf("[filegate-s3] recover: malformed active uploadId %q\n", upload.UploadID)
		return
	}
	record, err := svc.LookupMultipartUploadRecord(uploadID)
	if err != nil || record == nil {
		fmt.Printf("[filegate-s3] recover: committing upload %s bucket=%s key=%s has no durable record; leaving for CompleteMultipartUpload retry (stage=%s)\n",
			upload.UploadID, upload.Bucket, upload.Key, upload.StageDir)
		return
	}
	upload.Phase = domain.MultipartUploadDone
	if upload.CompositeETag == "" {
		upload.CompositeETag = record.CompositeETag
	}
	if upload.CompletedFileID == "" {
		upload.CompletedFileID = record.FileID.String()
	}
	if upload.CompletedAt == 0 {
		if record.CompletedAt != 0 {
			upload.CompletedAt = record.CompletedAt
		} else {
			upload.CompletedAt = time.Now().UnixMilli()
		}
	}
	if err := svc.UpdateActiveMultipartUpload(upload); err != nil {
		fmt.Printf("[filegate-s3] recover: write done active upload %s: %s\n", upload.UploadID, err)
	}
}

// reconcileCommittingManifest is the per-manifest body of the
// recovery sweep. Pulled out so it can be unit-tested without
// scaffolding the whole walk.
func reconcileCommittingManifest(svc *domain.Service, stageDir string, manifest *multipartManifest) {
	uploadIDHex := manifest.UploadID
	raw, err := hex.DecodeString(uploadIDHex)
	if err != nil || len(raw) != 16 {
		fmt.Printf("[filegate-s3] recover: malformed uploadId %q in manifest %s\n", uploadIDHex, stageDir)
		return
	}
	var uploadID [16]byte
	copy(uploadID[:], raw)

	record, err := svc.LookupMultipartUploadRecord(uploadID)
	if err != nil || record == nil {
		// No durable record — the original Complete didn't reach
		// the Pebble batch. Leave phase=committing so a client
		// retry redrives the flow.
		fmt.Printf("[filegate-s3] recover: committing upload %s bucket=%s key=%s has no durable record; leaving for CompleteMultipartUpload retry (stage=%s)\n",
			uploadIDHex, manifest.Bucket, manifest.Key, stageDir)
		return
	}
	// Durable record present — the upload is committed. Backfill
	// the manifest so listing stops showing it as in-progress.
	manifest.Phase = phaseDone
	if manifest.CompositeETag == "" {
		manifest.CompositeETag = record.CompositeETag
	}
	if manifest.CompletedFileID == "" {
		manifest.CompletedFileID = record.FileID.String()
	}
	if manifest.CompletedAt == 0 {
		if record.CompletedAt != 0 {
			manifest.CompletedAt = record.CompletedAt
		} else {
			manifest.CompletedAt = time.Now().UnixMilli()
		}
	}
	if err := writeManifest(stageDir, manifest); err != nil {
		fmt.Printf("[filegate-s3] recover: write done manifest %s: %s\n", stageDir, err)
	}
}
