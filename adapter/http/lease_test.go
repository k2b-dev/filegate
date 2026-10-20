package httpadapter

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/k2b-dev/filegate/v7/domain"
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
		{"version", []string{"read"}, true},
		{"thumbnail", []string{"read"}, true},
		{"version", []string{"write"}, false},
		{"thumbnail", []string{"status"}, false},
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
		{"version", "PUT", []string{"read"}},
		{"thumbnail", "POST", []string{"read"}},
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

func TestDerivedDownloadLeaseExpiry(t *testing.T) {
	h := New(nil, Options{Token: "test-secret"})
	for _, purpose := range []string{"version", "thumbnail"} {
		c := capability{Root: "missing", Path: "file", Purpose: purpose, Version: "version", Thumbnail: &thumbnailSize{Width: 128, Height: 64}, Operations: []string{"read"}, Expires: time.Now().Unix()}
		for _, method := range []string{"GET", "HEAD"} {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(method, "/v1/direct/"+h.sign(c), nil))
			if w.Code != 401 || !strings.Contains(w.Body.String(), "expired_capability") {
				t.Fatalf("%s %s: %d %s", purpose, method, w.Code, w.Body.String())
			}
		}
	}
}

func TestRawPathCapabilityRoundTrip(t *testing.T) {
	h := New(nil, Options{Token: "secret"})
	for _, p := range []string{"\xff.pdf", "mid-\xfe.pdf", "�.pdf", "mid-�.pdf"} {
		// Fixed far-future expiry: the fixture never depends on today's date.
		original := capability{Root: "files", Path: p, Purpose: "download", Operations: []string{"read"}, Expires: 253402300799}
		got, err := h.verify(h.sign(original))
		if err != nil || got.Path != p {
			t.Errorf("capability lost %q: %q %v", p, got.Path, err)
		}
	}
}

func TestRawPathCapabilityOmitsSanitizedPath(t *testing.T) {
	h := New(nil, Options{Token: "secret"})
	// Fourteen legal Linux components: 3500 raw bytes plus 13 separators.
	c := capability{Root: "files", Path: strings.Repeat(strings.Repeat("\xff", 250)+"/", 13) + strings.Repeat("\xff", 250), Purpose: "download", Operations: []string{"read"}, Expires: 253402300799}
	token := h.sign(c)
	if got, err := h.verify(token); err != nil || got.Path != c.Path {
		t.Fatalf("issued unusable token (%d bytes): %v", len(token), err)
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[0])
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["path"]; ok {
		t.Fatal("raw path duplicated as sanitized path")
	}
}

func TestCapabilityIssuanceBoundaryAndLegacyToken(t *testing.T) {
	h := New(nil, Options{Token: "secret"})
	c := capability{Root: "files", Path: "legacy/�", Purpose: "download", Operations: []string{"read"}, Expires: 253402300799}
	// Pre-upgrade tokens had only path. Sign those exact bytes without the
	// current issuer so backward compatibility is checked independently.
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	payload := base64.RawURLEncoding.EncodeToString(b)
	mac := hmac.New(sha256.New, []byte("secret"))
	mac.Write([]byte(payload))
	legacy := payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if got, err := h.verify(legacy); err != nil || got.Path != c.Path {
		t.Fatalf("legacy token: %+v %v", got, err)
	}
	for _, value := range []string{"x", "\xff"} {
		// Locate the last issuable path length, with a fixed capability fixture.
		low, high := 0, 20000
		for low < high {
			mid := (low + high + 1) / 2
			c.Path = strings.Repeat(value, mid)
			if len(h.sign(c)) <= capabilityTokenLimit {
				low = mid
			} else {
				high = mid - 1
			}
		}
		c.Path = strings.Repeat(value, low)
		target, err := h.url(c)
		if err != nil {
			t.Fatal(err)
		}
		token := strings.TrimPrefix(target, "/v1/direct/")
		if len(token) > capabilityTokenLimit || len(token) < capabilityTokenLimit-3 {
			t.Fatalf("not at token boundary: %d", len(token))
		}
		if got, err := h.verify(token); err != nil || got.Path != c.Path {
			t.Fatalf("boundary token: %v", err)
		}
		c.Path += value
		if _, err := h.url(c); !errors.Is(err, domain.ErrLimit) {
			t.Fatalf("issued token beyond verifier bound: %v", err)
		}
	}
	// Exercise the HTTP envelope too; an upload target need not exist yet.
	root := &domain.Root{Config: domain.RootConfig{Name: "files"}, MaxBytes: 1 << 20}
	h = New([]*domain.Root{root}, Options{Token: "secret"})
	req := httptest.NewRequest("POST", "/v1/roots/files/uploads/direct", strings.NewReader(`{"path":"`+strings.Repeat("x", 20000)+`","size":1}`))
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), `"error":"limit_exceeded"`) {
		t.Fatalf("issuance overflow: %d %s", w.Code, w.Body.String())
	}
}
