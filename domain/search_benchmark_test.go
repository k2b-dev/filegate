//go:build linux

package domain_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"runtime/pprof"
	"testing"
	"time"

	"github.com/k2b-dev/filegate/v7/domain"
	"github.com/k2b-dev/filegate/v7/infra/filesystem"
	"github.com/k2b-dev/filegate/v7/infra/pebble"
)

// BenchmarkIndexedSearch measures complete first-page requests, including scoring,
// sorting, highlights and node materialization. All queries reuse one on-disk
// fixture per size; no million-file filesystem tree or in-memory index is needed.
// entries/s divides root size by request duration (early-stop queries need not
// scan every row). The phase=search CPU label excludes fixture construction.
// The broad fuzzy-name case at 1M is explicitly a 413 diagnostic; its size-
// bounded variant measures a full first page within production retention budgets.
// Run the large fixture explicitly with -benchtime=1x -count=1.
func BenchmarkIndexedSearch(b *testing.B) {
	for _, size := range []int{10000, 100000, 1000000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			r := indexedSearchFixture(b, size)
			for _, tc := range indexedSearchBenchmarkCases(size) {
				b.Run(tc.name, func(b *testing.B) {
					b.ReportAllocs()
					pprof.Do(context.Background(), pprof.Labels("phase", "search"), func(ctx context.Context) {
						for i := 0; i < b.N; i++ {
							page, err := r.Search(ctx, tc.query, ".", tc.opts)
							if tc.wantLimit {
								if !errors.Is(err, domain.ErrLimit) {
									b.Fatalf("413 diagnostic: got %v, want limit_exceeded", err)
								}
								b.ReportMetric(1, "limited/op")
								continue
							}
							if err != nil {
								b.Fatalf("after %s: %v", b.Elapsed(), err)
							}
							if len(page.Items) != 100 {
								b.Fatalf("first page has %d items, want 100", len(page.Items))
							}
						}
					})
					b.ReportMetric(float64(size)*float64(b.N)/b.Elapsed().Seconds(), "entries/s")
				})
			}
		})
	}
}

// Size grows monotonically with the fixture index at every measured scale.
// Keeping its first eighth bounds selective hits even though the fuzzy year
// token itself matches every generated year from 2020 through 2026.
func indexedSearchBenchmarkCases(size int) []struct {
	name, query string
	opts        domain.ListingOptions
	wantLimit   bool
} {
	maxSize := int64(1024 + size/8 - 1)
	broadName := "fuzzy-name"
	if size == 1000000 {
		broadName += "-413-diagnostic"
	}
	return []struct {
		name, query string
		opts        domain.ListingOptions
		wantLimit   bool
	}{
		{"exact-name", "quartalsbericht", domain.ListingOptions{Sort: "name"}, false},
		{broadName, "quatralsberihct", domain.ListingOptions{Match: "fuzzy", Sort: "name"}, size == 1000000},
		{"fuzzy-name-selective", "quatralsberihct 2026", domain.ListingOptions{Match: "fuzzy", Sort: "name", MaxSize: &maxSize}, false},
		{"fuzzy-relevance", "quatralsberihct", domain.ListingOptions{Match: "fuzzy", Sort: "relevance"}, false},
		{"path-relevance", "archive quatralsberihct", domain.ListingOptions{Match: "fuzzy", Sort: "relevance", In: "path"}, false},
		{"filter-pdf", "", domain.ListingOptions{Ext: []string{"pdf"}}, false},
	}
}

func TestIndexedSearchBenchmarkSelectivity(t *testing.T) {
	const size = 10000
	r := indexedSearchFixture(t, size)
	for _, tc := range indexedSearchBenchmarkCases(size) {
		if tc.name != "fuzzy-name-selective" {
			continue
		}
		// At 1M this density projects to at most half of the 100k hit budget.
		// Exercise the actual workload and matcher, including the fuzzy year.
		tc.opts.MaxEntries = 500
		tc.opts.Limit = 1000
		page, err := r.Search(context.Background(), tc.query, ".", tc.opts)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) < 100 || len(page.Items) > 500 || page.Next != "" || page.Total == nil || *page.Total != len(page.Items) {
			t.Fatalf("selective fixture must have 100–500 hits in one page: %+v", page)
		}
		t.Logf("selective hits: %d at 10k (projected %d at 1M)", len(page.Items), len(page.Items)*100)
		return
	}
	t.Fatal("selective benchmark case missing")
}

