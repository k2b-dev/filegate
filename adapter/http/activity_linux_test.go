//go:build linux

package httpadapter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	apiv1 "github.com/k2b-dev/filegate/v3/api/v1"
	"github.com/k2b-dev/filegate/v3/infra/activity"
)

func TestActivityRecordsAuthenticatedRESTMutation(t *testing.T) {
	log := activity.NewRing(10)
	r, _, cleanup := newTestRouterWithCustomLimits(t, t.TempDir(), t.TempDir(), RouterOptions{
		BearerToken: "test-token",
		ActivityLog: log,
		Rescan:      func() error { return nil },
	})
	defer cleanup()

	req := authedRequest(http.MethodPost, "/v1/index/rescan")
	req.Header.Set("X-Filegate-Actor", "valentin")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("rescan status=%d body=%s", rec.Code, rec.Body.String())
	}

	listReq := authedRequest(http.MethodGet, "/v1/activity?limit=5")
	listRec := httptest.NewRecorder()
	r.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("activity status=%d body=%s", listRec.Code, listRec.Body.String())
	}

	var out apiv1.ActivityListResponse
	if err := json.Unmarshal(listRec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode activity: %v", err)
	}
	if out.Total != 1 || out.Capacity != 10 || len(out.Items) != 1 {
		t.Fatalf("activity summary total=%d capacity=%d items=%d", out.Total, out.Capacity, len(out.Items))
	}
	if out.Retained != 1 || out.Limit != 1 || out.Offset != 0 {
		t.Fatalf("activity page retained=%d limit=%d offset=%d", out.Retained, out.Limit, out.Offset)
	}
	if len(out.Operations) != 1 || out.Operations[0] != "index.rescan" {
		t.Fatalf("operations=%v", out.Operations)
	}
	event := out.Items[0]
	if event.Operation != "index.rescan" || event.Outcome != "succeeded" {
		t.Fatalf("event operation/outcome=%s/%s", event.Operation, event.Outcome)
	}
	if event.Actor.Kind != "bearer_token" || event.Actor.ID == "" || event.Actor.DelegatedActor != "valentin" {
		t.Fatalf("actor=%+v", event.Actor)
	}
	if event.Target == nil || event.Target.Kind != "index" {
		t.Fatalf("target=%+v", event.Target)
	}
	if event.RequestID == "" {
		t.Fatalf("request id should be recorded")
	}
}

func TestActivityListResponseFiltersAndPaginates(t *testing.T) {
	log := activity.NewRing(10)
	log.Record(activity.Event{
		Actor:     activity.Actor{Kind: activity.ActorBearer, ID: "bearer:a", DelegatedActor: "alice"},
		Operation: "path.put",
		Outcome:   activity.OutcomeFailed,
		Target:    &activity.Target{Kind: "path", Path: "mount-a/report.txt"},
		Error:     "Conflict",
	})
	log.Record(activity.Event{
		Actor:     activity.Actor{Kind: activity.ActorBearer, ID: "bearer:b", DelegatedActor: "bob"},
		Operation: "index.rescan",
		Outcome:   activity.OutcomeSucceeded,
		Target:    &activity.Target{Kind: "index"},
	})
	log.Record(activity.Event{
		Actor:     activity.Actor{Kind: activity.ActorS3Key, ID: "AKIA"},
		Operation: "s3.PutObject",
		Outcome:   activity.OutcomeSucceeded,
		Target:    &activity.Target{Kind: "s3_object", Path: "photos/a.jpg"},
	})

	page := activityListResponse(log, activityListQuery{limit: 1, offset: 1})
	if page.Total != 3 || page.Retained != 3 || len(page.Items) != 1 {
		t.Fatalf("page total=%d retained=%d items=%d", page.Total, page.Retained, len(page.Items))
	}
	if page.Items[0].Operation != "index.rescan" {
		t.Fatalf("page item=%s, want index.rescan", page.Items[0].Operation)
	}

	filtered := activityListResponse(log, activityListQuery{limit: 10, q: "report", outcome: "failed"})
	if filtered.Total != 1 || len(filtered.Items) != 1 {
		t.Fatalf("filtered total=%d items=%d", filtered.Total, len(filtered.Items))
	}
	if filtered.Items[0].Operation != "path.put" {
		t.Fatalf("filtered operation=%s, want path.put", filtered.Items[0].Operation)
	}

	opFiltered := activityListResponse(log, activityListQuery{limit: 10, operation: "s3.PutObject"})
	if opFiltered.Total != 1 || opFiltered.Items[0].Target.Path != "photos/a.jpg" {
		t.Fatalf("op filtered=%+v", opFiltered)
	}
	if got := opFiltered.Operations; len(got) != 3 || got[0] != "index.rescan" || got[1] != "path.put" || got[2] != "s3.PutObject" {
		t.Fatalf("operations=%v", got)
	}
}
