package apiv1

import (
	"encoding/json"
	"testing"
)

func TestExecutionContextDecode(t *testing.T) {
	for _, raw := range []string{
		`{}`, `{"mode":"other"}`, `{"mode":"service","identity":null}`,
		`{"mode":"service","identity":{"uid":1,"gid":1}}`,
		`{"mode":"unix"}`, `{"mode":"unix","identity":null}`,
		`{"mode":"unix","identity":{"uid":1}}`, `{"mode":"unix","identity":{"gid":1}}`,
		`{"mode":"unix","identity":{"uid":0,"gid":1}}`,
		`{"mode":"unix","identity":{"uid":1,"gid":1,"extra":1}}`,
		`{"mode":"service","extra":1}`,
	} {
		var target ExecutionContext
		if err := json.Unmarshal([]byte(raw), &target); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	for _, raw := range []string{`{"mode":"service"}`, `{"mode":"unix","identity":{"uid":1001,"gid":0,"groups":[4,2,4]}}`} {
		var target ExecutionContext
		if err := json.Unmarshal([]byte(raw), &target); err != nil {
			t.Fatal(err)
		}
		if target.Mode == "unix" && (target.Identity.GID != 0 || len(target.Identity.Groups) != 2 || target.Identity.Groups[0] != 2) {
			t.Fatalf("not normalized: %+v", target)
		}
	}
}
