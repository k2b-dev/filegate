package domain

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

type slowListingFiles struct {
	*listingFiles
	advance func()
}

func (f *slowListingFiles) Open(p string, flags int, mode os.FileMode) (*os.File, error) {
	if p != "." && f.advance != nil {
		f.advance()
		f.advance = nil
	}
	return f.listingFiles.Open(p, flags, mode)
}

func TestLiveListingLifetimeStartsAfterSlowScan(t *testing.T) {
	root, files, now := liveListingRoot(t)
	root.Files = &slowListingFiles{listingFiles: files, advance: func() {
		*now = now.Add(2 * time.Minute)
	}}
	opts := ListingOptions{Limit: 1}
	first, err := root.List(context.Background(), ".", opts)
	if err != nil || first.Next == "" {
		t.Fatalf("first page: %+v %v", first, err)
	}
	opts.After = first.Next
	if _, err := root.List(context.Background(), ".", opts); err != nil {
		t.Fatalf("slow scan consumed cursor lifetime: %v", err)
	}
	*now = now.Add(listingLifetime)
	if _, err := root.List(context.Background(), ".", opts); !errors.Is(err, ErrCursorInvalid) {
		t.Fatalf("cursor did not expire after its usable lifetime: %v", err)
	}
}
