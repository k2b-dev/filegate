package httpadapter

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/k2b-dev/filegate/v6/domain"
)

// ExecutionHeader is accepted only on backend-authenticated operations. Direct
// transfers use the identity authenticated by their capability instead.
const ExecutionHeader = "X-Filegate-Execution"

func requestExecution(r *http.Request) (*domain.ExecutionIdentity, error) {
	values, present := r.Header[http.CanonicalHeaderKey(ExecutionHeader)]
	if !present {
		return nil, nil
	}
	if len(values) != 1 || len(values[0]) > 4096 || values[0] == "" {
		return nil, domain.ErrInvalid
	}
	var wire struct {
		UID    *uint32  `json:"uid"`
		GID    *uint32  `json:"gid"`
		Groups []uint32 `json:"groups"`
	}
	d := json.NewDecoder(strings.NewReader(values[0]))
	d.DisallowUnknownFields()
	if d.Decode(&wire) != nil || wire.UID == nil || wire.GID == nil {
		return nil, domain.ErrInvalid
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return nil, domain.ErrInvalid
	}
	return domain.NormalizeExecution(&domain.ExecutionIdentity{UID: *wire.UID, GID: *wire.GID, Groups: wire.Groups})
}

func sameExecution(a, b *domain.ExecutionIdentity) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.UID == b.UID && a.GID == b.GID && slices.Equal(a.Groups, b.Groups)
}

func executionAdminRoute(pattern string) bool {
	switch pattern {
	case "GET /v1/roots/{root}", "GET /v1/roots/{root}/index", "GET /v1/roots/{root}/stats", "GET /v1/roots/{root}/search", "POST /v1/roots/{root}/index/rebuild", "POST /v1/roots/{root}/stats/refresh", "POST /v1/roots/{root}/versions/prune":
		return true
	}
	return false
}

func checkSessionExecution(root *domain.Root, id string, execution *domain.ExecutionIdentity, exact bool) error {
	if execution == nil && !exact {
		return nil
	}
	s, err := root.Session(id)
	if err != nil {
		return err
	}
	if !sameExecution(s.Execution, execution) {
		return errHTTP{http.StatusForbidden, "execution_mismatch"}
	}
	return nil
}
