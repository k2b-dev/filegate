package httpadapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/k2b-dev/filegate/v5/domain"
)

func TestArchiveNames(t *testing.T) {
	for _, name := range []string{"", ".", "..", "../a", "/a", "a//b", "a/./b", `a\b`, "C:/a", "a\nb", "a.", "a ", "CON.txt", "a/LPT1", "a?b", "bad\xff"} {
		if validArchiveName(name) {
			t.Errorf("accepted unsafe name %q", name)
		}
	}
	for _, name := range []string{"a", "Projects/Notizen.txt", "Überblick/2026.csv"} {
		if !validArchiveName(name) {
			t.Errorf("rejected name %q", name)
		}
	}
}

func TestArchiveSelectionCollisionsAndBounds(t *testing.T) {
	for _, pair := range [][2]string{{"a", "A"}, {"a", "a/b"}, {"a/b", "a"}, {"Ä", "A\u0308"}, {"Straße", "STRASSE"}} {
		items := []archiveSelection{{Root: "x", Path: ".", ArchivePath: pair[0]}, {Root: "y", Path: "a", ArchivePath: pair[1]}}
		if !errors.Is(validateArchiveSelection(items), domain.ErrConflict) {
			t.Errorf("accepted collision %v", pair)
		}
	}
	if !errors.Is(validateArchiveSelection(make([]archiveSelection, archiveItemLimit+1)), domain.ErrLimit) {
		t.Fatal("unbounded selection")
	}
	if !errors.Is(validateArchiveSelection(nil), domain.ErrInvalid) {
		t.Fatal("accepted empty selection")
	}
}

func TestArchiveRootSelectionMustBeExplicit(t *testing.T) {
	h := New(nil, Options{})
	for _, body := range []string{`{"items":[{"root":"test","archivePath":"root"}]}`, `{"items":[{"root":"test","path":"","archivePath":"root"}]}`} {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		if e := h.mintArchive(httptest.NewRecorder(), r); !errors.Is(e, domain.ErrInvalid) {
			t.Fatal("empty path accepted", e)
		}
	}
}

type neverArchiveReader struct{ t *testing.T }

func (r neverArchiveReader) Read([]byte) (int, error) {
	r.t.Fatal("read after cancellation")
	return 0, io.EOF
}
func TestArchiveCopyCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := archiveReader{ctx: ctx, r: neverArchiveReader{t}}
	if _, e := r.Read(make([]byte, 1)); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}

func TestArchiveStreamCapacityAndFormContract(t *testing.T) {
	manifest := `[{"root":"missing","path":"a","archivePath":"a","directory":false}]`
	hash := sha256.Sum256([]byte(manifest))
	c := capability{Purpose: "archive", Operations: []string{"read"}, ManifestHash: hex.EncodeToString(hash[:])}
	h := New(nil, Options{})
	request := func(body string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return r
	}
	for i := 0; i < cap(archiveSlots); i++ {
		archiveSlots <- struct{}{}
	}
	e := h.directArchive(httptest.NewRecorder(), request(url.Values{"manifest": {manifest}}.Encode()), c)
	for i := 0; i < cap(archiveSlots); i++ {
		<-archiveSlots
	}
	var he errHTTP
	if !errors.As(e, &he) || he.status != 503 {
		t.Fatal("stream budget ignored", e)
	}
	for _, values := range []url.Values{{"manifest": {manifest, manifest}}, {"manifest": {manifest}, "extra": {"x"}}} {
		if e := h.directArchive(httptest.NewRecorder(), request(values.Encode()), c); !errors.Is(e, domain.ErrInvalid) {
			t.Fatal("accepted ambiguous form", e)
		}
	}
	r := request(url.Values{"manifest": {manifest}}.Encode())
	r.Header.Set("Content-Type", "multipart/form-data")
	if e := h.directArchive(httptest.NewRecorder(), r, c); !errors.As(e, &he) || he.status != 415 {
		t.Fatal("accepted multipart", e)
	}
}
