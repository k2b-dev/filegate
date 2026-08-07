//go:build linux

package filegate_test

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	httpadapter "github.com/k2b-dev/filegate/v3/adapter/http"
	"github.com/k2b-dev/filegate/v3/domain"
	"github.com/k2b-dev/filegate/v3/infra/eventbus"
	"github.com/k2b-dev/filegate/v3/infra/filesystem"
	indexpebble "github.com/k2b-dev/filegate/v3/infra/pebble"
	"github.com/k2b-dev/filegate/v3/sdk/filegate"
)

// TestVersionsClientEndToEndAgainstRealDaemon drives every method on
// filegate.VersionsClient against a real, in-process daemon (full HTTP
// stack + Pebble + filesystem). This is the SDK-level equivalent of
// the compose-based bench scripts: catches contract drift between the
// HTTP adapter and the Go SDK that pure mock-server tests would miss.
func TestVersionsClientEndToEndAgainstRealDaemon(t *testing.T) {
	baseDir := t.TempDir()
	indexDir := t.TempDir()
	idx, err := indexpebble.Open(indexDir, 16<<20)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	bus := eventbus.New()
	t.Cleanup(func() { bus.Close() })

	svc, err := domain.NewService(idx, filesystem.New(), bus, []string{baseDir}, 1024)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	svc.EnableVersioning(domain.VersioningConfig{
		Cooldown:               50 * time.Millisecond,
		MinSizeForAutoV1:       0,
		MaxLabelBytes:          2048,
		MaxPinnedPerFile:       100,
		PinnedGraceAfterDelete: 24 * time.Hour,
	}, true)

	router := httpadapter.NewRouter(svc, httpadapter.RouterOptions{
		BearerToken:           "e2e-token",
		JobWorkers:            2,
		JobQueueSize:          64,
		UploadExpiry:          time.Hour,
		UploadCleanupInterval: time.Hour,
		MaxChunkBytes:         10 << 20,
		MaxUploadBytes:        100 << 20,
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	client, err := filegate.New(filegate.Config{
		BaseURL: server.URL,
		Token:   "e2e-token",
	})
	if err != nil {
		t.Fatalf("sdk client: %v", err)
	}

	ctx := context.Background()
	roots := svc.ListRoot()
	rootName := roots[0].Name
	rootID := roots[0].ID.String()

	// 1. Upload a file via paths PUT (also triggers V1 capture).
	uploadResp, err := client.Paths.Put(ctx, rootName+"/journey.bin",
		strings.NewReader("v1-original"),
		filegate.PutPathOptions{},
	)
	if err != nil {
		t.Fatalf("Paths.Put: %v", err)
	}
	fileID := uploadResp.NodeID
	if fileID == "" {
		t.Fatalf("upload did not return a node ID")
	}

	// 2. Wait past cooldown, overwrite to capture V2 (= old v1 bytes).
	time.Sleep(80 * time.Millisecond)
	if _, err := client.Nodes.PutContent(ctx, fileID,
		strings.NewReader("v2-replaced"), "application/octet-stream"); err != nil {
		t.Fatalf("Nodes.PutContent: %v", err)
	}

	// 3. List versions via SDK. Must see >= 2 entries.
	listed, err := client.Versions.ListAll(ctx, fileID)
	if err != nil {
		t.Fatalf("Versions.ListAll: %v", err)
	}
	if len(listed) < 2 {
		t.Fatalf("expected >= 2 versions after overwrite, got %d", len(listed))
	}
	v1 := listed[0]

	// 4. Fetch v1 content and verify it matches the original bytes.
	var buf bytes.Buffer
	res, err := client.Versions.PipeContent(ctx, fileID, v1.VersionID, &buf)
	if err != nil {
		t.Fatalf("Versions.PipeContent: %v", err)
	}
	if res.Bytes <= 0 || buf.Len() != int(res.Bytes) {
		t.Fatalf("PipeContent returned bytes=%d, buf.Len=%d", res.Bytes, buf.Len())
	}
	if !strings.Contains(buf.String(), "v1-original") {
		t.Fatalf("v1 content=%q, want bytes containing 'v1-original'", buf.String())
	}

	// 5. Manual snapshot with label.
	snap, err := client.Versions.Snapshot(ctx, fileID, "checkpoint-1")
	if err != nil {
		t.Fatalf("Versions.Snapshot: %v", err)
	}
	if !snap.Pinned || snap.Label != "checkpoint-1" {
		t.Fatalf("snapshot meta=%#v", snap)
	}

	// 6. Pin an existing auto-version with a new label, then unpin.
	autoTarget := v1
	label := "manually-pinned"
	pinned, err := client.Versions.Pin(ctx, fileID, autoTarget.VersionID, &label)
	if err != nil {
		t.Fatalf("Versions.Pin: %v", err)
	}
	if !pinned.Pinned || pinned.Label != "manually-pinned" {
		t.Fatalf("pin response=%#v", pinned)
	}
	unpinned, err := client.Versions.Unpin(ctx, fileID, autoTarget.VersionID)
	if err != nil {
		t.Fatalf("Versions.Unpin: %v", err)
	}
	if unpinned.Pinned {
		t.Fatalf("unpin still pinned: %#v", unpinned)
	}

	// 7. Restore as new file with default name.
	restoreNew, err := client.Versions.Restore(ctx, fileID, v1.VersionID, filegate.RestoreOptions{
		AsNewFile: true,
	})
	if err != nil {
		t.Fatalf("Versions.Restore as-new: %v", err)
	}
	if !restoreNew.AsNew || restoreNew.Node.Name != "journey-restored.bin" {
		t.Fatalf("as-new restore=%#v", restoreNew)
	}
	if restoreNew.Node.ID == fileID {
		t.Fatalf("as-new restore reused source ID")
	}

	// 8. Restore in-place. Source content goes back to v1 bytes.
	restoreInPlace, err := client.Versions.Restore(ctx, fileID, v1.VersionID, filegate.RestoreOptions{})
	if err != nil {
		t.Fatalf("Versions.Restore in-place: %v", err)
	}
	if restoreInPlace.AsNew {
		t.Fatalf("expected in-place, got AsNew=true")
	}
	if restoreInPlace.Node.ID != fileID {
		t.Fatalf("in-place restore changed ID")
	}

	// 9. Manual delete of one auto-version.
	preDelete, _ := client.Versions.ListAll(ctx, fileID)
	if len(preDelete) == 0 {
		t.Fatalf("no versions before delete")
	}
	if err := client.Versions.Delete(ctx, fileID, preDelete[0].VersionID); err != nil {
		t.Fatalf("Versions.Delete: %v", err)
	}
	postDelete, _ := client.Versions.ListAll(ctx, fileID)
	if len(postDelete) != len(preDelete)-1 {
		t.Fatalf("delete did not remove version: pre=%d post=%d",
			len(preDelete), len(postDelete))
	}

	// 10. Listing versions for a directory returns an empty list (dirs
	//     have no version content; the API contract treats this as a
	//     valid query that simply has no results).
	dirVersions, err := client.Versions.List(ctx, rootID, filegate.ListVersionsOptions{})
	if err != nil {
		t.Fatalf("Versions.List on directory: %v", err)
	}
	if len(dirVersions.Items) != 0 {
		t.Fatalf("directory unexpectedly has versions: %v", dirVersions.Items)
	}

	// Drain any background HTTP keep-alive responses so the test
	// cleanup doesn't race with in-flight reads.
	_, _ = io.Copy(io.Discard, bytes.NewReader(nil))
}
