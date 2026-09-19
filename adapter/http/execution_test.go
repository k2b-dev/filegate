package httpadapter

import (
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/k2b-dev/filegate/v6/domain"
)

func TestExecutionHeaderStrictAndNormalized(t *testing.T) {
	for _, value := range []string{"", "null", `{}`, `{"uid":1}`, `{"gid":1}`, `{"uid":0,"gid":1}`, `{"uid":1,"gid":4294967295}`, `{"uid":1,"gid":1,"extra":true}`, `{"uid":1,"gid":1} {}`, strings.Repeat(" ", 4097)} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set(ExecutionHeader, value)
		if _, err := requestExecution(r); err == nil {
			t.Errorf("accepted invalid execution header %q", value)
		}
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set(ExecutionHeader, `{"uid":1001,"gid":100,"groups":[200,100,200]}`)
	identity, err := requestExecution(r)
	if err != nil || identity.UID != 1001 || identity.GID != 100 || len(identity.Groups) != 2 || identity.Groups[0] != 100 {
		t.Fatalf("normalization: %+v %v", identity, err)
	}
	r.Header.Add(ExecutionHeader, `{"uid":2001,"gid":100}`)
	if _, err := requestExecution(r); err == nil {
		t.Fatal("accepted multiple execution headers")
	}
}

func TestExecutionBoundToCapabilityAndSessionRenewal(t *testing.T) {
	h := New(nil, Options{Token: "secret"})
	actor, _ := domain.NormalizeExecution(&domain.ExecutionIdentity{UID: 1001, GID: 100, Groups: []uint32{200}})
	s := domain.Session{ID: "session", Root: "files", Expires: time.Now().Add(time.Hour), Execution: actor}
	lease, err := h.sessionLease(s, 60, false)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimPrefix(lease.URL, "/v1/direct/")
	c, err := h.verify(token)
	if err != nil || !sameExecution(c.Execution, actor) {
		t.Fatalf("identity not bound: %+v %v", c, err)
	}
	c.Execution.UID++
	payload, _ := json.Marshal(c)
	changed := base64.RawURLEncoding.EncodeToString(payload) + "." + strings.Split(token, ".")[1]
	if _, err := h.verify(changed); err == nil {
		t.Fatal("accepted changed execution identity")
	}
	// Identity is never included in the public status projection.
	public, _ := json.Marshal(publicSession(s))
	if strings.Contains(string(public), "execution") {
		t.Fatal("public session exposed execution identity")
	}
}

func TestExecutionHeaderRejectedForDirectAndAdministrativeRequests(t *testing.T) {
	h := New(nil, Options{Token: "secret"})
	for _, path := range []string{"/v1/direct/anything", "/v1/system", "/v1/roots"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer secret")
		r.Header.Set(ExecutionHeader, `{"uid":1001,"gid":100}`)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
}

func TestExecutionRootRoutesRejectUnsupportedScopeBeforeAccess(t *testing.T) {
	h := New([]*domain.Root{{Config: domain.RootConfig{Name: "files"}}}, Options{Token: "secret"})
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/v1/roots/files", 400},
		{"GET", "/v1/roots/files/index", 400},
		{"GET", "/v1/roots/files/stats", 400},
		{"GET", "/v1/roots/files/search?q=a", 400},
		{"POST", "/v1/roots/files/index/rebuild", 400},
		{"POST", "/v1/roots/files/stats/refresh", 400},
		{"POST", "/v1/roots/files/versions/prune", 400},
		{"GET", "/v1/roots/files/content?path=a", 409},
		{"POST", "/v1/roots/files/uploads/direct", 409},
		{"POST", "/v1/downloads/archives", 409},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"items":[{"root":"files","path":"a","archivePath":"a"}]}`))
		r.Header.Set("Authorization", "Bearer secret")
		r.Header.Set(ExecutionHeader, `{"uid":1001,"gid":100}`)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
}

func TestExecutionCapacityIsRetryable(t *testing.T) {
	w := httptest.NewRecorder()
	fail(w, domain.ErrExecutionCapacity)
	if w.Code != 503 || w.Header().Get("Retry-After") != "1" || !strings.Contains(w.Body.String(), `"error":"execution_capacity"`) {
		t.Fatalf("capacity response: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
}
