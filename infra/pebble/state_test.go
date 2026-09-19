package pebble

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"testing"

	"github.com/k2b-dev/filegate/v6/domain"
)

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
		{false, "q/0", nil},
		{true, "", []string{"p/2", "p/1", "p/0"}},
		{true, "p/1", []string{"p/0"}},
		{true, "p/15", []string{"p/1", "p/0"}},
		{true, "q/0", []string{"p/2", "p/1", "p/0"}},
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
