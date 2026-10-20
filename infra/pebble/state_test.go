package pebble

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/k2b-dev/filegate/v7/domain"
)

func TestScansIncludeAllPrefixBytes(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	keys := []string{"a/0", "p/", "p/b", "p/\xff", "p/\xffa", "p/\xff\xff", "p0", "q/0", "\xff", "\xff\xff", "\xff\xffa"}
	for _, key := range keys {
		if err := s.Put(key, 0); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	for _, prefix := range []string{"p/", "p/\xff", "p/\xff\xff", "\xff", "\xff\xff", "", "missing/"} {
		t.Run(fmt.Sprintf("%x", prefix), func(t *testing.T) {
			var want []string
			for _, key := range keys {
				if strings.HasPrefix(key, prefix) {
					want = append(want, key)
				}
			}
			var got []string
			if err := s.Scan(prefix, func(k string, _ []byte) error { got = append(got, k); return nil }); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, want) {
				t.Errorf("Scan: got %q, want %q", got, want)
			}
			for _, reader := range []domain.StateSnapshot{s, snapshot} {
				for _, reverse := range []bool{false, true} {
					scan := reader.ScanAfter
					expected := slices.Clone(want)
					if reverse {
						scan = reader.ScanBefore
						slices.Reverse(expected)
					}
					// Page one key at a time, including exclusive seeks after/before
					// raw 0xff bytes, on both the live DB and an immutable snapshot.
					got = nil
					cursor := ""
					stop := errors.New("page complete")
					for page := 0; ; page++ {
						if page > len(want) {
							t.Fatal("pagination did not terminate")
						}
						err := scan(prefix, cursor, func(k string, _ []byte) error {
							got = append(got, k)
							cursor = k
							return stop
						})
						if err == nil {
							break
						}
						if !errors.Is(err, stop) {
							t.Fatal(err)
						}
					}
					if !slices.Equal(got, expected) {
						t.Errorf("%T reverse=%v: got %q, want %q", reader, reverse, got, expected)
					}
				}
			}
		})
	}
}

func TestSeekScansAreExclusiveAndPrefixBounded(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, key := range []string{"a/0", "p/0", "p/1", "p/2", "q/0"} {
		if err := s.Put(key, key); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		reverse bool
		cursor  string
		want    []string
	}{
		{false, "", []string{"p/0", "p/1", "p/2"}},
		{false, "p/1", []string{"p/2"}},
		{false, "p/15", []string{"p/2"}},
		{false, "a/0", []string{"p/0", "p/1", "p/2"}},
		{false, "a", []string{"p/0", "p/1", "p/2"}},
		{false, "q/0", nil},
		{true, "", []string{"p/2", "p/1", "p/0"}},
		{true, "p/1", []string{"p/0"}},
		{true, "p/15", []string{"p/1", "p/0"}},
		{true, "q/0", []string{"p/2", "p/1", "p/0"}},
		{true, "z", []string{"p/2", "p/1", "p/0"}},
		{true, "a/0", nil},
	} {
		var got []string
		scan := s.ScanAfter
		if tc.reverse {
			scan = s.ScanBefore
		}
		if err := scan("p/", tc.cursor, func(k string, _ []byte) error { got = append(got, k); return nil }); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("reverse=%v cursor=%q got=%v want=%v", tc.reverse, tc.cursor, got, tc.want)
		}
	}
	stop := errors.New("stop")
	if err := s.ScanAfter("p/", "", func(string, []byte) error { return stop }); !errors.Is(err, stop) {
		t.Fatal(err)
	}
}

func TestSeekPaginationVisitsRowsLinearly(t *testing.T) {
	for _, size := range []int{1000, 10000, 100000} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			s, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			cs := make([]domain.Change, 0, size)
			for i := 0; i < size; i++ {
				cs = append(cs, domain.Change{Key: "p/" + fmt.Sprintf("%08d", i), Value: []byte(`0`)})
			}
			if err := s.Batch(cs); err != nil {
				t.Fatal(err)
			}
			cursor := ""
			visited := 0
			pages := 0
			stop := errors.New("page")
			for {
				count := 0
				err := s.ScanAfter("p/", cursor, func(k string, _ []byte) error {
					visited++
					count++
					cursor = k
					if count == 100 {
						return stop
					}
					return nil
				})
				pages++
				if err == nil {
					break
				}
				if !errors.Is(err, stop) {
					t.Fatal(err)
				}
			}
			if visited != size {
				t.Fatalf("%d rows visited %d times", size, visited)
			}
			t.Logf("rows=%d visits=%d pages=%d", size, visited, pages)
		})
	}
}
