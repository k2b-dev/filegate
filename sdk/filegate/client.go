// Package filegate provides the trusted-backend Go client for Filegate.
// Browser clients receive scoped direct URLs, never the daemon bearer token.
package filegate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	api "github.com/k2b-dev/filegate/v5/api/v1"
	"github.com/k2b-dev/filegate/v5/domain"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

type Node = api.Node
type Page = api.Page
type RootInfo = api.RootInfo
type WriteOptions = api.WriteOptions
type Precondition = api.Precondition
type ListingOptions = api.ListingOptions
type DownloadOptions = api.DownloadOptions
type Ownership = api.Ownership
type ExecutionIdentity = api.ExecutionIdentity
type ACLScope = api.ACLScope
type ACLPermissions = api.ACLPermissions
type ACLTag = api.ACLTag
type ACLEntry = api.ACLEntry
type ACL = api.ACL

const (
	SessionOpen      = api.SessionOpen
	SessionCommitted = api.SessionCommitted
	SessionAborted   = api.SessionAborted
	SessionExpired   = api.SessionExpired

	AccessACL      = api.AccessACL
	DefaultACL     = api.DefaultACL
	ACLOwner       = api.ACLOwner
	ACLUser        = api.ACLUser
	ACLOwningGroup = api.ACLOwningGroup
	ACLGroup       = api.ACLGroup
	ACLMask        = api.ACLMask
	ACLOther       = api.ACLOther
)

type Metadata = api.Metadata
type Version = api.Version
type Session = api.Session
type SessionStatus = api.SessionStatus
type SessionSegment = api.SessionSegment
type SessionSegmentPage = api.SessionSegmentPage
type SessionCreateOptions = api.SessionCreateOptions
type SessionState = api.SessionState
type SessionLease = api.SessionLease
type SessionLeaseRequest = api.SessionLeaseRequest
type DirectoryOptions = api.DirectoryOptions
type DirectoryACLs = api.DirectoryACLs
type ArchiveItem = api.ArchiveItem
type ArchiveLease = api.ArchiveLease
type DirectURL = api.DirectURL
type TransferResult = api.TransferResult
type TransferRequest = api.TransferRequest
type VersionCopyRequest = api.VersionCopyRequest
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
	base         string
	token        string
	http         *http.Client
	execution    string
	transferBase string
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
	return &Client{base: strings.TrimRight(base, "/"), token: token, http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// WithExecution returns an independent backend client bound to numeric Unix
// credentials. It never attaches the execution header to direct transfer URLs.
// Administrative operations do not accept an execution identity.
func (c *Client) WithExecution(identity ExecutionIdentity) (*Client, error) {
	normalized, err := domain.NormalizeExecution(&identity)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return nil, err
	}
	scoped := *c
	scoped.execution = string(encoded)
	return &scoped, nil
}

