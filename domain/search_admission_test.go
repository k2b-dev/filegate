package domain

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

// Observing Done proves admission is evaluating its blocking select.
type waitingSearchContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *waitingSearchContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestSearchAdmissionCancellationAndViews(t *testing.T) {
	r := &Root{rootShared: &rootShared{}}
	view := &Root{rootShared: r.rootShared, execution: &ExecutionIdentity{UID: 1001, GID: 100}}
	if err := r.admitSearch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := view.admitSearch(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { <-r.searches; <-r.searches }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := make(chan struct{})
	done := make(chan error, 1)
	waitingCtx := &waitingSearchContext{Context: ctx, waiting: waiting}
	// Keep the production 200 ms timeout out of the cancellation assertion.
	go func() { done <- view.admitSearchWithin(waitingCtx, time.Hour) }()
	<-waiting
	cancel()
	waitingCtx.Done()
	waitingCtx.Done()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("admission ignored cancellation: %v", err)
	}
	if len(r.searches) != 2 {
		t.Fatalf("cancelled waiter changed active slots: %d", len(r.searches))
	}
	<-r.searches
	if err := view.admitSearch(context.Background()); err != nil {
		t.Fatalf("released slot was not reusable by an execution view: %v", err)
	}
}

type cancellingListingFiles struct {
	*listingFiles
	cancel context.CancelFunc
	nodes  int
}

func (f *cancellingListingFiles) Open(p string, flags int, mode os.FileMode) (*os.File, error) {
	if p != "." {
		f.nodes++
		f.cancel()
	}
	return f.listingFiles.Open(p, flags, mode)
}

func TestSearchLiveScanCancellation(t *testing.T) {
	r, files, _ := liveListingRoot(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &cancellingListingFiles{listingFiles: files, cancel: cancel}
	r.Files = f
	page, err := r.Search(ctx, "", ".", ListingOptions{})
	if !errors.Is(err, context.Canceled) || f.nodes != 1 || len(page.Items) != 0 || page.Next != "" || len(r.listings) != 0 || len(r.searches) != 0 {
		t.Fatalf("live scan ignored cancellation: %+v %v visits=%d", page, err, f.nodes)
	}
	// A fresh context can reuse the slot, without requiring xattrs or state.
	r.Files = files
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := r.Search(ctx, "", ".", ListingOptions{}); err != nil {
		t.Fatal(err)
	}
}
