// Package httpadapter exposes authenticated root-scoped REST and signed transfers.
package httpadapter

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	api "github.com/k2b-dev/filegate/v4/api/v1"
	"github.com/k2b-dev/filegate/v4/domain"
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Options struct {
	Token     string
	PublicURL string
	Origins   []string
	Version   string
}
type Handler struct {
	roots            map[string]*domain.Root
	order            []string
	opts             Options
	mux              *http.ServeMux
	started          time.Time
	mu               sync.RWMutex
	maintenanceError string
	uploadSlots      chan struct{}
	draining         bool
	requests         sync.WaitGroup
}
type capability struct {
	Root         string              `json:"root"`
	Path         string              `json:"path"`
	Purpose      string              `json:"purpose"`
	Session      string              `json:"session,omitempty"`
	Size         int64               `json:"size"`
	Expires      int64               `json:"expires"`
	Nonce        string              `json:"nonce"`
	Options      domain.WriteOptions `json:"options"`
	Operations   []string            `json:"operations"`
	ManifestHash string              `json:"manifestHash,omitempty"`
}

func New(roots []*domain.Root, o Options) *Handler {
	h := &Handler{roots: map[string]*domain.Root{}, uploadSlots: make(chan struct{}, 16), opts: o, mux: http.NewServeMux(), started: time.Now()}
	for _, r := range roots {
		h.roots[r.Config.Name] = r
		h.order = append(h.order, r.Config.Name)
	}
	h.routes()
	return h
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	if h.draining {
		h.mu.Unlock()
		fail(w, errHTTP{503, "shutting_down"})
		return
	}
	h.requests.Add(1)
	h.mu.Unlock()
	defer h.requests.Done()
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	if origin := r.Header.Get("Origin"); origin != "" {
		for _, allowed := range h.opts.Origins {
			if origin == allowed {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Add("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Range")
				w.Header().Set("Access-Control-Expose-Headers", "Content-Length, Content-Range")
				break
			}
		}
	}
	if r.Method == "OPTIONS" {
		w.WriteHeader(204)
		return
	}
	if r.URL.Path != "/health" && !strings.HasPrefix(r.URL.Path, "/v1/direct/") {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if h.opts.Token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(h.opts.Token)) != 1 {
			fail(w, errHTTP{401, "unauthorized"})
			return
		}
	}
	h.mux.ServeHTTP(w, r)
}

type errHTTP struct {
	status int
	msg    string
}

