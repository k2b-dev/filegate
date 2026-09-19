//go:build linux

package integration_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	httpadapter "github.com/k2b-dev/filegate/v6/adapter/http"
	api "github.com/k2b-dev/filegate/v6/api/v1"
	"github.com/k2b-dev/filegate/v6/domain"
)

func leaseRequest(t *testing.T, method, url, body string, backend bool, want int, out any) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if backend {
		req.Header.Set("Authorization", "Bearer "+strings.Repeat("t", 32))
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != want {
		t.Fatalf("%s %s: status %d want %d: %s", method, url, res.StatusCode, want, data)
	}
	if out != nil {
		if err = json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s: %v", data, err)
		}
	}
}

func TestSessionLeasesKeepCommitAndRenewalBackendOnly(t *testing.T) {
	x, server, _ := server(t)
	base := server.URL + "/v1/roots/test/uploads/sessions"
	var created api.SessionCreated
	leaseRequest(t, "POST", base, `{"path":"private/file","size":3,"ownership":{"mode":"0600"},"metadata":{"secret":"private backend data"}}`, true, 201, &created)
	if created.Session.State != domain.SessionOpen || len(created.Lease.Operations) != 2 || !created.Lease.Expires.Before(created.Session.Expires) {
		t.Fatalf("unexpected response: %+v", created)
	}
	sessionURL := base + "/" + created.Session.ID
	for _, tc := range []struct{ method, path, body string }{{"GET", sessionURL, ""}, {"POST", sessionURL + "/lease", "{}"}, {"POST", sessionURL + "/commit", ""}, {"DELETE", sessionURL, ""}} {
		leaseRequest(t, tc.method, tc.path, tc.body, false, 401, nil)
	}
	var public map[string]json.RawMessage
	leaseRequest(t, "GET", created.Lease.URL, "", false, 200, &public)
	for _, field := range []string{"path", "options", "result", "metadata"} {
		if _, ok := public[field]; ok {
			t.Fatalf("browser status contains %s", field)
		}
	}
	leaseRequest(t, "POST", created.Lease.URL, "", false, 403, nil)
	leaseRequest(t, "DELETE", created.Lease.URL, "", false, 403, nil)
	leaseRequest(t, "PUT", created.Lease.URL+"?segment=0&path=other&mode=0777", "abc", false, 200, nil)
	if _, err := x.r.Stat("private/file"); err == nil {
		t.Fatal("segment published without backend commit")
	}
	var lease api.SessionLease
	leaseRequest(t, "POST", sessionURL+"/lease", `{"expiresIn":300,"allowAbort":true}`, true, 201, &lease)
	if len(lease.Operations) != 3 {
		t.Fatal(lease)
	}
	var result domain.Node
	leaseRequest(t, "POST", sessionURL+"/commit", "", true, 200, &result)
	if result.Path != "private/file" || result.Mode != "0600" || result.Size != 3 {
		t.Fatalf("unbound commit result: %+v", result)
	}
	var again domain.Node
	leaseRequest(t, "POST", sessionURL+"/commit", "", true, 200, &again)
	if result != again {
		t.Fatalf("commit changed: %+v %+v", result, again)
	}
	leaseRequest(t, "DELETE", sessionURL, "", true, 409, nil)
	leaseRequest(t, "DELETE", lease.URL, "", false, 409, nil)
	leaseRequest(t, "POST", sessionURL+"/lease", "{}", true, 409, nil)
	var status domain.Session
	leaseRequest(t, "GET", sessionURL, "", true, 200, &status)
	if status.State != domain.SessionCommitted || status.Result == nil || status.RetainUntil == nil {
		t.Fatal(status)
	}
}

