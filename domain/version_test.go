package domain

import (
	"strings"
	"testing"
	"time"
)

func TestRetentionCalendarUnionAndPinnedExclusion(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	vs := []Version{{ID: "p", Pinned: true, Created: now}, {ID: "a", Created: now.Add(-time.Hour)}, {ID: "b", Created: now.Add(-2 * time.Hour)}, {ID: "c", Created: now.AddDate(0, 0, -1)}, {ID: "d", Created: now.AddDate(0, -1, 0)}, {ID: "old", Created: now.AddDate(-1, 0, 0)}}
	kept := Retained(vs, Keep{Last: 2, Daily: 2, Monthly: 2}, now)
	for _, id := range []string{"p", "a", "b", "c", "d"} {
		if !kept[id] {
			t.Fatal("missing", id)
		}
	}
	if kept["old"] {
		t.Fatal("old retained")
	}
	onlyPins := Retained(vs, Keep{}, now)
	if len(onlyPins) != 1 || !onlyPins["p"] {
		t.Fatal(onlyPins)
	}
}
func TestMetadataLimitCountsSerializedUTF8(t *testing.T) {
	if e := ValidateMetadata(Metadata{"m": strings.Repeat("x", 8184)}); e != nil {
		t.Fatal(e)
	}
	if e := ValidateMetadata(Metadata{"m": strings.Repeat("x", 8185)}); e != ErrLimit {
		t.Fatal(e)
	}
	if e := ValidateMetadata(Metadata{"m": strings.Repeat("ä", 4093)}); e != ErrLimit {
		t.Fatal(e)
	}
}