func (e errHTTP) Error() string { return e.msg }
func fail(w http.ResponseWriter, e error) {
	status, code := 500, "internal_error"
	var he errHTTP
	var sizeError *http.MaxBytesError
	switch {
	case errors.As(e, &he):
		status, code = he.status, he.msg
	case errors.Is(e, domain.ErrSessionCommitted):
		status, code = 409, "session_committed"
	case errors.Is(e, domain.ErrSessionAborted):
		status, code = 409, "session_aborted"
	case errors.Is(e, domain.ErrSessionExpired):
		status, code = 410, "session_expired"
	case errors.Is(e, domain.ErrCrossDevice):
		status, code = 501, "unsupported_storage_layout"
	case errors.Is(e, domain.ErrInvalidACL):
		status, code = 400, "invalid_acl"
	case errors.Is(e, domain.ErrACLUnsupported):
		status, code = 501, "acl_not_supported"
	case errors.Is(e, domain.ErrInvalid):
		status, code = 400, "invalid_argument"
	case errors.As(e, &sizeError), errors.Is(e, domain.ErrLimit):
		status, code = 413, "limit_exceeded"
	case errors.Is(e, domain.ErrConflict), errors.Is(e, os.ErrExist):
		status, code = 409, "conflict"
	case errors.Is(e, os.ErrNotExist):
		status, code = 404, "not_found"
	case errors.Is(e, os.ErrPermission):
		status, code = 403, "forbidden"
	case errors.Is(e, domain.ErrDisabled):
		status, code = 409, "feature_disabled"
	case errors.Is(e, context.Canceled):
		status, code = 408, "canceled"
	}
	message := e.Error()
	switch code {
	case "internal_error":
		message = "file operation failed"
	case "unsupported_storage_layout":
		message = "the root staging directory and target must be on the same filesystem"
	case "invalid_acl":
		message = "invalid ACL scope, entries, IDs, permissions, or mask"
	case "acl_not_supported":
		message = "POSIX ACLs are not supported by this filesystem or mount"
	case "forbidden":
		message = "permission denied; check filesystem permissions, process privileges, and NFS export policy"
	}
	send(w, status, api.Error{Error: code, Message: message})
}
func send(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return domain.ErrInvalid
	}
	if e := d.Decode(&struct{}{}); !errors.Is(e, io.EOF) {
		return domain.ErrInvalid
	}
	return nil
}
func paramInt(r *http.Request, k string, d int) int {
	v := r.URL.Query().Get(k)
	if v == "" {
		return d
	}
	n, e := strconv.Atoi(v)
	if e != nil {
		return -1
	}
	return n
}
func (h *Handler) route(pattern string, fn func(http.ResponseWriter, *http.Request, *domain.Root) error) {
	h.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		root := h.roots[r.PathValue("root")]
		if root == nil {
			fail(w, os.ErrNotExist)
			return
		}
		if e := fn(w, r, root); e != nil {
			fail(w, e)
		}
	})
}
func (h *Handler) routes() {
	h.mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { send(w, 200, map[string]bool{"ready": true}) })
	h.mux.HandleFunc("GET /v1/system", func(w http.ResponseWriter, r *http.Request) {
		h.mu.RLock()
		defer h.mu.RUnlock()
		send(w, 200, api.System{Version: h.opts.Version, Started: h.started, UptimeSeconds: int64(time.Since(h.started).Seconds()), Ready: true, MaintenanceError: h.maintenanceError})
	})
	h.mux.HandleFunc("GET /v1/roots", func(w http.ResponseWriter, r *http.Request) {
		out := []api.RootInfo{}
		for _, name := range h.order {
			v, e := h.roots[name].Info()
			if e != nil {
				fail(w, e)
				return
			}
			out = append(out, v)
		}
		send(w, 200, out)
	})
	h.route("GET /v1/roots/{root}", func(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
		v, e := root.Info()
		if e == nil {
			send(w, 200, v)
		}
		return e
	})
	h.route("GET /v1/roots/{root}/index", func(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
		v, e := root.Info()
		if e == nil {
			send(w, 200, v.Index)
		}
		return e
	})
	h.route("GET /v1/roots/{root}/stats", func(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
		v, e := root.Info()
		if e == nil {
			send(w, 200, v.Stats)
		}
		return e
	})
	h.route("PATCH /v1/roots/{root}/ownership", func(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
		var o domain.Ownership
		if e := decode(w, r, &o); e != nil {
			return e
		}
		n, e := root.SetOwnership(r.URL.Query().Get("path"), &o)
		if e == nil {
			send(w, 200, n)
		}
		return e
	})
	h.route("GET /v1/roots/{root}/acl", func(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
		if r.URL.Query().Get("path") == "" {
			return domain.ErrInvalid
		}
		v, e := root.GetACL(r.URL.Query().Get("path"), domain.ACLScope(r.URL.Query().Get("scope")))
		if e == nil {
			send(w, 200, v)
		}
		return e
	})
	h.route("PUT /v1/roots/{root}/acl", func(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
		if r.URL.Query().Get("path") == "" {
			return domain.ErrInvalid
		}
		var acl api.ACL
		if e := decode(w, r, &acl); e != nil {
			return domain.ErrInvalidACL
		}
		v, e := root.SetACL(r.URL.Query().Get("path"), domain.ACLScope(r.URL.Query().Get("scope")), acl)
		if e == nil {
			send(w, 200, v)
		}
		return e
	})
	h.route("DELETE /v1/roots/{root}/acl", func(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
		if r.URL.Query().Get("path") == "" {
			return domain.ErrInvalid
		}
		if domain.ACLScope(r.URL.Query().Get("scope")) != domain.DefaultACL {
			return domain.ErrInvalidACL
		}
		if e := root.ClearDefaultACL(r.URL.Query().Get("path")); e != nil {
			return e
		}
		w.WriteHeader(204)
		return nil
	})
	h.route("GET /v1/roots/{root}/stat", func(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
		v, e := root.Stat(r.URL.Query().Get("path"))
		if e == nil {
			send(w, 200, v)
		}
		return e
	})
	h.route("GET /v1/roots/{root}/resolve", func(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
		v, e := root.Resolve(r.URL.Query().Get("id"))
		if e == nil {
			send(w, 200, v)
		}
		return e
	})
	h.route("GET /v1/roots/{root}/entries", func(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
		v, e := root.List(r.URL.Query().Get("path"), r.URL.Query().Get("after"), paramInt(r, "limit", 100))
		if e == nil {
			send(w, 200, v)
		}
		return e
	})
	h.route("GET /v1/roots/{root}/search", func(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
		v, e := root.Search(r.Context(), r.URL.Query().Get("q"), r.URL.Query().Get("path"), r.URL.Query().Get("after"), paramInt(r, "limit", 100), paramInt(r, "maxEntries", 100000))
		if e == nil {
			send(w, 200, v)
		}
		return e
	})
	h.route("GET /v1/roots/{root}/content", func(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
		return content(w, r, root, r.URL.Query().Get("path"))
	})
	h.route("POST /v1/roots/{root}/directories", func(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
		var q api.MkdirRequest
		if e := decode(w, r, &q); e != nil {
			return e
		}
		v, e := root.Mkdir(q.Path, q.DirectoryOptions)
		if e == nil {
			send(w, 201, v)
		}
		return e
	})
	h.route("DELETE /v1/roots/{root}/files", func(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
		e := root.Remove(r.URL.Query().Get("path"), r.URL.Query().Get("recursive") == "true")
		if e == nil {
			w.WriteHeader(204)
		}
		return e
	})
	h.route("POST /v1/roots/{root}/transfers", func(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
		var q api.TransferRequest
		if e := decode(w, r, &q); e != nil {
			return e
		}
		dst := h.roots[q.TargetRoot]
		if dst == nil {
			return os.ErrNotExist
		}
		n, e := domain.Transfer(r.Context(), root, q.Path, dst, q.TargetPath, q.Move, q.WriteOptions)
		if e == nil {
			send(w, 200, n)
		}
		return e
	})
	h.route("POST /v1/roots/{root}/index/rebuild", func(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
		e := root.Rebuild(r.Context())
		if e == nil {
			v, ie := root.Info()
			if ie != nil {
				return ie
			}
			send(w, 200, v.Index)
		}
		return e
	})
	h.route("POST /v1/roots/{root}/stats/refresh", func(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
		v, e := root.RefreshStats(r.Context(), paramInt(r, "maxEntries", 100000))
		if e == nil {
			send(w, 200, v)
		}
		return e
	})
	h.route("POST /v1/roots/{root}/versions/prune", func(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
		n, e := root.Prune(r.Context())
		if e == nil {
			send(w, 200, map[string]int{"deleted": n})
		}
		return e
	})
	h.route("GET /v1/roots/{root}/versions", func(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
		v, e := root.Versions(r.URL.Query().Get("path"))
		if e == nil {
			send(w, 200, v)
		}
		return e
	})
	h.route("POST /v1/roots/{root}/versions", func(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
		var q api.VersionRequest
		if e := decode(w, r, &q); e != nil {
			return e
		}
		v, e := root.Snapshot(r.URL.Query().Get("path"), q.Pinned, q.Metadata)
		if e == nil {
			send(w, 201, v)
		}
		return e
	})
	h.route("PATCH /v1/roots/{root}/versions/{version}", func(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
		var q api.VersionRequest
		if e := decode(w, r, &q); e != nil {
			return e
		}
		v, e := root.UpdateVersion(r.URL.Query().Get("path"), r.PathValue("version"), q.Pinned, q.Metadata)
		if e == nil {
			send(w, 200, v)
		}
		return e
	})
	h.route("DELETE /v1/roots/{root}/versions/{version}", func(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
		e := root.DeleteVersion(r.URL.Query().Get("path"), r.PathValue("version"))
		if e == nil {
			w.WriteHeader(204)
		}
		return e
	})
	h.route("POST /v1/roots/{root}/versions/{version}/restore", func(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
		v, e := root.Restore(r.URL.Query().Get("path"), r.PathValue("version"))
		if e == nil {
			send(w, 200, v)
		}
		return e
	})
	h.route("GET /v1/roots/{root}/versions/{version}/content", func(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
		f, e := root.OpenVersion(r.URL.Query().Get("path"), r.PathValue("version"))
		if e != nil {
			return e
		}
		defer f.Close()
		st, e := f.Stat()
		if e != nil {
			return e
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, path.Base(r.URL.Query().Get("path")), st.ModTime(), f)
		return nil
	})
	h.route("POST /v1/roots/{root}/uploads/direct", h.mintUpload)
	h.route("POST /v1/roots/{root}/downloads/direct", h.mintDownload)
	h.route("POST /v1/roots/{root}/uploads/sessions", h.createSession)
	h.mux.HandleFunc("/v1/direct/{token}", h.direct)
	h.route("GET /v1/roots/{root}/uploads/sessions/{session}", h.sessionStatus)
	h.route("POST /v1/roots/{root}/uploads/sessions/{session}/lease", h.renewSessionLease)
	h.route("POST /v1/roots/{root}/uploads/sessions/{session}/commit", h.commitSession)
	h.route("DELETE /v1/roots/{root}/uploads/sessions/{session}", h.abortSession)
	h.archiveRoutes()
	h.route("GET /v1/roots/{root}/thumbnail", thumbnail)
}
func content(w http.ResponseWriter, r *http.Request, root *domain.Root, p string) error {
	f, e := root.Open(p)
	if e != nil {
		return e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return e
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment")
	http.ServeContent(w, r, path.Base(p), st.ModTime(), f)
	return nil
}
func (h *Handler) sign(c capability) string {
	b, _ := json.Marshal(c)
	payload := base64.RawURLEncoding.EncodeToString(b)
	mac := hmac.New(sha256.New, []byte(h.opts.Token))
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (h *Handler) verify(token string) (capability, error) {
	var c capability
	if len(token) > 20000 {
		return c, domain.ErrInvalid
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return c, domain.ErrInvalid
	}
	mac := hmac.New(sha256.New, []byte(h.opts.Token))
	mac.Write([]byte(parts[0]))
	sig, e := base64.RawURLEncoding.DecodeString(parts[1])
	if e != nil || !hmac.Equal(sig, mac.Sum(nil)) {
		return c, errHTTP{401, "invalid_capability"}
	}
	b, e := base64.RawURLEncoding.DecodeString(parts[0])
	if e != nil {
		return c, domain.ErrInvalid
	}
	if e = json.Unmarshal(b, &c); e != nil {
		return c, domain.ErrInvalid
	}
	if c.Expires <= time.Now().Unix() {
		return c, errHTTP{401, "expired_capability"}
	}
	if !validOperations(c) {
		return c, errHTTP{401, "invalid_capability"}
	}
	return c, nil
}

const defaultLeaseSeconds = 60
const maxLeaseSeconds = 300

func leaseSeconds(seconds int) (int, error) {
	if seconds == 0 {
		seconds = defaultLeaseSeconds
	}
	if seconds < 1 || seconds > maxLeaseSeconds {
		return 0, domain.ErrInvalid
	}
	return seconds, nil
}

func leaseExpiry(seconds int) (time.Time, error) {
	seconds, e := leaseSeconds(seconds)
	if e != nil {
		return time.Time{}, e
	}
	return time.Unix(time.Now().Unix()+int64(seconds), 0), nil
}

func validOperations(c capability) bool {
	if len(c.Operations) == 0 {
		return false
	}
	seen := map[string]bool{}
	for _, operation := range c.Operations {
		if seen[operation] {
			return false
		}
		seen[operation] = true
		switch c.Purpose {
		case "upload":
			if operation != "write" {
				return false
			}
		case "download", "archive":
			if operation != "read" {
				return false
			}
		case "session":
			if operation != "status" && operation != "write" && operation != "abort" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func allowedMethod(c capability, method string) bool {
	operation := ""
	switch c.Purpose {
	case "upload":
		if method == http.MethodPut {
			operation = "write"
		}
	case "download":
		if method == http.MethodGet || method == http.MethodHead {
			operation = "read"
		}
	case "archive":
		if method == http.MethodPost {
			operation = "read"
		}
	case "session":
		switch method {
		case http.MethodGet:
			operation = "status"
		case http.MethodPut:
			operation = "write"
		case http.MethodDelete:
			operation = "abort"
		}
	}
	for _, allowed := range c.Operations {
		if operation == allowed {
			return true
		}
	}
	return false
}

func (h *Handler) url(c capability) string {
	return strings.TrimRight(h.opts.PublicURL, "/") + "/v1/direct/" + h.sign(c)
}
func (h *Handler) mintUpload(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
	var q api.DirectRequest
	if e := decode(w, r, &q); e != nil {
		return e
	}
	p, e := domain.CleanPath(q.Path)
	if e != nil || p == "." {
		return domain.ErrInvalid
	}
	if e = domain.ValidateOptions(q.WriteOptions); e != nil {
		return e
	}
	if q.Size < 0 || q.Size > root.MaxBytes {
		return domain.ErrLimit
	}
	expires, e := leaseExpiry(q.ExpiresIn)
	if e != nil {
		return e
	}
	c := capability{Root: root.Config.Name, Path: p, Purpose: "upload", Size: q.Size, Expires: expires.Unix(), Nonce: uuid.NewString(), Options: q.WriteOptions, Operations: []string{"write"}}
	send(w, 201, api.DirectURL{URL: h.url(c), Method: "PUT", Expires: time.Unix(c.Expires, 0)})
	return nil
}
func (h *Handler) mintDownload(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
	var q struct {
		Path      string `json:"path"`
		ExpiresIn int    `json:"expiresIn"`
	}
	if e := decode(w, r, &q); e != nil {
		return e
	}
	n, e := root.Stat(q.Path)
	if e != nil {
		return e
	}
	if n.Directory {
		return domain.ErrInvalid
	}
	expires, e := leaseExpiry(q.ExpiresIn)
	if e != nil {
		return e
	}
	c := capability{Root: root.Config.Name, Path: n.Path, Purpose: "download", Expires: expires.Unix(), Nonce: uuid.NewString(), Operations: []string{"read"}}
	send(w, 201, api.DirectURL{URL: h.url(c), Method: "GET", Expires: time.Unix(c.Expires, 0)})
	return nil
}
func (h *Handler) createSession(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
	var q api.SessionRequest
	if e := decode(w, r, &q); e != nil {
		return e
	}
	_, e := leaseSeconds(q.ExpiresIn)
	if e != nil {
		return e
	}
	s, e := root.CreateSession(q.Path, q.Size, q.WriteOptions)
	if e != nil {
		return e
	}
	lease, e := h.sessionLease(s, q.ExpiresIn, q.AllowAbort)
	if e != nil {
		return e
	}
	send(w, 201, api.SessionCreated{Session: s, Lease: lease})
	return nil
}

func (h *Handler) sessionLease(s domain.Session, seconds int, allowAbort bool) (api.SessionLease, error) {
	expires, e := leaseExpiry(seconds)
	if e != nil {
		return api.SessionLease{}, e
	}
	if expires.After(s.Expires) {
		expires = s.Expires
	}
	if expires.Unix() <= time.Now().Unix() {
		return api.SessionLease{}, domain.ErrSessionExpired
	}
	operations := []string{"status", "write"}
	if allowAbort {
		operations = append(operations, "abort")
	}
	c := capability{Root: s.Root, Session: s.ID, Purpose: "session", Expires: expires.Unix(), Nonce: uuid.NewString(), Operations: operations}
	return api.SessionLease{URL: h.url(c), Expires: time.Unix(c.Expires, 0), Operations: operations}, nil
}

func (h *Handler) sessionStatus(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
	s, e := root.Session(r.PathValue("session"))
	if e == nil {
		send(w, 200, s)
	}
	return e
}

func (h *Handler) renewSessionLease(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
	var q api.SessionLeaseRequest
	if e := decode(w, r, &q); e != nil {
		return e
	}
	_, e := leaseSeconds(q.ExpiresIn)
	if e != nil {
		return e
	}
	s, e := root.Session(r.PathValue("session"))
	if e != nil {
		return e
	}
	switch s.State {
	case "committed":
		return domain.ErrSessionCommitted
	case "aborted":
		return domain.ErrSessionAborted
	case "expired":
		return domain.ErrSessionExpired
	case "open":
	default:
		return domain.ErrConflict
	}
	lease, e := h.sessionLease(s, q.ExpiresIn, q.AllowAbort)
	if e != nil {
		return e
	}
	send(w, 201, lease)
	return nil
}

func (h *Handler) commitSession(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
	n, e := root.CommitSession(r.Context(), r.PathValue("session"))
	if e == nil {
		send(w, 200, n)
	}
	return e
}

func (h *Handler) abortSession(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
	e := root.AbortSession(r.PathValue("session"))
	if e == nil {
		w.WriteHeader(204)
	}
	return e
}

func publicSession(s domain.Session) api.SessionStatus {
	return api.SessionStatus{ID: s.ID, Root: s.Root, Size: s.Size, ChunkSize: s.ChunkSize,
		Expires: s.Expires, State: s.State, Segments: s.Segments, Received: s.Received,
		TerminalAt: s.TerminalAt, RetainUntil: s.RetainUntil}
}

func (h *Handler) direct(w http.ResponseWriter, r *http.Request) {
	c, e := h.verify(r.PathValue("token"))
	if e != nil {
		fail(w, e)
		return
	}
	if !allowedMethod(c, r.Method) {
		fail(w, errHTTP{403, "operation_not_allowed"})
		return
	}
	if c.Purpose == "archive" {
		if e := h.directArchive(w, r, c); e != nil {
			fail(w, e)
		}
		return
	}
	root := h.roots[c.Root]
	if root == nil {
		fail(w, os.ErrNotExist)
		return
	}
	if r.Method == "PUT" {
		select {
		case h.uploadSlots <- struct{}{}:
			defer func() { <-h.uploadSlots }()
		default:
			fail(w, errHTTP{503, "upload_capacity"})
			return
		}
	}
	switch c.Purpose {
	case "upload":
		if r.Method != "PUT" {
			e = errHTTP{405, "method_not_allowed"}
			break
		}
		if r.ContentLength >= 0 && r.ContentLength != c.Size {
			e = domain.ErrInvalid
			break
		}
		var n domain.Node
		n, e = root.Put(r.Context(), c.Path, &exactReader{r: http.MaxBytesReader(w, r.Body, c.Size), remaining: c.Size}, c.Options)
		if e == nil {
			send(w, 201, n)
		}
	case "download":
		if r.Method != "GET" && r.Method != "HEAD" {
			e = errHTTP{405, "method_not_allowed"}
			break
		}
		e = content(w, r, root, c.Path)
	case "session":
		switch r.Method {
		case "GET":
			var s domain.Session
			s, e = root.Session(c.Session)
			if e == nil {
				send(w, 200, publicSession(s))
			}
		case "PUT":
			var s domain.Session
			s, e = root.PutSegment(r.Context(), c.Session, paramInt(r, "segment", -1), r.Body)
			if e == nil {
				send(w, 200, publicSession(s))
			}
		case "DELETE":
			e = root.AbortSession(c.Session)
			if e == nil {
				w.WriteHeader(204)
			}
		default:
			e = errHTTP{405, "method_not_allowed"}
		}
	default:
		e = domain.ErrInvalid
	}
	if e != nil {
		fail(w, e)
	}
}

type exactReader struct {
	r         io.Reader
	remaining int64
}

func (r *exactReader) Read(b []byte) (int, error) {
	n, e := r.r.Read(b)
	r.remaining -= int64(n)
	if r.remaining < 0 {
		return n, domain.ErrLimit
	}
	if e == io.EOF && r.remaining != 0 {
		return n, domain.ErrInvalid
	}
	return n, e
}
func (h *Handler) Maintain(ctx context.Context) {
	errs := []string{}
	for _, name := range h.order {
		r := h.roots[name]
		if e := r.CleanupSessions(ctx); e != nil {
			errs = append(errs, fmt.Sprintf("%s uploads: %v", name, e))
		}
		if r.Config.Versioning.Enabled {
			if _, e := r.Prune(ctx); e != nil {
				errs = append(errs, fmt.Sprintf("%s versions: %v", name, e))
			}
		}
	}
	h.mu.Lock()
	h.maintenanceError = strings.Join(errs, "; ")
	h.mu.Unlock()
}

// Drain prevents state from closing while a handler still publishes a file.
func (h *Handler) Drain() { h.mu.Lock(); h.draining = true; h.mu.Unlock(); h.requests.Wait() }
