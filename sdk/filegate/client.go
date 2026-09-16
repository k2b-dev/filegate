// Package filegate provides the trusted-backend Go client for Filegate.
// Browser clients receive scoped direct URLs, never the daemon bearer token.
package filegate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	api "github.com/k2b-dev/filegate/v4/api/v1"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type Node = api.Node
type Page = api.Page
type RootInfo = api.RootInfo
type WriteOptions = api.WriteOptions
type Ownership = api.Ownership
type Metadata = api.Metadata
type Version = api.Version
type Session = api.Session
type DirectURL = api.DirectURL
type SessionCreated = api.SessionCreated
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("filegate %d %s: %s", e.Status, e.Code, e.Message)
}

type Client struct {
	base  string
	token string
	http  *http.Client
}
type Root struct {
	client *Client
	name   string
}

func New(base, token string) (*Client, error) {
	u, e := url.Parse(base)
	if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" || token == "" {
		return nil, fmt.Errorf("HTTP(S) origin and token required")
	}
	return &Client{strings.TrimRight(base, "/"), token, &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (c *Client) Root(name string) *Root { return &Root{c, name} }
func (c *Client) Raw(ctx context.Context, method, path string, q url.Values, body any) (*http.Response, error) {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return nil, fmt.Errorf("request path must be relative to Filegate origin")
	}
	parsed, e := url.Parse(path)
	if e != nil || parsed.IsAbs() || parsed.Host != "" || parsed.User != nil {
		return nil, fmt.Errorf("invalid request path")
	}
	var b io.Reader
	if body != nil {
		data, e := json.Marshal(body)
		if e != nil {
			return nil, e
		}
		b = bytes.NewReader(data)
	}
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, e := http.NewRequestWithContext(ctx, method, u, b)
	if e != nil {
		return nil, e
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.http.Do(req)
}
func decode(resp *http.Response, out any) error {
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var v api.Error
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&v)
		return &APIError{resp.StatusCode, v.Error, v.Message}
	}
	if out == nil || resp.StatusCode == 204 {
		_, e := io.Copy(io.Discard, resp.Body)
		return e
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
func (c *Client) call(ctx context.Context, method, path string, q url.Values, body, out any) error {
	r, e := c.Raw(ctx, method, path, q, body)
	if e != nil {
		return e
	}
	return decode(r, out)
}
func (r *Root) endpoint(s string) string { return "/v1/roots/" + url.PathEscape(r.name) + s }
func (c *Client) System(ctx context.Context) (api.System, error) {
	var v api.System
	e := c.call(ctx, "GET", "/v1/system", nil, nil, &v)
	return v, e
}
func (c *Client) Roots(ctx context.Context) ([]RootInfo, error) {
	var v []RootInfo
	e := c.call(ctx, "GET", "/v1/roots", nil, nil, &v)
	return v, e
}
func (r *Root) Info(ctx context.Context) (RootInfo, error) {
	var v RootInfo
	e := r.client.call(ctx, "GET", r.endpoint(""), nil, nil, &v)
	return v, e
}
func query(p string) url.Values { return url.Values{"path": {p}} }
func (r *Root) Stat(ctx context.Context, p string) (Node, error) {
	var v Node
	e := r.client.call(ctx, "GET", r.endpoint("/stat"), query(p), nil, &v)
	return v, e
}
func (r *Root) Resolve(ctx context.Context, id string) (Node, error) {
	var v Node
	e := r.client.call(ctx, "GET", r.endpoint("/resolve"), url.Values{"id": {id}}, nil, &v)
	return v, e
}
func (r *Root) List(ctx context.Context, p, after string, limit int) (Page, error) {
	q := query(p)
	q.Set("after", after)
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var v Page
	e := r.client.call(ctx, "GET", r.endpoint("/entries"), q, nil, &v)
	return v, e
}
func (r *Root) Search(ctx context.Context, text, p, after string, limit, maxEntries int) (Page, error) {
	q := query(p)
	q.Set("q", text)
	q.Set("after", after)
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if maxEntries > 0 {
		q.Set("maxEntries", strconv.Itoa(maxEntries))
	}
	var v Page
	e := r.client.call(ctx, "GET", r.endpoint("/search"), q, nil, &v)
	return v, e
}
func (r *Root) ContentRaw(ctx context.Context, p string) (*http.Response, error) {
	return r.client.Raw(ctx, "GET", r.endpoint("/content"), query(p), nil)
}
func (r *Root) ArchiveRaw(ctx context.Context, p string) (*http.Response, error) {
	return r.client.Raw(ctx, "GET", r.endpoint("/archive"), query(p), nil)
}
func (r *Root) Mkdir(ctx context.Context, p string, o *Ownership) (Node, error) {
	var v Node
	e := r.client.call(ctx, "POST", r.endpoint("/directories"), nil, api.MkdirRequest{Path: p, Ownership: o}, &v)
	return v, e
}
func (r *Root) Remove(ctx context.Context, p string, recursive bool) error {
	q := query(p)
	q.Set("recursive", strconv.FormatBool(recursive))
	return r.client.call(ctx, "DELETE", r.endpoint("/files"), q, nil, nil)
}
func (r *Root) Transfer(ctx context.Context, req api.TransferRequest) (Node, error) {
	var v Node
	e := r.client.call(ctx, "POST", r.endpoint("/transfers"), nil, req, &v)
	return v, e
}
func (r *Root) DirectUpload(ctx context.Context, p string, size int64, o WriteOptions) (DirectURL, error) {
	var v DirectURL
	e := r.client.call(ctx, "POST", r.endpoint("/uploads/direct"), nil, api.DirectRequest{Path: p, Size: size, WriteOptions: o}, &v)
	return v, e
}
func (r *Root) DirectDownload(ctx context.Context, p string, expiresIn int) (DirectURL, error) {
	var v DirectURL
	e := r.client.call(ctx, "POST", r.endpoint("/downloads/direct"), nil, map[string]any{"path": p, "expiresIn": expiresIn}, &v)
	return v, e
}
func (r *Root) Put(ctx context.Context, p string, body io.Reader, size int64, o WriteOptions) (Node, error) {
	u, e := r.DirectUpload(ctx, p, size, o)
	if e != nil {
		return Node{}, e
	}
	return PutDirect(ctx, u.URL, body, size)
}
func PutDirect(ctx context.Context, u string, body io.Reader, size int64) (Node, error) {
	var v Node
	req, e := http.NewRequestWithContext(ctx, "PUT", u, body)
	if e != nil {
		return v, e
	}
	req.ContentLength = size
	resp, e := directHTTP.Do(req)
	if e != nil {
		return v, e
	}
	e = decode(resp, &v)
	return v, e
}

var directHTTP = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (r *Root) CreateSession(ctx context.Context, p string, size int64, o WriteOptions) (SessionCreated, error) {
	var v SessionCreated
	e := r.client.call(ctx, "POST", r.endpoint("/uploads/sessions"), nil, api.SessionRequest{Path: p, Size: size, WriteOptions: o}, &v)
	return v, e
}

type DirectSession struct{ URL string }

func (s DirectSession) call(ctx context.Context, method string, index int, body io.Reader, out any) error {
	u := s.URL
	if index >= 0 {
		parsed, e := url.Parse(u)
		if e != nil {
			return e
		}
		q := parsed.Query()
		q.Set("segment", strconv.Itoa(index))
		parsed.RawQuery = q.Encode()
		u = parsed.String()
	}
	req, e := http.NewRequestWithContext(ctx, method, u, body)
	if e != nil {
		return e
	}
	resp, e := directHTTP.Do(req)
	if e != nil {
		return e
	}
	return decode(resp, out)
}
func (s DirectSession) Status(ctx context.Context) (Session, error) {
	var v Session
	e := s.call(ctx, "GET", -1, nil, &v)
	return v, e
}
func (s DirectSession) Put(ctx context.Context, index int, body io.Reader) (Session, error) {
	var v Session
	e := s.call(ctx, "PUT", index, body, &v)
	return v, e
}
func (s DirectSession) Commit(ctx context.Context) (Node, error) {
	var v Node
	e := s.call(ctx, "POST", -1, nil, &v)
	return v, e
}
func (s DirectSession) Abort(ctx context.Context) error { return s.call(ctx, "DELETE", -1, nil, nil) }
func (r *Root) Rebuild(ctx context.Context) error {
	return r.client.call(ctx, "POST", r.endpoint("/index/rebuild"), nil, nil, nil)
}
func (r *Root) RefreshStats(ctx context.Context, maxEntries int) (api.Stats, error) {
	var v api.Stats
	e := r.client.call(ctx, "POST", r.endpoint("/stats/refresh"), url.Values{"maxEntries": {strconv.Itoa(maxEntries)}}, nil, &v)
	return v, e
}
func (r *Root) Versions(ctx context.Context, p string) ([]Version, error) {
	var v []Version
	e := r.client.call(ctx, "GET", r.endpoint("/versions"), query(p), nil, &v)
	return v, e
}
func (r *Root) Snapshot(ctx context.Context, p string, o api.VersionRequest) (Version, error) {
	var v Version
	e := r.client.call(ctx, "POST", r.endpoint("/versions"), query(p), o, &v)
	return v, e
}
func (r *Root) UpdateVersion(ctx context.Context, p, id string, o api.VersionRequest) (Version, error) {
	var v Version
	e := r.client.call(ctx, "PATCH", r.endpoint("/versions/"+url.PathEscape(id)), query(p), o, &v)
	return v, e
}
func (r *Root) DeleteVersion(ctx context.Context, p, id string) error {
	return r.client.call(ctx, "DELETE", r.endpoint("/versions/"+url.PathEscape(id)), query(p), nil, nil)
}
func (r *Root) Restore(ctx context.Context, p, id string) (Node, error) {
	var v Node
	e := r.client.call(ctx, "POST", r.endpoint("/versions/"+url.PathEscape(id)+"/restore"), query(p), nil, &v)
	return v, e
}
func (r *Root) VersionContentRaw(ctx context.Context, p, id string) (*http.Response, error) {
	return r.client.Raw(ctx, "GET", r.endpoint("/versions/"+url.PathEscape(id)+"/content"), query(p), nil)
}
func (r *Root) Prune(ctx context.Context) (int, error) {
	var v struct {
		Deleted int `json:"deleted"`
	}
	e := r.client.call(ctx, "POST", r.endpoint("/versions/prune"), nil, nil, &v)
	return v.Deleted, e
}

func (r *Root) SetOwnership(ctx context.Context, p string, o Ownership) (Node, error) {
	var n Node
	e := r.client.call(ctx, "PATCH", r.endpoint("/ownership"), query(p), o, &n)
	return n, e
}

func (r *Root) ThumbnailRaw(ctx context.Context, p string, width, height int) (*http.Response, error) {
	q := query(p)
	q.Set("width", strconv.Itoa(width))
	q.Set("height", strconv.Itoa(height))
	return r.client.Raw(ctx, "GET", r.endpoint("/thumbnail"), q, nil)
}