// WithExecution returns an independent root client bound to this identity.
func (r *Root) WithExecution(identity ExecutionIdentity) (*Root, error) {
	client, err := r.client.WithExecution(identity)
	if err != nil {
		return nil, err
	}
	return client.Root(r.name), nil
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
	if c.execution != "" {
		req.Header.Set("X-Filegate-Execution", c.execution)
	}
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
func listingQuery(p string, o ListingOptions) url.Values {
	q := query(p)
	if o.After != "" {
		q.Set("after", o.After)
	}
	if o.Limit != 0 {
		q.Set("limit", strconv.Itoa(o.Limit))
	}
	if o.MaxEntries != 0 {
		q.Set("maxEntries", strconv.Itoa(o.MaxEntries))
	}
	if o.Sort != "" {
		q.Set("sort", o.Sort)
	}
	if o.Order != "" {
		q.Set("order", o.Order)
	}
	if o.Type != "" {
		q.Set("type", o.Type)
	}
	return q
}
func (r *Root) List(ctx context.Context, p string, o ListingOptions) (Page, error) {
	var page Page
	err := r.client.call(ctx, "GET", r.endpoint("/entries"), listingQuery(p, o), nil, &page)
	return page, err
}
func (r *Root) Search(ctx context.Context, text, p string, o ListingOptions) (Page, error) {
	var page Page
	q := listingQuery(p, o)
	q.Set("q", text)
	err := r.client.call(ctx, "GET", r.endpoint("/search"), q, nil, &page)
	return page, err
}

func (r *Root) ContentRaw(ctx context.Context, p string) (*http.Response, error) {
	return r.client.Raw(ctx, "GET", r.endpoint("/content"), query(p), nil)
}
func (r *Root) Mkdir(ctx context.Context, p string, o DirectoryOptions) (Node, error) {
	var v Node
	e := r.client.call(ctx, "POST", r.endpoint("/directories"), nil, api.MkdirRequest{Path: p, DirectoryOptions: o}, &v)
	return v, e
}
func (r *Root) Remove(ctx context.Context, p string, recursive bool) error {
	q := query(p)
	q.Set("recursive", strconv.FormatBool(recursive))
	return r.client.call(ctx, "DELETE", r.endpoint("/files"), q, nil, nil)
}
func (r *Root) Transfer(ctx context.Context, req TransferRequest) (TransferResult, error) {
	var v TransferResult
	e := r.client.call(ctx, "POST", r.endpoint("/transfers"), nil, req, &v)
	return v, e
}
func (r *Root) DirectUpload(ctx context.Context, p string, size int64, o WriteOptions, expiresIn int) (DirectURL, error) {
	var v DirectURL
	e := r.client.call(ctx, "POST", r.endpoint("/uploads/direct"), nil, api.DirectRequest{Path: p, Size: size, WriteOptions: o, ExpiresIn: expiresIn}, &v)
	return v, e
}
func (r *Root) DirectDownload(ctx context.Context, p string, options DownloadOptions) (DirectURL, error) {
	var v DirectURL
	e := r.client.call(ctx, "POST", r.endpoint("/downloads/direct"), nil, api.DownloadRequest{Path: p, DownloadOptions: options}, &v)
	return v, e
}

// DirectVersionDownload issues a GET/HEAD lease for exactly one historical version.
func (r *Root) DirectVersionDownload(ctx context.Context, p, id string, options DownloadOptions) (DirectURL, error) {
	var v DirectURL
	e := r.client.call(ctx, "POST", r.endpoint("/versions/"+url.PathEscape(id)+"/downloads/direct"), nil, api.DownloadRequest{Path: p, DownloadOptions: options}, &v)
	return v, e
}

// DirectThumbnail issues a GET/HEAD lease with fixed dimensions (1–2048 each).
func (r *Root) DirectThumbnail(ctx context.Context, p string, width, height, expiresIn int) (DirectURL, error) {
	var v DirectURL
	e := r.client.call(ctx, "POST", r.endpoint("/thumbnail/direct"), nil, api.ThumbnailRequest{Path: p, ExpiresIn: expiresIn, Width: &width, Height: &height}, &v)
	return v, e
}
func (r *Root) Put(ctx context.Context, p string, body io.Reader, size int64, o WriteOptions) (Node, error) {
	u, e := r.DirectUpload(ctx, p, size, o, 0)
	if e != nil {
		return Node{}, e
	}
	transferURL, e := r.client.TransferURL(u.URL)
	if e != nil {
		return Node{}, e
	}
	return PutDirect(ctx, transferURL, body, size)
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

func (r *Root) CreateSession(ctx context.Context, p string, size int64, o WriteOptions, options SessionCreateOptions) (SessionCreated, error) {
	var v SessionCreated
	e := r.client.call(ctx, "POST", r.endpoint("/uploads/sessions"), nil, api.SessionRequest{Path: p, Size: size, WriteOptions: o, SessionCreateOptions: options}, &v)
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
func (s DirectSession) Status(ctx context.Context) (SessionStatus, error) {
	var v SessionStatus
	e := s.call(ctx, "GET", -1, nil, &v)
	return v, e
}
func (s DirectSession) Put(ctx context.Context, index int, body io.Reader) (SessionStatus, error) {
	var v SessionStatus
	e := s.call(ctx, "PUT", index, body, &v)
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

// GetACL reads an access or default ACL. An absent default ACL has empty entries.
func (r *Root) GetACL(ctx context.Context, p string, scope ACLScope) (ACL, error) {
	q := query(p)
	q.Set("scope", string(scope))
	var acl ACL
	e := r.client.call(ctx, "GET", r.endpoint("/acl"), q, nil, &acl)
	return acl, e
}

// SetACL replaces one ACL scope. Named entries require an explicit mask.
func (r *Root) SetACL(ctx context.Context, p string, scope ACLScope, acl ACL) (ACL, error) {
	q := query(p)
	q.Set("scope", string(scope))
	var result ACL
	e := r.client.call(ctx, "PUT", r.endpoint("/acl"), q, acl, &result)
	return result, e
}

// ClearDefaultACL removes future-child inheritance without changing existing children.
func (r *Root) ClearDefaultACL(ctx context.Context, p string) error {
	q := query(p)
	q.Set("scope", string(DefaultACL))
	return r.client.call(ctx, "DELETE", r.endpoint("/acl"), q, nil, nil)
}

func (r *Root) ThumbnailRaw(ctx context.Context, p string, width, height int) (*http.Response, error) {
	q := query(p)
	q.Set("width", strconv.Itoa(width))
	q.Set("height", strconv.Itoa(height))
	return r.client.Raw(ctx, "GET", r.endpoint("/thumbnail"), q, nil)
}

// Session returns backend status, including the immutable completion result.
func (r *Root) Session(ctx context.Context, id string) (Session, error) {
	var v Session
	e := r.client.call(ctx, "GET", r.endpoint("/uploads/sessions/"+url.PathEscape(id)), nil, nil, &v)
	return v, e
}
func (r *Root) SessionLease(ctx context.Context, id string, o SessionLeaseRequest) (SessionLease, error) {
	var v SessionLease
	e := r.client.call(ctx, "POST", r.endpoint("/uploads/sessions/"+url.PathEscape(id)+"/lease"), nil, o, &v)
	return v, e
}
func (r *Root) CommitSession(ctx context.Context, id string) (Node, error) {
	var v Node
	e := r.client.call(ctx, "POST", r.endpoint("/uploads/sessions/"+url.PathEscape(id)+"/commit"), nil, nil, &v)
	return v, e
}
func (r *Root) AbortSession(ctx context.Context, id string) error {
	return r.client.call(ctx, "DELETE", r.endpoint("/uploads/sessions/"+url.PathEscape(id)), nil, nil, nil)
}
func (c *Client) ArchiveLease(ctx context.Context, items []ArchiveItem, expiresIn int) (ArchiveLease, error) {
	var v ArchiveLease
	e := c.call(ctx, "POST", "/v1/downloads/archives", nil, api.ArchiveRequest{Items: items, ExpiresIn: expiresIn}, &v)
	return v, e
}

// ArchiveRaw streams the signed archive response without attaching backend credentials.
// Non-success responses are returned unchanged; the caller closes the response body.
func (c *Client) ArchiveRaw(ctx context.Context, lease ArchiveLease) (*http.Response, error) {
	transferURL, e := c.TransferURL(lease.URL)
	if e != nil {
		return nil, e
	}
	req, e := http.NewRequestWithContext(ctx, "POST", transferURL, strings.NewReader(url.Values{"manifest": {lease.Manifest}}.Encode()))
	if e != nil {
		return nil, e
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return directHTTP.Do(req)
}

// SessionSegments returns acknowledged chunks after an exclusive numeric index.
func (r *Root) SessionSegments(ctx context.Context, id string, after, limit int) (SessionSegmentPage, error) {
	var page SessionSegmentPage
	err := r.client.call(ctx, "GET", r.endpoint("/uploads/sessions/"+url.PathEscape(id)+"/segments"), url.Values{"after": {strconv.Itoa(after)}, "limit": {strconv.Itoa(limit)}}, nil, &page)
	return page, err
}
func (s DirectSession) Segments(ctx context.Context, after, limit int) (SessionSegmentPage, error) {
	var page SessionSegmentPage
	parsed, err := url.Parse(s.URL)
	if err != nil {
		return page, err
	}
	query := parsed.Query()
	query.Set("segments", "1")
	query.Set("after", strconv.Itoa(after))
	query.Set("limit", strconv.Itoa(limit))
	parsed.RawQuery = query.Encode()
	scoped := DirectSession{URL: parsed.String()}
	err = scoped.call(ctx, "GET", -1, nil, &page)
	return page, err
}

// RecursiveStats observes one bounded subtree; it does not update the root cache.
func (r *Root) RecursiveStats(ctx context.Context, p string, maxEntries int) (api.Stats, error) {
	var stats api.Stats
	q := query(p)
	q.Set("maxEntries", strconv.Itoa(maxEntries))
	err := r.client.call(ctx, "POST", r.endpoint("/stats/refresh"), q, nil, &stats)
	return stats, err
}

// WithTransferBaseURL selects an explicitly trusted internal transfer origin.
// Backend requests and public lease URLs remain unchanged.
func (c *Client) WithTransferBaseURL(origin string) (*Client, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("transfer base URL must be an HTTP(S) origin")
	}
	scoped := *c
	scoped.transferBase = strings.TrimRight(origin, "/")
	return &scoped, nil
}

var scopedTransferPath = regexp.MustCompile(`^/v1/direct/[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$`)

// TransferURL maps a Filegate-issued signed lease to the configured origin.
// It is not an endpoint for importing arbitrary remote URLs.
func (c *Client) TransferURL(leaseURL string) (string, error) {
	u, err := url.Parse(leaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" || !scopedTransferPath.MatchString(u.EscapedPath()) {
		return "", fmt.Errorf("expected a signed Filegate transfer URL")
	}
	if c.transferBase == "" {
		return leaseURL, nil
	}
	destination, _ := url.Parse(c.transferBase)
	u.Scheme, u.Host = destination.Scheme, destination.Host
	return u.String(), nil
}

// DownloadRaw sends no backend credentials and preserves non-success responses.
func (c *Client) DownloadRaw(ctx context.Context, lease DirectURL) (*http.Response, error) {
	target, err := c.TransferURL(lease.URL)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, "GET", target, nil)
	if err != nil {
		return nil, err
	}
	return directHTTP.Do(request)
}

// DirectSession uses the configured internal transfer origin for server uploads.
func (c *Client) DirectSession(lease SessionLease) (DirectSession, error) {
	target, err := c.TransferURL(lease.URL)
	return DirectSession{URL: target}, err
}

// Stats returns nil when no cached root observation is available.
func (r *Root) Stats(ctx context.Context) (*api.Stats, error) {
	var stats *api.Stats
	err := r.client.call(ctx, "GET", r.endpoint("/stats"), nil, nil, &stats)
	return stats, err
}

func (r *Root) TransferStatus(ctx context.Context, id string) (TransferResult, error) {
	var result TransferResult
	err := r.client.call(ctx, "GET", r.endpoint("/transfers/"+url.PathEscape(id)), nil, nil, &result)
	return result, err
}
func (r *Root) ResumeTransfer(ctx context.Context, id string) (TransferResult, error) {
	var result TransferResult
	err := r.client.call(ctx, "POST", r.endpoint("/transfers/"+url.PathEscape(id)+"/resume"), nil, nil, &result)
	return result, err
}
func (r *Root) AbandonTransfer(ctx context.Context, id string) (TransferResult, error) {
	var result TransferResult
	err := r.client.call(ctx, "POST", r.endpoint("/transfers/"+url.PathEscape(id)+"/abandon"), nil, nil, &result)
	return result, err
}
func (r *Root) CopyVersion(ctx context.Context, id string, request VersionCopyRequest) (Node, error) {
	var node Node
	err := r.client.call(ctx, "POST", r.endpoint("/versions/"+url.PathEscape(id)+"/copy"), nil, request, &node)
	return node, err
}
