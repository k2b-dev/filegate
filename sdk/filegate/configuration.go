package filegate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	apiv1 "github.com/valentinkolb/filegate/api/v1"
)

// ConfigClient contains declarative configuration endpoints.
type ConfigClient struct {
	core *clientCore
}

// Schema returns every configuration key with its type, scope and default.
func (c ConfigClient) Schema(ctx context.Context) (*ConfigSchemaResponse, error) {
	var out ConfigSchemaResponse
	if err := c.core.doJSON(ctx, http.MethodGet, "/v1/config/schema", nil, nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Values returns desired and effective values plus manifest metadata.
func (c ConfigClient) Values(ctx context.Context) (*ConfigValuesResponse, error) {
	var out ConfigValuesResponse
	if err := c.core.doJSON(ctx, http.MethodGet, "/v1/config", nil, nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Plan validates and diffs a complete replacement manifest without applying it.
func (c ConfigClient) Plan(ctx context.Context, values map[string]any) (*ConfigManifestPlanResponse, error) {
	payload, err := json.Marshal(apiv1.ConfigManifestPlanRequest{Values: values})
	if err != nil {
		return nil, fmt.Errorf("marshal config plan request: %w", err)
	}
	var out ConfigManifestPlanResponse
	if err := c.core.doJSON(ctx, http.MethodPost, "/v1/config/plan", nil, bytes.NewReader(payload), "application/json", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Apply replaces the manifest if expectedRevision still matches the server.
func (c ConfigClient) Apply(ctx context.Context, values map[string]any, expectedRevision string) (*ConfigManifestApplyResponse, error) {
	payload, err := json.Marshal(apiv1.ConfigManifestApplyRequest{
		Values:           values,
		ExpectedRevision: expectedRevision,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal config apply request: %w", err)
	}
	var out ConfigManifestApplyResponse
	if err := c.core.doJSON(ctx, http.MethodPost, "/v1/config/apply", nil, bytes.NewReader(payload), "application/json", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// S3KeysClient contains S3 access-key lifecycle endpoints.
type S3KeysClient struct {
	core *clientCore
}

func (c S3KeysClient) List(ctx context.Context) (*S3KeyListResponse, error) {
	var out S3KeyListResponse
	if err := c.core.doJSON(ctx, http.MethodGet, "/v1/s3/keys", nil, nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Create returns the new key including its one-time secret.
func (c S3KeysClient) Create(ctx context.Context, req S3KeyCreateRequest) (*S3KeyCreated, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal S3 key create request: %w", err)
	}
	var out S3KeyCreated
	if err := c.core.doJSON(ctx, http.MethodPost, "/v1/s3/keys", nil, bytes.NewReader(payload), "application/json", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c S3KeysClient) Update(ctx context.Context, accessKey string, req S3KeyUpdateRequest) (*S3Key, error) {
	endpoint, err := s3KeyEndpoint(accessKey, "")
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal S3 key update request: %w", err)
	}
	var out S3Key
	if err := c.core.doJSON(ctx, http.MethodPatch, endpoint, nil, bytes.NewReader(payload), "application/json", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Rotate returns a replacement secret while keeping the key's grants.
func (c S3KeysClient) Rotate(ctx context.Context, accessKey string) (*S3KeyCreated, error) {
	endpoint, err := s3KeyEndpoint(accessKey, "/rotate")
	if err != nil {
		return nil, err
	}
	var out S3KeyCreated
	if err := c.core.doJSON(ctx, http.MethodPost, endpoint, nil, nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c S3KeysClient) Delete(ctx context.Context, accessKey string) error {
	endpoint, err := s3KeyEndpoint(accessKey, "")
	if err != nil {
		return err
	}
	resp, err := c.core.doRaw(ctx, http.MethodDelete, endpoint, nil, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return ensureSuccess(resp)
}

func s3KeyEndpoint(accessKey, suffix string) (string, error) {
	accessKey = strings.TrimSpace(accessKey)
	if accessKey == "" {
		return "", fmt.Errorf("accessKey is required")
	}
	return "/v1/s3/keys/" + url.PathEscape(accessKey) + suffix, nil
}
