//go:build linux

package integration_test

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/k2b-dev/filegate/v5/api/v1"
	"github.com/k2b-dev/filegate/v5/domain"
)

func TestSessionIdempotentOpenAndTerminalReplay(t *testing.T) {
	x := setup(t, false, false)
	opts := domain.WriteOptions{Metadata: domain.Metadata{"message": "same"}}
	first, err := x.r.CreateSession("file", 3, opts, "open-key")
	if err != nil {
		t.Fatal(err)
	}
	again, err := x.r.CreateSession("file", 3, opts, "open-key")
	if err != nil || !reflect.DeepEqual(first, again) {
		t.Fatalf("lost-open retry: %+v %+v %v", first, again, err)
	}
	for _, changed := range []struct {
		path    string
		size    int64
		options domain.WriteOptions
	}{
		{"other", 3, opts}, {"file", 4, opts}, {"file", 3, domain.WriteOptions{Metadata: domain.Metadata{"message": "different"}}},
	} {
		if _, err := x.r.CreateSession(changed.path, changed.size, changed.options, "open-key"); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("key mismatch accepted: %v", err)
		}
	}
	if _, err := x.r.PutSegment(ctx, first.ID, 0, strings.NewReader("abc")); err != nil {
		t.Fatal(err)
	}
	result, err := x.r.CommitSession(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	reopen(t, x)
	terminal, err := x.r.CreateSession("file", 3, opts, "open-key")
	if err != nil || terminal.State != domain.SessionCommitted || terminal.Result == nil || !reflect.DeepEqual(*terminal.Result, result) {
		t.Fatalf("terminal replay: %+v %v", terminal, err)
	}
	if !terminal.Expires.Equal(first.Expires) {
		t.Fatal("replay extended upload expiry")
	}
}

func TestSessionOpenKeyFollowsReceiptRetention(t *testing.T) {
	x := setup(t, false, false)
	original, err := x.r.CreateSession("file", 0, domain.WriteOptions{}, "key")
	if err != nil {
		t.Fatal(err)
	}
	original.Expires = time.Now().Add(-time.Minute)
	if err := x.state.Put("session/"+original.ID, original); err != nil {
		t.Fatal(err)
	}
	expired, err := x.r.CreateSession("file", 0, domain.WriteOptions{}, "key")
	if err != nil || expired.ID != original.ID || expired.State != domain.SessionExpired {
		t.Fatalf("expired replay: %+v %v", expired, err)
	}
	past := time.Now().Add(-time.Minute)
	expired.RetainUntil = &past
	if err := x.state.Put("done/"+expired.ID, expired); err != nil {
		t.Fatal(err)
	}
	replacement, err := x.r.CreateSession("new", 0, domain.WriteOptions{}, "key")
	if err != nil || replacement.ID == original.ID {
		t.Fatalf("key did not expire with receipt: %+v %v", replacement, err)
	}
	if _, err := x.r.CreateSession("file", 0, domain.WriteOptions{}, strings.Repeat("x", 129)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("oversize key: %v", err)
	}
}

