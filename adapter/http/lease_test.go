package httpadapter

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/k2b-dev/filegate/v5/domain"
)

func TestTransferLeaseLimitsAndOperations(t *testing.T) {
	h := New(nil, Options{Token: "test-secret"})
	for _, seconds := range []int{-1, 301, 86400} {
		if _, err := leaseExpiry(seconds); err == nil {
			t.Fatalf("accepted TTL %d", seconds)
		}
	}
	for _, seconds := range []int{0, 1, 60, 300} {
		before := time.Now().Unix()
		expiry, err := leaseExpiry(seconds)
		want := seconds
		if want == 0 {
			want = 60
		}
		if err != nil || expiry.Unix() < before+int64(want) || expiry.Unix() > time.Now().Unix()+int64(want) {
			t.Fatalf("TTL %d: %v %v", seconds, expiry, err)
		}
	}
	for _, tc := range []struct {
		purpose    string
		operations []string
		valid      bool
	}{
		{"upload", []string{"write"}, true},
		{"download", []string{"read"}, true},
		{"archive", []string{"read"}, true},
		{"session", []string{"status", "write", "abort"}, true},
		{"session", []string{"status"}, true},
		{"session", nil, false},
		{"session", []string{"commit"}, false},
		{"session", []string{"renew"}, false},
		{"session", []string{"status", "status"}, false},
		{"upload", []string{"read"}, false},
		{"download", []string{"write"}, false},
		{"unknown", []string{"read"}, false},
	} {
		c := capability{Purpose: tc.purpose, Operations: tc.operations, Expires: time.Now().Add(time.Minute).Unix()}
		if _, err := h.verify(h.sign(c)); (err == nil) != tc.valid {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
	c := capability{Purpose: "session", Operations: []string{"status"}, Expires: time.Now().Unix() - 1}
	if _, err := h.verify(h.sign(c)); err == nil {
		t.Fatal("accepted expired lease")
	}
	c.Expires = time.Now().Add(time.Minute).Unix()
	token := h.sign(c)
	if _, err := h.verify("x" + token); err == nil {
		t.Fatal("accepted tampered lease")
	}
}

func TestDirectOperationsRejectedBeforeUploadCapacityOrFilesystem(t *testing.T) {
	h := New(nil, Options{Token: "test-secret"})
	for i := 0; i < cap(h.uploadSlots); i++ {
		h.uploadSlots <- struct{}{}
	}
	for _, tc := range []struct {
		purpose, method string
		operations      []string
	}{
		{"session", "POST", []string{"status", "write", "abort"}},
		{"session", "PUT", []string{"status"}},
		{"session", "DELETE", []string{"status", "write"}},
		{"download", "PUT", []string{"read"}},
		{"upload", "GET", []string{"write"}},
	} {
		c := capability{Root: "missing", Purpose: tc.purpose, Operations: tc.operations, Expires: time.Now().Add(time.Minute).Unix()}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tc.method, "/v1/direct/"+h.sign(c), nil))
		if w.Code != 403 {
			t.Fatalf("%s/%s: %d %s", tc.purpose, tc.method, w.Code, w.Body.String())
		}
	}
}

func TestSessionLeaseExpiryAndPublicProjection(t *testing.T) {
	h := New(nil, Options{Token: "test-secret"})
	s := domain.Session{ID: "session", Root: "root", Path: "private/path", Size: 12, ChunkSize: 8, Expires: time.Now().Add(5 * time.Second), State: domain.SessionOpen, Options: domain.WriteOptions{Metadata: domain.Metadata{"secret": "sensitive"}}, Result: &domain.Node{Path: "private/path"}}
	lease, err := h.sessionLease(s, 60, false)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Expires.After(s.Expires) || len(lease.Operations) != 2 {
		t.Fatal(lease)
	}
	body, err := json.Marshal(publicSession(s))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"private/path", "secret", "options", "result", "metadata"} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("browser status leaked %s: %s", forbidden, body)
		}
	}
	lease, err = h.sessionLease(s, 60, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(lease.Operations) != 3 || lease.Operations[2] != "abort" {
		t.Fatal(lease)
	}
}

func TestSessionLeaseRejectsElapsedSessionDeadline(t *testing.T) {
	h := New(nil, Options{Token: "test-secret"})
	s := domain.Session{ID: "session", Root: "root", State: domain.SessionOpen, Expires: time.Now().Add(-time.Second)}
	if _, err := h.sessionLease(s, 60, false); err != domain.ErrSessionExpired {
		t.Fatalf("expired session minted a lease: %v", err)
	}
}
