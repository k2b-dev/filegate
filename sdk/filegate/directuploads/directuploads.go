// Package directuploads contains helpers for upload-session direct tokens.
package directuploads

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/k2b-dev/filegate/v3/sdk/filegate"
)

type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

type SessionRequest struct {
	Direct     filegate.UploadSessionDirect
	HTTPClient HTTPDoer
}

type PutSegmentRequest struct {
	Direct      filegate.UploadSessionDirect
	Index       int
	Body        io.Reader
	Checksum    string
	ContentType string
	HTTPClient  HTTPDoer
}

type SegmentsClient struct{}

var Segments SegmentsClient

func (SegmentsClient) Put(ctx context.Context, req PutSegmentRequest) (*filegate.UploadSegmentResponse, error) {
	resp, err := Segments.PutRaw(ctx, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := ensureSuccess(resp); err != nil {
		return nil, err
	}
	var out filegate.UploadSegmentResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode upload segment response: %w", err)
	}
	return &out, nil
}

func (SegmentsClient) PutRaw(ctx context.Context, req PutSegmentRequest) (*http.Response, error) {
	if req.Index < 0 {
		return nil, fmt.Errorf("index must be >= 0")
	}
	if strings.TrimSpace(req.Direct.BaseURL) == "" || strings.TrimSpace(req.Direct.Token) == "" {
		return nil, fmt.Errorf("direct upload session is required")
	}
	contentType := strings.TrimSpace(req.ContentType)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	httpReq, err := http.NewRequestWithContext(
		ctx,
		http.MethodPut,
		strings.TrimRight(req.Direct.BaseURL, "/")+"/segments/"+strconv.Itoa(req.Index),
		req.Body,
	)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Filegate-Upload-Session", req.Direct.Token)
	httpReq.Header.Set("Content-Type", contentType)
	if checksum := strings.TrimSpace(req.Checksum); checksum != "" {
		httpReq.Header.Set("X-Segment-Checksum", checksum)
	}
	return httpClient(req.HTTPClient).Do(httpReq)
}

func Status(ctx context.Context, req SessionRequest) (*filegate.UploadSessionResponse, error) {
	var out filegate.UploadSessionResponse
	if err := doJSON(ctx, req, http.MethodGet, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func Commit(ctx context.Context, req SessionRequest) (*filegate.UploadSessionCommitResponse, error) {
	var out filegate.UploadSessionCommitResponse
	if err := doJSON(ctx, req, http.MethodPost, "/commit", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func Abort(ctx context.Context, req SessionRequest) error {
	return doJSON(ctx, req, http.MethodDelete, "", nil)
}

func doJSON(ctx context.Context, req SessionRequest, method, suffix string, out any) error {
	if strings.TrimSpace(req.Direct.BaseURL) == "" || strings.TrimSpace(req.Direct.Token) == "" {
		return fmt.Errorf("direct upload session is required")
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(req.Direct.BaseURL, "/")+suffix, nil)
	if err != nil {
		return err
	}
	httpReq.Header.Set("Filegate-Upload-Session", req.Direct.Token)
	resp, err := httpClient(req.HTTPClient).Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := ensureSuccess(resp); err != nil {
		return err
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func httpClient(client HTTPDoer) HTTPDoer {
	if client != nil {
		return client
	}
	return http.DefaultClient
}

func ensureSuccess(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	var body struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Error == "" {
		body.Error = resp.Status
	}
	return fmt.Errorf("filegate direct upload %s %s failed: %s", resp.Request.Method, resp.Request.URL, body.Error)
}
