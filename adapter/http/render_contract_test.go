package httpadapter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"mime"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/k2b-dev/filegate/v6/domain"
)

func TestDownloadDispositionRoundTripAndInjection(t *testing.T) {
	for _, name := range []string{"notes.txt", "Überblick 2026.pdf", `quote"and;name.txt`, "100% ready.txt"} {
		w := httptest.NewRecorder()
		if err := setDownloadDisposition(w, name); err != nil {
			t.Fatal(err)
		}
		value := w.Header().Get("Content-Disposition")
		kind, params, err := mime.ParseMediaType(value)
		if err != nil || kind != "attachment" || params["filename"] != name || !strings.Contains(value, "filename*=UTF-8''") {
			t.Fatalf("%q: %q %#v %v", name, value, params, err)
		}
		for _, b := range []byte(value) {
			if b < 32 || b > 126 {
				t.Fatalf("non-ASCII or control header: %q", value)
			}
		}
	}
	for _, name := range []string{"", ".", "..", "a/b", `a\b`, "a\r\nX-Evil: yes", "a\x00b", "bad\xff", strings.Repeat("x", 256)} {
		if ValidateDownloadName(name) == nil {
			t.Errorf("accepted unsafe explicit name %q", name)
		}
	}
	for _, name := range []string{"dir/a\nb", `dir/a\b`, "dir/bad\xff", ".", strings.Repeat("ä", 200)} {
		safe := defaultDownloadName(name)
		if !utf8.ValidString(safe) || ValidateDownloadName(safe) != nil {
			t.Errorf("unsafe default %q -> %q", name, safe)
		}
	}
}

func TestArchiveSortedAncestorAndSourceAliases(t *testing.T) {
	items := []archiveSelection{{Root: "x", Path: "same", ArchivePath: "a"}, {Root: "x", Path: "same", ArchivePath: "a-"}, {Root: "y", Path: "other", ArchivePath: "a/child"}}
	if !errors.Is(validateArchiveSelection(items), domain.ErrConflict) {
		t.Fatal("missed nonadjacent lexical ancestor")
	}
	items = items[:2]
	if err := validateArchiveSelection(items); err != nil {
		t.Fatal("source aliases or component siblings rejected", err)
	}
}

func BenchmarkArchiveCollisionSort(b *testing.B) {
	for _, count := range []int{1000, 10000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			source := make([]string, count)
			for i := range source {
				source[i] = fmt.Sprintf("file-%08d/", count-i)
			}
			b.ReportAllocs()
			for b.Loop() {
				keys := append([]string(nil), source...)
				if overlappingArchiveNames(keys) {
					b.Fatal("unexpected collision")
				}
			}
		})
	}
}

