package filegate

import (
	"context"
	"net/http"
	"net/url"

	apiv1 "github.com/k2b-dev/filegate/v3/api/v1"
)

// SystemClient contains the operational endpoints.
//
// Info probes the mounts, so read it occasionally. Runtime is made of in-memory
// counters and is the one to poll for a dashboard.
type SystemClient struct {
	core *clientCore
}

// Info returns build details, mount health and effective limits. This touches
// the filesystem to probe mounts and is not meant to be polled.
func (c SystemClient) Info(ctx context.Context) (*apiv1.SystemInfoResponse, error) {
	var out apiv1.SystemInfoResponse
	if err := c.core.doJSON(ctx, http.MethodGet, "/v1/system/info", nil, nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Runtime returns live counters for the detector, worker pool, caches and
// upload sessions. Cheap enough to poll.
func (c SystemClient) Runtime(ctx context.Context) (*apiv1.SystemRuntimeResponse, error) {
	var out apiv1.SystemRuntimeResponse
	if err := c.core.doJSON(ctx, http.MethodGet, "/v1/system/runtime", nil, nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Health runs the dependency checks. The server answers 503 when a check fails,
// which surfaces here as an error; use the returned body from a raw call if the
// individual check results matter in that case.
func (c SystemClient) Health(ctx context.Context) (*apiv1.HealthResponse, error) {
	var out apiv1.HealthResponse
	if err := c.core.doJSON(ctx, http.MethodGet, "/v1/health", nil, nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Prune runs one version-retention round. The server returns a conflict when
// another round is already in flight.
func (c SystemClient) Prune(ctx context.Context) (*PruneResponse, error) {
	var out PruneResponse
	if err := c.core.doJSON(ctx, http.MethodPost, "/v1/versions/prune", nil, nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UploadSessions lists resumable upload sessions. An empty phase lists all of
// them; this is how an orphaned session left by an interrupted upload is found,
// since aborting one needs an ID that is otherwise no longer known.
func (c SystemClient) UploadSessions(ctx context.Context, phase string) (*apiv1.UploadSessionListResponse, error) {
	// The query has to travel as url.Values: newRequest assigns the endpoint
	// to URL.Path, which escapes a literal "?" into %3F and turns the filter
	// into part of a path that matches no route.
	query := url.Values{}
	if phase != "" {
		query.Set("phase", phase)
	}
	var out apiv1.UploadSessionListResponse
	if err := c.core.doJSON(ctx, http.MethodGet, "/v1/uploads/sessions", query, nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}