func indexedSearchFixture(b testing.TB, count int) *domain.Root {
	b.Helper()
	dir := b.TempDir()
	files, err := filesystem.Open(dir)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := files.Close(); err != nil {
			b.Error(err)
		}
	})
	state, err := pebble.Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := state.Close(); err != nil {
			b.Error(err)
		}
	})
	r, err := domain.NewRoot(domain.RootConfig{Name: "bench", Path: dir, Index: true}, files, state, 1<<20)
	if err != nil {
		b.Fatal(err)
	}
	var gen string
	if err := state.Get("index/generation", &gen); err != nil {
		b.Fatal(err)
	}
	words := []string{"Invoice", "Rechnung", "Meeting-notes", "Lieferschein", "Project_plan", "Urlaub", "Inventory", "Bestellung", "Résumé", "Müller", "Straße", "Überblick", "Budget", "Contract", "Photo", "Quartalsbericht"}
	extensions := []string{"pdf", "docx", "xlsx", "jpg", "txt", "csv", "PDF", "png"}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	changes := make([]domain.Change, 0, 7000)
	for i := 0; i < count; i++ {
		// 10% root, 25% one parent, 45% two, 15% three, 5% five.
		depth := 0
		switch n := i % 100; {
		case n < 10:
			depth = 0
		case n < 35:
			depth = 1
		case n < 80:
			depth = 2
		case n < 95:
			depth = 3
		default:
			depth = 5
		}
		parent := ""
		folders := []string{"archive", "Kunden", "projects", "2026", "Entwürfe"}
		for d := 0; d < depth; d++ {
			parent = path.Join(parent, fmt.Sprintf("%s-%02d", folders[d], (i/100+d)%32))
		}
		name := fmt.Sprintf("%s_%s_%04d-%02d-%02d_%07d.%s", words[i%len(words)], words[(i/len(words)+3)%len(words)], 2020+i%7, 1+i%12, 1+i%28, i, extensions[(i/7)%len(extensions)])
		n := domain.Node{Root: "bench", Path: path.Join(parent, name), Size: int64(1024 + i%10000000), Modified: now.Add(time.Duration(i%100000) * time.Second), Mode: "-rw-r--r--", UID: 1000, GID: 1000}
		// Populate all derived index families, using the public on-disk key format.
		keys := []string{"i/" + gen + "/" + n.Path}
		order := []struct{ by, key string }{{"name", path.Base(n.Path) + "\x00" + n.Path}, {"size", fmt.Sprintf("%016x/", uint64(n.Size)^(uint64(1)<<63)) + n.Path}, {"modified", fmt.Sprintf("%016x%08x/", uint64(n.Modified.Unix())^(uint64(1)<<63), uint32(n.Modified.Nanosecond())) + n.Path}}
		for _, s := range order {
			keys = append(keys, "q/"+gen+"/a/"+s.by+"/"+s.key)
		}
		dp := "q/" + gen + "/d/" + hex.EncodeToString([]byte(path.Dir(n.Path))) + "/"
		keys = append(keys, dp+"path/"+n.Path, dp+"size/"+order[1].key, dp+"modified/"+order[2].key)
		for j, key := range keys {
			var value any = n.Path
			if j == 0 {
				value = n
			}
			encoded, err := json.Marshal(value)
			if err != nil {
				b.Fatal(err)
			}
			changes = append(changes, domain.Change{Key: key, Value: encoded})
		}
		if len(changes) >= 7000 || i == count-1 {
			if err := state.Batch(changes); err != nil {
				b.Fatal(err)
			}
			changes = changes[:0]
		}
	}
	return r
}
