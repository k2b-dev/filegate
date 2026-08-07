package pebble

import (
	"encoding/binary"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/k2b-dev/filegate/v3/domain"
)

func benchID(i int) domain.FileID {
	var id domain.FileID
	binary.BigEndian.PutUint64(id[8:], uint64(i+1))
	return id
}

func buildReadBenchIndex(b *testing.B, n int) (*Index, domain.FileID, []domain.FileID, []string) {
	b.Helper()
	dir := b.TempDir()
	idx, err := Open(dir, 128<<20)
	if err != nil {
		b.Fatalf("open index: %v", err)
	}
	b.Cleanup(func() { _ = idx.Close() })

	rootID := benchID(0)
	if err := idx.Batch(func(batch domain.Batch) error {
		batch.PutEntity(domain.Entity{
			ID:       rootID,
			ParentID: domain.FileID{},
			Name:     "root",
			IsDir:    true,
		})
		return nil
	}); err != nil {
		b.Fatalf("put root entity: %v", err)
	}

	ids := make([]domain.FileID, 0, n)
	names := make([]string, 0, n)
	const chunkSize = 1024
	for start := 0; start < n; start += chunkSize {
		end := start + chunkSize
		if end > n {
			end = n
		}
		if err := idx.Batch(func(batch domain.Batch) error {
			for i := start; i < end; i++ {
				id := benchID(i + 1)
				name := fmt.Sprintf("file-%06d.bin", i)
				ids = append(ids, id)
				names = append(names, name)
				batch.PutEntity(domain.Entity{
					ID:       id,
					ParentID: rootID,
					Name:     name,
					IsDir:    false,
					Size:     4096,
					Mtime:    1_700_000_000_000,
					UID:      1000,
					GID:      1000,
					Mode:     0o644,
					MimeType: "application/octet-stream",
				})
				batch.PutChild(rootID, name, domain.DirEntry{
					ID:    id,
					Name:  name,
					IsDir: false,
					Size:  4096,
					Mtime: 1_700_000_000_000,
				})
			}
			return nil
		}); err != nil {
			b.Fatalf("seed batch [%d:%d): %v", start, end, err)
		}
	}
	return idx, rootID, ids, names
}

func BenchmarkIndexGetEntityHot(b *testing.B) {
	idx, _, ids, _ := buildReadBenchIndex(b, 50_000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := ids[i%len(ids)]
		if _, err := idx.GetEntity(id); err != nil {
			b.Fatalf("get entity: %v", err)
		}
	}
}

func BenchmarkIndexGetEntityHotParallel(b *testing.B) {
	idx, _, ids, _ := buildReadBenchIndex(b, 50_000)
	b.ReportAllocs()
	b.ResetTimer()
	var next uint64
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := int(atomic.AddUint64(&next, 1) - 1)
			id := ids[i%len(ids)]
			if _, err := idx.GetEntity(id); err != nil {
				b.Fatalf("get entity: %v", err)
			}
		}
	})
}

func BenchmarkIndexLookupChildHot(b *testing.B) {
	idx, rootID, _, names := buildReadBenchIndex(b, 50_000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		name := names[i%len(names)]
		if _, err := idx.LookupChild(rootID, name); err != nil {
			b.Fatalf("lookup child: %v", err)
		}
	}
}

func BenchmarkIndexLookupChildHotParallel(b *testing.B) {
	idx, rootID, _, names := buildReadBenchIndex(b, 50_000)
	b.ReportAllocs()
	b.ResetTimer()
	var next uint64
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := int(atomic.AddUint64(&next, 1) - 1)
			name := names[i%len(names)]
			if _, err := idx.LookupChild(rootID, name); err != nil {
				b.Fatalf("lookup child: %v", err)
			}
		}
	})
}
