package filegate

import (
	"context"
	"testing"
)

func TestRawRejectsOffOriginPaths(t *testing.T) {
	c, e := New("https://files.example", "secret")
	if e != nil {
		t.Fatal(e)
	}
	for _, p := range []string{"@attacker.example/", "https://attacker.example/", "//attacker.example/"} {
		if _, e = c.Raw(context.Background(), "GET", p, nil, nil); e == nil {
			t.Fatal("unsafe URL accepted", p)
		}
	}
}
