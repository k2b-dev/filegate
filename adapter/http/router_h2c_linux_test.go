//go:build linux

package httpadapter

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// h2cOnlyClient speaks cleartext HTTP/2 and nothing else, so a downgrade shows
// up as a failure instead of a passing test that proved nothing.
func h2cOnlyClient() *http.Client {
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Protocols: protocols}}
}

func h2cRouterServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	server.Config.Protocols = protocols
	server.Start()
	t.Cleanup(server.Close)
	return server
}

// The real routes work over h2c, not just a stub handler.
//
// The upload paths read Content-Length and wrap bodies in MaxBytesReader, and
// HTTP/2 does not require a client to send a length at all. A body that arrived
// truncated over h2c would land a corrupt file rather than fail a request, so
// this asserts the stored bytes.
func TestOneShotUploadOverH2C(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()
	server := h2cRouterServer(t, r)

	root := svc.ListRoot()[0]
	payload := bytes.Repeat([]byte("h2c-payload;"), 200_000) // ~2.4 MiB, several flow-control windows

	// No Content-Length: the body is an unknown-length reader, which is the
	// case HTTP/2 allows and HTTP/1.1 would have chunked.
	req, err := http.NewRequest(http.MethodPut, server.URL+"/v1/paths/"+root.Name+"/h2c/upload.bin", newUnsizedReader(payload))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-token")

	res, err := h2cOnlyClient().Do(req)
	if err != nil {
		t.Fatalf("h2c put: %v", err)
	}
	defer res.Body.Close()
	if res.ProtoMajor != 2 {
		t.Fatalf("request went over %s, so this test proves nothing about h2c", res.Proto)
	}
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("put status=%d body=%s", res.StatusCode, body)
	}

	id, err := svc.ResolvePath(root.Name + "/h2c/upload.bin")
	if err != nil {
		t.Fatalf("uploaded file does not resolve: %v", err)
	}
	meta, err := svc.GetFile(id)
	if err != nil {
		t.Fatalf("get file: %v", err)
	}
	if meta.Size != int64(len(payload)) {
		t.Fatalf("stored %d bytes, want %d", meta.Size, len(payload))
	}

	// Read it back over h2c too, so the download path is covered as well.
	getReq, err := http.NewRequest(http.MethodGet, server.URL+"/v1/nodes/"+id.String()+"/content", nil)
	if err != nil {
		t.Fatalf("new get request: %v", err)
	}
	getReq.Header.Set("Authorization", "Bearer test-token")
	getRes, err := h2cOnlyClient().Do(getReq)
	if err != nil {
		t.Fatalf("h2c get: %v", err)
	}
	defer getRes.Body.Close()
	got, err := io.ReadAll(getRes.Body)
	if err != nil {
		t.Fatalf("read downloaded body: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("downloaded %d bytes, want %d, and they differ", len(got), len(payload))
	}
}

// The upload size limit still applies over h2c.
//
// It cannot lean on Content-Length here, because a client need not send one, so
// this is the case where MaxBytesReader has to be what stops the request. A
// missing limit would let a client write past max_upload_bytes.
func TestOversizedUploadOverH2CIsRefused(t *testing.T) {
	r, svc, cleanup := newTestRouterWithCustomLimits(t, t.TempDir(), t.TempDir(), RouterOptions{
		BearerToken:           "test-token",
		JobWorkers:            2,
		JobQueueSize:          64,
		UploadExpiry:          time.Hour,
		UploadCleanupInterval: time.Hour,
		MaxChunkBytes:         1 << 20,
		MaxUploadBytes:        64 << 10,
	})
	defer cleanup()
	server := h2cRouterServer(t, r)

	root := svc.ListRoot()[0]
	oversized := bytes.Repeat([]byte("x"), 256<<10) // four times the limit

	req, err := http.NewRequest(http.MethodPut, server.URL+"/v1/paths/"+root.Name+"/h2c/too-big.bin", newUnsizedReader(oversized))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-token")

	res, err := h2cOnlyClient().Do(req)
	if err != nil {
		// A stream reset is an acceptable way to refuse an oversized body; what
		// matters is that nothing was stored.
		t.Logf("h2c put failed at the transport: %v", err)
	} else {
		defer res.Body.Close()
		if res.ProtoMajor != 2 {
			t.Fatalf("request went over %s, so this test proves nothing about h2c", res.Proto)
		}
		if res.StatusCode != http.StatusRequestEntityTooLarge {
			body, _ := io.ReadAll(res.Body)
			t.Errorf("status=%d body=%s, want 413", res.StatusCode, body)
		}
	}

	if _, err := svc.ResolvePath(root.Name + "/h2c/too-big.bin"); err == nil {
		t.Error("an oversized upload was stored")
	}
}

// newUnsizedReader hides the length from net/http so no Content-Length is sent.
func newUnsizedReader(payload []byte) io.Reader {
	return &unsizedReader{payload: payload}
}

type unsizedReader struct {
	payload []byte
	offset  int
}

func (r *unsizedReader) Read(p []byte) (int, error) {
	if r.offset >= len(r.payload) {
		return 0, io.EOF
	}
	n := copy(p, r.payload[r.offset:])
	r.offset += n
	return n, nil
}
