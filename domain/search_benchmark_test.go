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
// The broad fuzzy-name case can exceed retained-hit budgets at 1M; its dated
// variant measures a full first page within those same production budgets.
// Run the large fixture explicitly with -benchtime=1x -count=1.
func BenchmarkIndexedSearch(b *testing.B) {
	for _, size := range []int{10000, 100000, 1000000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			r := indexedSearchFixture(b, size)
			for _, tc := range []struct {
				name, query string
				opts        domain.ListingOptions
			}{
				{"exact-name", "quartalsbericht", domain.ListingOptions{Sort: "name"}},
				{"fuzzy-name", "quatralsberihct", domain.ListingOptions{Match: "fuzzy", Sort: "name"}},
				{"fuzzy-name-selective", "quatralsberihct 2026", domain.ListingOptions{Match: "fuzzy", Sort: "name"}},
				{"fuzzy-relevance", "quatralsberihct", domain.ListingOptions{Match: "fuzzy", Sort: "relevance"}},
				{"path-relevance", "archive quatralsberihct", domain.ListingOptions{Match: "fuzzy", Sort: "relevance", In: "path"}},
				{"filter-pdf", "", domain.ListingOptions{Ext: []string{"pdf"}}},
			} {
				b.Run(tc.name, func(b *testing.B) {
					b.ReportAllocs()
					pprof.Do(context.Background(), pprof.Labels("phase", "search"), func(ctx context.Context) {
						for i := 0; i < b.N; i++ {
							page, err := r.Search(ctx, tc.query, ".", tc.opts)
							if size == 1000000 && tc.name == "fuzzy-name" && errors.Is(err, domain.ErrLimit) {
								// The broad diagnostic exceeds the retention budget;
								// fuzzy-name-selective measures the successful first page.
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

func indexedSearchFixture(b *testing.B, count int) *domain.Root {
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