func TestSessionLeaseAbortExpiryAndValidation(t *testing.T) {
	x, server, _ := server(t)
	base := server.URL + "/v1/roots/test/uploads/sessions"
	for _, ttl := range []int{-1, 301} {
		leaseRequest(t, "POST", base, fmt.Sprintf(`{"path":"bad","size":0,"expiresIn":%d}`, ttl), true, 400, nil)
	}
	var c api.SessionCreated
	leaseRequest(t, "POST", base, `{"path":"aborted","size":0,"allowAbort":true}`, true, 201, &c)
	sessionURL := base + "/" + c.Session.ID
	leaseRequest(t, "DELETE", c.Lease.URL, "", false, 204, nil)
	leaseRequest(t, "DELETE", c.Lease.URL, "", false, 204, nil)
	leaseRequest(t, "POST", sessionURL+"/commit", "", true, 409, nil)
	leaseRequest(t, "POST", sessionURL+"/lease", "{}", true, 409, nil)
	var s domain.Session
	leaseRequest(t, "GET", sessionURL, "", true, 200, &s)
	if s.State != domain.SessionAborted {
		t.Fatal(s)
	}
	leaseRequest(t, "POST", base, `{"path":"expired","size":0}`, true, 201, &c)
	s = c.Session
	s.Expires = time.Now().Add(-time.Second)
	if err := x.r.State.Put("session/"+s.ID, s); err != nil {
		t.Fatal(err)
	}
	sessionURL = base + "/" + s.ID
	leaseRequest(t, "GET", sessionURL, "", true, 200, &s)
	if s.State != domain.SessionExpired || s.RetainUntil == nil {
		t.Fatal(s)
	}
	leaseRequest(t, "POST", sessionURL+"/commit", "", true, 410, nil)
	leaseRequest(t, "DELETE", sessionURL, "", true, 410, nil)
	leaseRequest(t, "POST", sessionURL+"/lease", "{}", true, 410, nil)
}

// These gates model a slow state operation while synctest advances its virtual
// clock. There are no wall-clock sleeps or timing assumptions in this test.
type leaseBlockingState struct {
	domain.State
	started  chan struct{}
	release  chan struct{}
	creation bool
	enabled  bool
	records  map[string][]byte
}

func (s *leaseBlockingState) Put(key string, value any) error {
	if s.enabled && s.creation && strings.HasPrefix(key, "session/") {
		close(s.started)
		<-s.release
	}
	b, err := json.Marshal(value)
	if err == nil {
		s.records[key] = b
	}
	return err
}

func (s *leaseBlockingState) Batch(changes []domain.Change) error {
	if s.enabled && s.creation {
		for _, change := range changes {
			if strings.HasPrefix(change.Key, "session/") {
				close(s.started)
				<-s.release
				break
			}
		}
	}
	for _, change := range changes {
		if change.Delete {
			delete(s.records, change.Key)
		} else {
			s.records[change.Key] = append([]byte{}, change.Value...)
		}
	}
	return nil
}

func (s *leaseBlockingState) Get(key string, value any) error {
	if s.enabled && !s.creation && strings.HasPrefix(key, "session/") {
		close(s.started)
		<-s.release
	}
	b, ok := s.records[key]
	if !ok {
		return os.ErrNotExist
	}
	return json.Unmarshal(b, value)
}

func TestSessionLeaseStartsAfterBlockingStateOperation(t *testing.T) {
	for _, creation := range []bool{true, false} {
		t.Run(fmt.Sprintf("creation=%v", creation), func(t *testing.T) {
			x := setup(t, false, false)
			synctest.Test(t, func(t *testing.T) {
				original := x.r.State
				gate := &leaseBlockingState{started: make(chan struct{}), release: make(chan struct{}), creation: creation, records: map[string][]byte{}}
				x.r.State = gate
				defer func() { x.r.State = original }()
				endpoint := "/v1/roots/test/uploads/sessions"
				body := `{"path":"file","size":0}`
				if !creation {
					s, err := x.r.CreateSession("file", 0, domain.WriteOptions{}, "")
					if err != nil {
						t.Fatal(err)
					}
					endpoint += "/" + s.ID + "/lease"
					body = "{}"
				}
				gate.enabled = true
				h := httpadapter.New([]*domain.Root{x.r}, httpadapter.Options{Token: "secret"})
				w := httptest.NewRecorder()
				req := httptest.NewRequest("POST", endpoint, strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer secret")
				done := make(chan struct{})
				go func() { defer close(done); h.ServeHTTP(w, req) }()
				<-gate.started
				time.Sleep(2 * time.Minute)
				close(gate.release)
				<-done
				if w.Code != 201 {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
				var lease api.SessionLease
				if creation {
					var result api.SessionCreated
					if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
					if result.Lease == nil {
						t.Fatal("open session has no lease")
					}
					lease = *result.Lease
				} else if err := json.Unmarshal(w.Body.Bytes(), &lease); err != nil {
					t.Fatal(err)
				}
				if want := time.Now().Unix() + 60; lease.Expires.Unix() != want {
					t.Fatalf("lease expiry %v does not start after blocked operation; want %v", lease.Expires, time.Unix(want, 0))
				}
			})
		})
	}
}