func TestSessionSparseReceiptPaginationAndDuplicateChunks(t *testing.T) {
	x := setup(t, false, false)
	s, err := x.r.CreateSession("file", 7, domain.WriteOptions{}, "")
	if err != nil {
		t.Fatal(err)
	}
	s.ChunkSize = 1
	if err := x.state.Put("session/"+s.ID, s); err != nil {
		t.Fatal(err)
	}
	for _, index := range []int{5, 0, 2} {
		if _, err := x.r.PutSegment(ctx, s.ID, index, strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}
	status, err := x.r.PutSegment(ctx, s.ID, 2, strings.NewReader("x"))
	if err != nil || status.Received != 3 || status.UploadedSegments != 3 {
		t.Fatalf("duplicate counted twice: %+v %v", status, err)
	}
	if _, err := x.r.PutSegment(ctx, s.ID, 2, strings.NewReader("y")); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("changed chunk accepted: %v", err)
	}
	page, err := x.r.SessionSegments(s.ID, -1, 2)
	if err != nil || len(page.Items) != 2 || page.Items[0].Index != 0 || page.Items[1].Index != 2 || page.Next == nil || *page.Next != 2 {
		t.Fatalf("first page: %+v %v", page, err)
	}
	page, err = x.r.SessionSegments(s.ID, *page.Next, 2)
	if err != nil || len(page.Items) != 1 || page.Items[0].Index != 5 || page.Next != nil {
		t.Fatalf("last page: %+v %v", page, err)
	}
	if err := x.r.AbortSession(s.ID); err != nil {
		t.Fatal(err)
	}
	page, err = x.r.SessionSegments(s.ID, -1, 2)
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("terminal chunk page: %+v %v", page, err)
	}
	var remaining int
	if err := x.state.Scan("segment/"+s.ID+"/", func(string, []byte) error { remaining++; return nil }); err != nil || remaining != 0 {
		t.Fatalf("terminal receipts leaked: %d %v", remaining, err)
	}
}

func TestSessionMigratesAcknowledgedLegacyChunksAndReceipts(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "open", true: "committed"}[terminal], func(t *testing.T) {
			x := setup(t, false, false)
			s := createFilledSession(t, x, "file", "abc")
			page, err := x.r.SessionSegments(s.ID, -1, 10)
			if err != nil {
				t.Fatal(err)
			}
			legacySegments := map[int]string{0: page.Items[0].Hash}
			key := "session/" + s.ID
			if terminal {
				if _, err := x.r.CommitSession(ctx, s.ID); err != nil {
					t.Fatal(err)
				}
				s, err = x.r.Session(s.ID)
				if err != nil {
					t.Fatal(err)
				}
				key = "done/" + s.ID
				legacySegments = map[int]string{}
			}
			data, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			var legacy map[string]any
			if err := json.Unmarshal(data, &legacy); err != nil {
				t.Fatal(err)
			}
			delete(legacy, "uploadedSegments")
			legacy["segments"] = legacySegments
			if err := x.state.Put(key, legacy); err != nil {
				t.Fatal(err)
			}
			if err := x.state.Delete("segment/" + s.ID + "/00000"); err != nil {
				t.Fatal(err)
			}
			reopen(t, x)
			got, err := x.r.Session(s.ID)
			if err != nil || got.UploadedSegments != 1 || got.Received != 3 {
				t.Fatalf("migration lost chunks: %+v %v", got, err)
			}
			if _, err := x.r.CommitSession(ctx, s.ID); err != nil {
				t.Fatal(err)
			}
			if read(t, x.r, "file") != "abc" {
				t.Fatal("migration lost bytes")
			}
			var raw map[string]json.RawMessage
			if err := x.state.Get("done/"+s.ID, &raw); err != nil {
				t.Fatal(err)
			}
			if _, ok := raw["segments"]; ok {
				t.Fatal("legacy map retained after migration")
			}
		})
	}
}

type segmentReceiptFailure struct {
	domain.State
	fail bool
}

