//go:build linux

package s3

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/k2b-dev/filegate/v3/domain"
	"github.com/k2b-dev/filegate/v3/infra/activity"
	"github.com/k2b-dev/filegate/v3/infra/eventbus"
	"github.com/k2b-dev/filegate/v3/infra/filesystem"
	indexpebble "github.com/k2b-dev/filegate/v3/infra/pebble"
)

func TestActivityRecordsS3Mutation(t *testing.T) {
	baseDir := t.TempDir()
	indexDir := t.TempDir()
	idx, err := indexpebble.Open(indexDir, 16<<20)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	bus := eventbus.New()
	t.Cleanup(func() {
		bus.Close()
		_ = idx.Close()
	})

	mountPath := baseDir + "/data"
	if err := os.MkdirAll(mountPath, 0o755); err != nil {
		t.Fatalf("mkdir mount: %v", err)
	}
	svc, err := domain.NewService(idx, filesystem.New(), bus, []string{mountPath}, 1000)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	log := activity.NewRing(10)
	handler, err := NewHandler(svc, Options{
		Region:      testRegion,
		AccessKey:   testAccessKey,
		SecretKey:   testSecretKey,
		ActivityLog: log,
	})
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}

	body := []byte("hello")
	req := httptest.NewRequest(http.MethodPut, "/data/a.txt", bytes.NewReader(body))
	req.Host = "example.com"
	req.Header.Set("X-Filegate-Actor", "sync-client")
	signRequestPayload(req, body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("put status=%d body=%s", rec.Code, rec.Body.String())
	}

	events := log.Snapshot(1)
	if len(events) != 1 {
		t.Fatalf("events=%d, want 1", len(events))
	}
	event := events[0]
	if event.Operation != "s3.PutObject" || event.Outcome != activity.OutcomeSucceeded {
		t.Fatalf("event operation/outcome=%s/%s", event.Operation, event.Outcome)
	}
	if event.Actor.Kind != activity.ActorS3Key || event.Actor.ID != testAccessKey || event.Actor.DelegatedActor != "sync-client" {
		t.Fatalf("actor=%+v", event.Actor)
	}
	if event.Target == nil || event.Target.Kind != "s3_object" || event.Target.Path != "data/a.txt" {
		t.Fatalf("target=%+v", event.Target)
	}
}
