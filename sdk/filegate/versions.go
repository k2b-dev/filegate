package filegate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// VersionsClient covers the per-file versioning surface
// (`/v1/nodes/{id}/versions/...`). Returned 404 from any of these
// methods while versioning is disabled is the canonical "versioning
// unsupported" signal —
// the caller can fall back to a non-versioned UX without a separate
// capability check.
type VersionsClient struct {
	core *clientCore
}

// ListVersionsOptions controls List pagination. Cursor and Limit are
// optional; an empty Cursor starts from the beginning, Limit <=0 lets
// the server pick (defaults to 100, capped at 1000).
type ListVersionsOptions struct {
	Cursor string
	Limit  int
}

func (o ListVersionsOptions) toQuery() url.Values {
	q := url.Values{}
	if strings.TrimSpace(o.Cursor) != "" {
		q.Set("cursor", o.Cursor)
	}
	if o.Limit > 0 {
		q.Set("limit", strconv.Itoa(o.Limit))
	}
	return q
}

// List returns one page of versions for fileID.
func (c VersionsClient) List(ctx context.Context, fileID string, opts ListVersionsOptions) (*ListVersionsResponse, error) {
	trimmed := strings.TrimSpace(fileID)
	if trimmed == "" {
		return nil, fmt.Errorf("fileID is required")
	}
	var out ListVersionsResponse
	endpoint := "/v1/nodes/" + url.PathEscape(trimmed) + "/versions"
	if err := c.core.doJSON(ctx, http.MethodGet, endpoint, opts.toQuery(), nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListAll iterates the full version history for fileID. Calls List
// repeatedly until NextCursor is empty. Convenience wrapper for
// callers that want every entry without managing pagination.
func (c VersionsClient) ListAll(ctx context.Context, fileID string) ([]VersionResponse, error) {
	out := make([]VersionResponse, 0)
	cursor := ""
	for {
		page, err := c.List(ctx, fileID, ListVersionsOptions{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return out, nil
}

// ContentRaw returns the raw HTTP response for a version's bytes,
// including non-2xx responses. Use this for relay handlers; PipeContent
// for the standard "throw on non-2xx" behavior.
func (c VersionsClient) ContentRaw(ctx context.Context, fileID, versionID string) (*http.Response, error) {
	endpoint, err := versionEndpoint(fileID, versionID, "/content")
	if err != nil {
		return nil, err
	}
	return c.core.doRaw(ctx, http.MethodGet, endpoint, nil, nil, "")
}

// PipeContent streams a version's bytes into dst.
func (c VersionsClient) PipeContent(ctx context.Context, fileID, versionID string, dst io.Writer) (*PipeResult, error) {
	resp, err := c.ContentRaw(ctx, fileID, versionID)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := ensureSuccess(resp); err != nil {
		return nil, err
	}
	n, err := io.Copy(dst, resp.Body)
	if err != nil {
		return nil, err
	}
	return &PipeResult{
		StatusCode: resp.StatusCode,
		Header:     cloneHeader(resp.Header),
		Bytes:      n,
	}, nil
}

// Snapshot captures the file's current bytes as a NEW pinned version,
// ignoring cooldown and the size floor. label is opaque (≤ 2 KiB by
// default, configurable).
func (c VersionsClient) Snapshot(ctx context.Context, fileID, label string) (*VersionResponse, error) {
	trimmed := strings.TrimSpace(fileID)
	if trimmed == "" {
		return nil, fmt.Errorf("fileID is required")
	}
	body, err := json.Marshal(VersionSnapshotRequest{Label: label})
	if err != nil {
		return nil, fmt.Errorf("marshal snapshot request: %w", err)
	}
	var out VersionResponse
	endpoint := "/v1/nodes/" + url.PathEscape(trimmed) + "/versions/snapshot"
	if err := c.core.doJSON(ctx, http.MethodPost, endpoint, nil, bytes.NewReader(body), "application/json", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Pin marks an existing version as pinned. label is optional — pass
// nil to leave the existing label unchanged, or a non-nil pointer
// (including pointing to "") to set/clear it.
func (c VersionsClient) Pin(ctx context.Context, fileID, versionID string, label *string) (*VersionResponse, error) {
	endpoint, err := versionEndpoint(fileID, versionID, "/pin")
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(VersionPinRequest{Label: label})
	if err != nil {
		return nil, fmt.Errorf("marshal pin request: %w", err)
	}
	var out VersionResponse
	if err := c.core.doJSON(ctx, http.MethodPost, endpoint, nil, bytes.NewReader(body), "application/json", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Unpin removes the pinned flag from a version. Label is preserved.
func (c VersionsClient) Unpin(ctx context.Context, fileID, versionID string) (*VersionResponse, error) {
	endpoint, err := versionEndpoint(fileID, versionID, "/unpin")
	if err != nil {
		return nil, err
	}
	var out VersionResponse
	if err := c.core.doJSON(ctx, http.MethodPost, endpoint, nil, nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RestoreOptions controls Restore. AsNewFile=false (default) replaces
// the source file's bytes in place after first snapshotting current
// state. AsNewFile=true creates a fresh sibling file; Name overrides
// the default `<base>-restored<ext>` (with `-N` conflict suffix).
type RestoreOptions struct {
	AsNewFile bool
	Name      string
}

// Restore brings the bytes of versionID back. See RestoreOptions for
// in-place vs as-new semantics.
func (c VersionsClient) Restore(ctx context.Context, fileID, versionID string, opts RestoreOptions) (*VersionRestoreResponse, error) {
	endpoint, err := versionEndpoint(fileID, versionID, "/restore")
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(VersionRestoreRequest{
		AsNewFile: opts.AsNewFile,
		Name:      opts.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal restore request: %w", err)
	}
	var out VersionRestoreResponse
	if err := c.core.doJSON(ctx, http.MethodPost, endpoint, nil, bytes.NewReader(body), "application/json", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Delete removes a single version (blob + metadata). Works on any
// version, including pinned ones (operator override).
func (c VersionsClient) Delete(ctx context.Context, fileID, versionID string) error {
	endpoint, err := versionEndpoint(fileID, versionID, "")
	if err != nil {
		return err
	}
	resp, err := c.core.doRaw(ctx, http.MethodDelete, endpoint, nil, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := ensureSuccess(resp); err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// versionEndpoint composes /v1/nodes/<file>/versions/<vid><suffix>.
// suffix may be empty for the bare /versions/{vid} path used by Delete.
func versionEndpoint(fileID, versionID, suffix string) (string, error) {
	trimmed := strings.TrimSpace(fileID)
	if trimmed == "" {
		return "", fmt.Errorf("fileID is required")
	}
	vid := strings.TrimSpace(versionID)
	if vid == "" {
		return "", fmt.Errorf("versionID is required")
	}
	return "/v1/nodes/" + url.PathEscape(trimmed) + "/versions/" + url.PathEscape(vid) + suffix, nil
}
