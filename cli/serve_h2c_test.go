package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// h2cClient speaks cleartext HTTP/2 and nothing else, so a server that only
// offers HTTP/1.1 fails rather than quietly downgrading and passing the test.
func h2cClient() *http.Client {
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{Protocols: protocols},
	}
}

func h1Client() *http.Client {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{Protocols: protocols},
	}
}

func echoProtoServer(t *testing.T, h2cEnabled bool) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Echo-Len", string(rune('0'+len(body)%10)))
		_, _ = io.WriteString(w, r.Proto)
	}))
	// Mirrors the REST listener: the same timeouts, and Protocols decided by the
	// same function the daemon uses.
	server.Config.ReadHeaderTimeout = 10 * time.Second
	server.Config.ReadTimeout = 30 * time.Second
	server.Config.WriteTimeout = 5 * time.Minute
	server.Config.IdleTimeout = 120 * time.Second
	server.Config.Protocols = restListenerProtocols(h2cEnabled)
	server.Start()
	t.Cleanup(server.Close)
	return server
}

// With h2c on, an HTTP/2 client gets HTTP/2 on the plain listener.
//
// The benchmark could only compare protocols through a TLS-terminating proxy
// because the listener had no h2c, which left an operator whose proxy speaks h2
// to its backends with no way to connect.
func TestRESTListenerServesH2CWhenEnabled(t *testing.T) {
	server := echoProtoServer(t, true)

	res, err := h2cClient().Get(server.URL + "/")
	if err != nil {
		t.Fatalf("h2c request: %v", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if res.ProtoMajor != 2 {
		t.Errorf("client saw %s, want HTTP/2", res.Proto)
	}
	if got := string(body); got != "HTTP/2.0" {
		t.Errorf("server saw %q, want HTTP/2.0", got)
	}
}

// HTTP/1.1 keeps working on the same port. This is what makes the option safe to
// turn on: it adds a protocol rather than swapping one.
func TestRESTListenerStillServesHTTP1WhenH2CIsEnabled(t *testing.T) {
	server := echoProtoServer(t, true)

	res, err := h1Client().Get(server.URL + "/")
	if err != nil {
		t.Fatalf("http/1.1 request: %v", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.ProtoMajor != 1 {
		t.Errorf("client saw %s, want HTTP/1.1", res.Proto)
	}
	if got := string(body); got != "HTTP/1.1" {
		t.Errorf("server saw %q, want HTTP/1.1", got)
	}
}

// Off by default, and off means refused rather than silently downgraded. An
// operator who did not ask for a second protocol does not get one.
func TestRESTListenerRefusesH2CWhenDisabled(t *testing.T) {
	if got := restListenerProtocols(false); got != nil {
		t.Fatalf("protocols = %v with h2c off, want nil so net/http keeps its default", got)
	}

	server := echoProtoServer(t, false)

	res, err := h2cClient().Get(server.URL + "/")
	if err == nil {
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("h2c succeeded against a listener with h2c off: proto=%s body=%q", res.Proto, body)
	}

	// The same listener still answers HTTP/1.1, so the failure above is about
	// the protocol and not about the server being broken.
	res1, err := h1Client().Get(server.URL + "/")
	if err != nil {
		t.Fatalf("http/1.1 request against h2c-disabled listener: %v", err)
	}
	defer res1.Body.Close()
	if res1.ProtoMajor != 1 {
		t.Errorf("client saw %s, want HTTP/1.1", res1.Proto)
	}
}

// Request bodies survive the switch.
//
// The upload paths stream bodies and enforce size limits from Content-Length,
// which HTTP/2 does not require a client to send. A body that arrives truncated
// or empty over h2c would corrupt uploads rather than fail them, so this checks
// the bytes rather than the status.
func TestH2CDeliversRequestBodiesIntact(t *testing.T) {
	var seen []byte
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		seen = body
		_, _ = io.WriteString(w, r.Proto)
	}))
	server.Config.Protocols = restListenerProtocols(true)
	server.Start()
	defer server.Close()

	payload := make([]byte, 3<<20) // larger than one HTTP/2 flow-control window
	for i := range payload {
		payload[i] = byte(i % 251)
	}

	req, err := http.NewRequest(http.MethodPut, server.URL+"/v1/paths/data/x.bin", newRepeatReader(payload))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	res, err := h2cClient().Do(req)
	if err != nil {
		t.Fatalf("h2c put: %v", err)
	}
	defer res.Body.Close()
	proto, _ := io.ReadAll(res.Body)

	if string(proto) != "HTTP/2.0" {
		t.Fatalf("server saw %q, want HTTP/2.0", proto)
	}
	if len(seen) != len(payload) {
		t.Fatalf("server read %d bytes, want %d", len(seen), len(payload))
	}
	for i := range payload {
		if seen[i] != payload[i] {
			t.Fatalf("body differs at offset %d", i)
		}
	}
}

// newRepeatReader hands the bytes over without a Content-Length, which is the
// case HTTP/2 makes possible and HTTP/1.1 would have chunked.
func newRepeatReader(payload []byte) io.Reader {
	return &repeatReader{payload: payload}
}

type repeatReader struct {
	payload []byte
	offset  int
}

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.offset >= len(r.payload) {
		return 0, io.EOF
	}
	n := copy(p, r.payload[r.offset:])
	r.offset += n
	return n, nil
}
