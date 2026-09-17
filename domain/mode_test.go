package domain

import (
	"os"
	"testing"
)

func TestUnixModeSpecialBits(t *testing.T) {
	for _, tc := range []struct {
		unix   uint32
		goMode os.FileMode
	}{
		{0644, 0644},
		{02770, 0770 | os.ModeSetgid},
		{04755, 0755 | os.ModeSetuid},
		{01777, 0777 | os.ModeSticky},
		{07777, 0777 | os.ModeSetuid | os.ModeSetgid | os.ModeSticky},
	} {
		if got := FileMode(tc.unix); got != tc.goMode {
			t.Errorf("Unix %04o: got %v, want %v", tc.unix, got, tc.goMode)
		}
		if got := UnixMode(tc.goMode | os.ModeDir); got != tc.unix {
			t.Errorf("Go %v: got %04o, want %04o", tc.goMode, got, tc.unix)
		}
	}
}

func TestOwnershipRejectsUnsafeModesAndIDs(t *testing.T) {
	zero, negative := 0, -1
	for _, o := range []Ownership{
		{Mode: "2770"}, {DirMode: "4770"}, {DirMode: "1770"},
		{Mode: "888"}, {DirMode: "-1"}, {UID: &zero},
		{UID: &negative, GID: &zero},
	} {
		if e := validateOwnership(&o); e == nil {
			t.Errorf("accepted %+v", o)
		}
	}
	if e := validateOwnership(&Ownership{UID: &zero, GID: &zero, Mode: "0750", DirMode: "2770"}); e != nil {
		t.Fatal(e)
	}
}
