//go:build linux

package integration_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/k2b-dev/filegate/v6/domain"
)

func TestListingOrdersBeforePaginationAndKeepsFoldersScoped(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		t.Run(fmt.Sprint(indexed), func(t *testing.T) {
			x := setup(t, indexed, false)
			for _, file := range []struct{ path, body string }{{"folder/z", "x"}, {"folder/a", "xxx"}, {"folder/m", "xx"}, {"folder/sub/deep", "xxxx"}, {"outside", ""}} {
				put(t, x.r, file.path, file.body, domain.WriteOptions{})
			}
			opts := domain.ListingOptions{Sort: "size", Order: "desc", Type: "files", Limit: 1}
			paths := []string{}
			for {
				page, err := x.r.List(ctx, "folder", opts)
				if err != nil {
					t.Fatal(err)
				}
				for _, n := range page.Items {
					paths = append(paths, n.Path)
				}
				if page.Next == "" {
					break
				}
				opts.After = page.Next
			}
			if strings.Join(paths, ",") != "folder/a,folder/m,folder/z" {
				t.Fatal(paths)
			}
			dirs, err := x.r.List(ctx, "folder", domain.ListingOptions{Type: "directories"})
			if err != nil || len(dirs.Items) != 1 || dirs.Items[0].Path != "folder/sub" {
				t.Fatalf("directories %+v %v", dirs, err)
			}
			stats, err := x.r.RecursiveStats(ctx, "folder", 100)
			if err != nil || !stats.Complete || stats.Files != 4 || stats.Directories != 2 || stats.Bytes != 10 {
				t.Fatalf("stats %+v %v", stats, err)
			}
		})
	}
}

func TestListingCursorInvalidatedByMutationAndRebuild(t *testing.T) {
	x := setup(t, true, false)
	put(t, x.r, "a", "a", domain.WriteOptions{})
	put(t, x.r, "b", "b", domain.WriteOptions{})
	page, err := x.r.List(ctx, ".", domain.ListingOptions{Limit: 1})
	if err != nil || page.Next == "" {
		t.Fatal(page, err)
	}
	put(t, x.r, "c", "c", domain.WriteOptions{})
	if _, err := x.r.List(ctx, ".", domain.ListingOptions{Limit: 1, After: page.Next}); !errors.Is(err, domain.ErrCursorInvalid) {
		t.Fatalf("mutation cursor accepted %v", err)
	}
	page, err = x.r.Search(ctx, "", ".", domain.ListingOptions{Limit: 1, Sort: "size"})
	if err != nil {
		t.Fatal(err)
	}
	if err := x.r.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := x.r.Search(ctx, "", ".", domain.ListingOptions{Limit: 1, Sort: "size", After: page.Next}); !errors.Is(err, domain.ErrCursorInvalid) {
		t.Fatalf("rebuilt cursor accepted %v", err)
	}
}

func TestIndexSortingSurvivesOverwriteAndDerivedFormatRebuild(t *testing.T) {
	x := setup(t, true, false)
	put(t, x.r, "a", "longer", domain.WriteOptions{})
	put(t, x.r, "b", "xx", domain.WriteOptions{})
	put(t, x.r, "a", "x", domain.WriteOptions{OnConflict: "overwrite"})
	page, err := x.r.Search(ctx, "", ".", domain.ListingOptions{Sort: "size"})
	if err != nil || len(page.Items) != 2 || page.Items[0].Path != "a" || page.Items[0].Size != 1 {
		t.Fatal(page, err)
	}
	// Removing only the derived format marker simulates an older index layout.
	// Opening the root rebuilds derived rows without touching file identities.
	id := page.Items[0].ID
	if err := x.r.State.Delete("index/format"); err != nil {
		t.Fatal(err)
	}
	reopen(t, x)
	page, err = x.r.Search(ctx, "", ".", domain.ListingOptions{Sort: "size"})
	if err != nil || len(page.Items) != 2 || page.Items[0].ID != id {
		t.Fatal(page, err)
	}
}