func coordinatorForTest() *thumbnailCoordinator {
	return &thumbnailCoordinator{flights: map[thumbnailKey]*thumbnailFlight{}, slots: make(chan struct{}, 4), waiters: make(chan struct{}, 32)}
}
func TestThumbnailDedupeCancellationAndBounds(t *testing.T) {
	c := coordinatorForTest()
	key := thumbnailKey{inode: 1}
	first, leader, err := c.acquire(key)
	if err != nil || !leader {
		t.Fatal(err, leader)
	}
	for i := 0; i < 32; i++ {
		joined, isLeader, err := c.acquire(key)
		if err != nil || isLeader || joined != first {
			t.Fatal("duplicate render", err, isLeader)
		}
	}
	if _, _, err := c.acquire(key); err == nil {
		t.Fatal("unbounded waiters")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.wait(cancelled, first, true); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if first.ctx.Err() != nil {
		t.Fatal("leader cancellation cancelled followers")
	}
	for i := 0; i < 31; i++ {
		c.wait(cancelled, first, false)
	}
	if first.ctx.Err() != nil {
		t.Fatal("cancelled final active follower")
	}
	c.complete(key, first, []byte("jpeg"), nil)
	data, err := c.wait(context.Background(), first, false)
	if err != nil || string(data) != "jpeg" || len(c.slots) != 0 || len(c.waiters) != 0 || len(c.flights) != 0 {
		t.Fatal("completion leaked resource", err)
	}
	var active []*thumbnailFlight
	for i := 0; i < 4; i++ {
		f, _, err := c.acquire(thumbnailKey{inode: uint64(i)})
		if err != nil {
			t.Fatal(err)
		}
		active = append(active, f)
	}
	if _, _, err := c.acquire(thumbnailKey{inode: 10}); err == nil {
		t.Fatal("unbounded rendering")
	}
	for i, f := range active {
		c.wait(cancelled, f, true)
		if f.ctx.Err() == nil {
			t.Fatal("last cancellation ignored")
		}
		c.complete(thumbnailKey{inode: uint64(i)}, f, nil, context.Canceled)
	}
}

func TestThumbnailRetryAndOutputLimit(t *testing.T) {
	w := httptest.NewRecorder()
	err := thumbnailCapacity(w)
	var httpErr errHTTP
	if !errors.As(err, &httpErr) || httpErr.status != 503 || w.Header().Get("Retry-After") != "1" {
		t.Fatal(err, w.Header())
	}
	var buffer thumbnailBuffer
	if _, err = buffer.Write(make([]byte, thumbnailOutputLimit)); err != nil {
		t.Fatal(err)
	}
	if _, err = buffer.Write([]byte{1}); !errors.Is(err, domain.ErrLimit) || buffer.Len() != thumbnailOutputLimit {
		t.Fatal("unbounded result", err)
	}
}

func TestThumbnailConcurrentDuplicateUsesOneRender(t *testing.T) {
	c := coordinatorForTest()
	key := thumbnailKey{inode: 42, bounds: thumbnailSize{Width: 128, Height: 128}}
	started := make(chan struct{})
	acquired := make(chan struct{}, 24)
	finish := make(chan struct{})
	results := make(chan error, 24)
	var renders atomic.Int32
	for i := 0; i < 24; i++ {
		go func() {
			<-started
			f, leader, err := c.acquire(key)
			if err != nil {
				results <- err
				acquired <- struct{}{}
				return
			}
			if leader {
				renders.Add(1)
				go func() { <-finish; c.complete(key, f, []byte("jpeg"), nil) }()
			}
			acquired <- struct{}{}
			data, err := c.wait(context.Background(), f, leader)
			if err == nil && string(data) != "jpeg" {
				err = fmt.Errorf("wrong result")
			}
			results <- err
		}()
	}
	close(started)
	for i := 0; i < 24; i++ {
		<-acquired
	}
	if renders.Load() != 1 {
		t.Fatalf("rendered %d times", renders.Load())
	}
	close(finish)
	for i := 0; i < 24; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}

func TestThumbnailRevalidatesFrozenInputAndRendersJPEG(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "source")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err = png.Encode(f, image.NewRGBA(image.Rect(0, 0, 16, 8))); err != nil {
		t.Fatal(err)
	}
	if err = validateThumbnailSourceAfterRewind(f); err != nil {
		t.Fatal(err)
	}
	data, err := renderThumbnail(context.Background(), f, thumbnailSize{Width: 8, Height: 8})
	if err != nil {
		t.Fatal(err)
	}
	rendered, format, err := image.Decode(bytes.NewReader(data))
	if err != nil || format != "jpeg" || rendered.Bounds().Dx() != 8 || rendered.Bounds().Dy() != 4 {
		t.Fatal(format, err)
	}
	// Replace a source after its preflight validation with a GIF header claiming
	// 65535*65535 pixels. Rendering must re-check its own frozen input, not trust
	// the earlier configuration read.
	if err = f.Truncate(0); err != nil {
		t.Fatal(err)
	}
	f.Seek(0, 0)
	if _, err = f.Write([]byte{'G', 'I', 'F', '8', '9', 'a', 255, 255, 255, 255, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	f.Seek(0, 0)
	if _, err = renderThumbnail(context.Background(), f, thumbnailSize{Width: 8, Height: 8}); !errors.Is(err, domain.ErrLimit) {
		t.Fatal("changed dimensions bypassed limit", err)
	}
}

func validateThumbnailSourceAfterRewind(f *os.File) error {
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	return validateThumbnailSource(f)
}
