package filegate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const defaultUserAgent = "filegate-go-sdk/1"

// HTTPDoer is the minimal interface implemented by http.Client.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Config configures the Filegate SDK client.
type Config struct {
	BaseURL        string
	Token          string
	HTTPClient     HTTPDoer
	UserAgent      string
	DefaultHeaders http.Header
}

// Filegate is a stateless, scoped API client.
//
// Pure helpers (segment math + sha256 in Filegate's checksum format, HTTP
// response relaying) live in dedicated subpackages so callers can use them
// without constructing a Filegate:
//
//	import "github.com/valentinkolb/filegate/sdk/filegate/segments"
//	import "github.com/valentinkolb/filegate/sdk/filegate/relay"
type Filegate struct {
	core *clientCore

	Paths        PathsClient
	Nodes        NodesClient
	Uploads      UploadsClient
	Transfers    TransfersClient
	Search       SearchClient
	Index        IndexClient
	Stats        StatsClient
	System       SystemClient
	Config       ConfigClient
	S3Keys       S3KeysClient
	Capabilities CapabilitiesClient
	Versions     VersionsClient
	Downloads    DownloadsClient
	Activity     ActivityClient
}

type clientCore struct {
	baseURL        string
	token          string
	httpClient     HTTPDoer
	userAgent      string
	defaultHeaders http.Header
}

// New creates a new Filegate client with scoped namespaces.
func New(cfg Config) (*Filegate, error) {
	baseURL := strings.TrimSpace(cfg.BaseURL)
	if baseURL == "" {
		return nil, fmt.Errorf("base URL is required")
	}
	baseURL = strings.TrimRight(baseURL, "/")

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	userAgent := strings.TrimSpace(cfg.UserAgent)
	if userAgent == "" {
		userAgent = defaultUserAgent
	}

	core := &clientCore{
		baseURL:        baseURL,
		token:          strings.TrimSpace(cfg.Token),
		httpClient:     httpClient,
		userAgent:      userAgent,
		defaultHeaders: cloneHeader(cfg.DefaultHeaders),
	}

	client := &Filegate{core: core}
	client.Paths = PathsClient{core: core}
	client.Nodes = NodesClient{core: core}
	client.Uploads = UploadsClient{
		core:     core,
		Sessions: UploadSessionsClient{core: core},
	}
	client.Transfers = TransfersClient{core: core}
	client.Search = SearchClient{core: core}
	client.Index = IndexClient{core: core}
	client.Stats = StatsClient{core: core}
	client.System = SystemClient{core: core}
	client.Config = ConfigClient{core: core}
	client.S3Keys = S3KeysClient{core: core}
	client.Capabilities = CapabilitiesClient{core: core}
	client.Versions = VersionsClient{core: core}
	client.Downloads = DownloadsClient{core: core}
	client.Activity = ActivityClient{core: core}
	return client, nil
}

// MustNew creates a client and panics when configuration is invalid.
func MustNew(cfg Config) *Filegate {
	client, err := New(cfg)
	if err != nil {
		panic(err)
	}
	return client
}

// BaseURL returns the configured API base URL.
func (c *Filegate) BaseURL() string {
	if c == nil || c.core == nil {
		return ""
	}
	return c.core.baseURL
}

func (c *clientCore) doRaw(ctx context.Context, method, endpoint string, query url.Values, body io.Reader, contentType string) (*http.Response, error) {
	req, err := c.newRequest(ctx, method, endpoint, query, body, contentType)
	if err != nil {
		return nil, err
	}
	return c.httpClient.Do(req)
}

func (c *clientCore) doJSON(ctx context.Context, method, endpoint string, query url.Values, body io.Reader, contentType string, out any) error {
	resp, err := c.doRaw(ctx, method, endpoint, query, body, contentType)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if err := ensureSuccess(resp); err != nil {
		return err
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}

	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("decode json response: %w", err)
	}
	return nil
}

func (c *clientCore) newRequest(ctx context.Context, method, endpoint string, query url.Values, body io.Reader, contentType string) (*http.Request, error) {
	requestURL, err := c.endpointURL(endpoint, query)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, requestURL, body)
	if err != nil {
		return nil, err
	}
	for key, values := range c.defaultHeaders {
		for _, v := range values {
			req.Header.Add(key, v)
		}
	}
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req, nil
}

func (c *clientCore) endpointURL(endpoint string, query url.Values) (string, error) {
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return "", fmt.Errorf("parse base URL: %w", err)
	}
	if !strings.HasPrefix(endpoint, "/") {
		endpoint = "/" + endpoint
	}
	base.Path = strings.TrimRight(base.Path, "/") + endpoint
	if len(query) > 0 {
		base.RawQuery = query.Encode()
	}
	return base.String(), nil
}

func cloneHeader(h http.Header) http.Header {
	if h == nil {
		return nil
	}
	out := make(http.Header, len(h))
	for k, values := range h {
		dup := make([]string, len(values))
		copy(dup, values)
		out[k] = dup
	}
	return out
}