func (s *segmentReceiptFailure) Batch(changes []domain.Change) error {
	for _, change := range changes {
		if s.fail && strings.HasPrefix(change.Key, "segment/") && !change.Delete {
			return errors.New("injected chunk receipt failure")
		}
	}
	return s.State.Batch(changes)
}
func TestSessionChunkReceiptFailureCanRetryWithoutDoubleCounting(t *testing.T) {
	x := setup(t, false, false)
	s, err := x.r.CreateSession("file", 3, domain.WriteOptions{}, "")
	if err != nil {
		t.Fatal(err)
	}
	state := &segmentReceiptFailure{State: x.state, fail: true}
	x.r.State = state
	if _, err := x.r.PutSegment(ctx, s.ID, 0, strings.NewReader("abc")); err == nil {
		t.Fatal("failure not injected")
	}
	status, err := x.r.Session(s.ID)
	if err != nil || status.Received != 0 || status.UploadedSegments != 0 {
		t.Fatalf("failed receipt partially committed: %+v %v", status, err)
	}
	state.fail = false
	if _, err := x.r.PutSegment(ctx, s.ID, 0, strings.NewReader("xyz")); err != nil {
		t.Fatal(err)
	}
	if _, err := x.r.CommitSession(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if read(t, x.r, "file") != "xyz" {
		t.Fatal("unacknowledged bytes published")
	}
}

func TestHTTPSessionIdempotencyAndDirectReceiptPage(t *testing.T) {
	_, server, _ := server(t)
	endpoint := server.URL + "/v1/roots/test/uploads/sessions"
	var first, again api.SessionCreated
	body := `{"path":"file","size":3,"idempotencyKey":"key","expiresIn":30}`
	leaseRequest(t, "POST", endpoint, body, true, 201, &first)
	leaseRequest(t, "POST", endpoint, body, true, 201, &again)
	if first.Session.ID != again.Session.ID || first.Lease == nil || again.Lease == nil {
		t.Fatal("open retry did not return same session with lease")
	}
	leaseRequest(t, "POST", endpoint, `{"path":"other","size":3,"idempotencyKey":"key"}`, true, 409, nil)
	var status map[string]json.RawMessage
	leaseRequest(t, "PUT", first.Lease.URL+"?segment=0", "abc", false, 200, &status)
	if _, ok := status["segments"]; ok {
		t.Fatal("growing receipt map on wire")
	}
	if string(status["uploadedSegments"]) != "1" {
		t.Fatal(status)
	}
	var page api.SessionSegmentPage
	leaseRequest(t, "GET", first.Lease.URL+"?segments=1&after=-1&limit=1", "", false, 200, &page)
	if len(page.Items) != 1 || page.Items[0].Index != 0 {
		t.Fatal(page)
	}
	leaseRequest(t, "GET", endpoint+"/"+first.Session.ID+"/segments?after=-1&limit=1", "", true, 200, &page)
	leaseRequest(t, "POST", endpoint+"/"+first.Session.ID+"/commit", "", true, 200, nil)
	again = api.SessionCreated{}
	leaseRequest(t, "POST", endpoint, body, true, 201, &again)
	if again.Session.State != domain.SessionCommitted || again.Session.Result == nil || again.Lease != nil {
		t.Fatalf("terminal replay: %+v", again)
	}
}

func TestSessionIdempotencyIncludesExecutionIdentity(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root")
	}
	x := executionFixture(t, false, false)
	actor := executionView(t, x)
	if _, err := actor.CreateSession("file", 0, domain.WriteOptions{}, "key"); err != nil {
		t.Fatal(err)
	}
	if _, err := x.r.CreateSession("file", 0, domain.WriteOptions{}, "key"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("identity substitution accepted: %v", err)
	}
}

func TestSessionConcurrentOpenKeyHasOneSession(t *testing.T) {
	x := setup(t, false, false)
	const count = 12
	outcomes := make(chan domain.Session, count)
	failures := make(chan error, count)
	var wait sync.WaitGroup
	for range count {
		wait.Add(1)
		go func() {
			defer wait.Done()
			session, err := x.r.CreateSession("file", 0, domain.WriteOptions{}, "same-key")
			outcomes <- session
			failures <- err
		}()
	}
	wait.Wait()
	close(outcomes)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	id := ""
	for session := range outcomes {
		if id == "" {
			id = session.ID
		}
		if id != session.ID {
			t.Fatal("concurrent open allocated duplicate sessions")
		}
	}
	records := 0
	if err := x.state.Scan("session/", func(string, []byte) error { records++; return nil }); err != nil || records != 1 {
		t.Fatalf("session records=%d error=%v", records, err)
	}
}
