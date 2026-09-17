package httpadapter

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	api "github.com/k2b-dev/filegate/v4/api/v1"
	"github.com/k2b-dev/filegate/v4/domain"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const (
	archiveManifestLimit       = 128 << 10
	archiveItemLimit           = 1000
	archiveEntryLimit          = 10000
	archiveScanLimit           = 20000
	archiveDepthLimit          = 64
	archiveBytesLimit    int64 = 100 << 30
)

// This is a process-wide resource budget, shared by every HTTP handler.
var archiveSlots = make(chan struct{}, 4)

type archiveSelection struct {
	Root        string `json:"root"`
	Path        string `json:"path"`
	ArchivePath string `json:"archivePath"`
	Directory   bool   `json:"directory"`
}

type archiveEntry struct {
	root *domain.Root
	path string
	name string
	info os.FileInfo
}

func (h *Handler) archiveRoutes() {
	h.mux.HandleFunc("POST /v1/downloads/archives", func(w http.ResponseWriter, r *http.Request) {
		if e := h.mintArchive(w, r); e != nil {
			fail(w, e)
		}
	})
}

// ZIP paths use a portable subset: no ambiguous separators, drive names,
// traversal, control characters or Windows trailing-dot/space aliases.
func validArchiveName(name string) bool {
	if name == "" || !utf8.ValidString(name) || len(name) > 4096 || path.Clean(name) != name || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\:") {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	for _, part := range strings.Split(name, "/") {
		if part == "." || part == ".." || strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") || strings.ContainsAny(part, "<>\"|?*") {
			return false
		}
		base := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
		if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || (len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9') {
			return false
		}
	}
	return true
}

func archiveNameKey(name string) string { return cases.Fold().String(norm.NFC.String(name)) }

func validateArchiveSelection(items []archiveSelection) error {
	if len(items) == 0 {
		return domain.ErrInvalid
	}
	if len(items) > archiveItemLimit {
		return domain.ErrLimit
	}
	for i, item := range items {
		p, e := domain.CleanPath(item.Path)
		if e != nil || p != item.Path || item.Root == "" || !validArchiveName(item.ArchivePath) {
			return domain.ErrInvalid
		}
		name := archiveNameKey(item.ArchivePath)
		for _, prev := range items[:i] {
			other := archiveNameKey(prev.ArchivePath)
			if name == other || strings.HasPrefix(name, other+"/") || strings.HasPrefix(other, name+"/") {
				return domain.ErrConflict
			}
		}
	}
	return nil
}

func (h *Handler) mintArchive(w http.ResponseWriter, r *http.Request) error {
	var q api.ArchiveRequest
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, archiveManifestLimit))
	d.DisallowUnknownFields()
	if e := d.Decode(&q); e != nil {
		return domain.ErrInvalid
	}
	if e := d.Decode(&struct{}{}); e != io.EOF {
		return domain.ErrInvalid
	}
	if len(q.Items) > archiveItemLimit {
		return domain.ErrLimit
	}
	items := make([]archiveSelection, len(q.Items))
	for i, item := range q.Items {
		if item.Path == "" {
			return domain.ErrInvalid
		}
		p, e := domain.CleanPath(item.Path)
		if e != nil {
			return e
		}
		items[i] = archiveSelection{Root: item.Root, Path: p, ArchivePath: item.ArchivePath}
	}
	if e := validateArchiveSelection(items); e != nil {
		return e
	}
	for i := range items {
		if e := r.Context().Err(); e != nil {
			return e
		}
		root := h.roots[items[i].Root]
		if root == nil {
			return os.ErrNotExist
		}
		st, e := root.ArchiveStat(items[i].Path)
		if e != nil {
			return e
		}
		items[i].Directory = st.IsDir()
	}
	expires, e := leaseExpiry(q.ExpiresIn)
	if e != nil {
		return e
	}
	manifest, e := json.Marshal(items)
	if e != nil {
		return e
	}
	if len(manifest) > archiveManifestLimit {
		return domain.ErrLimit
	}
	hash := sha256.Sum256(manifest)
	c := capability{Purpose: "archive", ManifestHash: hex.EncodeToString(hash[:]), Operations: []string{"read"}, Expires: expires.Unix(), Nonce: uuid.NewString()}
	send(w, http.StatusCreated, api.ArchiveLease{URL: h.url(c), Method: http.MethodPost, Expires: time.Unix(c.Expires, 0), Manifest: string(manifest)})
	return nil
}

func (h *Handler) archiveEntries(ctx context.Context, items []archiveSelection) ([]archiveEntry, error) {
	if e := validateArchiveSelection(items); e != nil {
		return nil, e
	}
	entries := make([]archiveEntry, 0)
	names := make(map[string]bool)
	var bytes int64
	scanBudget := archiveScanLimit
	for _, item := range items {
		root := h.roots[item.Root]
		if root == nil {
			return nil, os.ErrNotExist
		}
		e := root.WalkArchive(ctx, item.Path, item.Directory, archiveDepthLimit, &scanBudget, func(p string, st os.FileInfo) error {
			if len(entries) >= archiveEntryLimit {
				return domain.ErrLimit
			}
			name := item.ArchivePath
			if p != item.Path {
				rel := p
				if item.Path != "." {
					rel = strings.TrimPrefix(p, item.Path+"/")
				}
				name += "/" + rel
			}
			if !validArchiveName(name) {
				return domain.ErrInvalid
			}
			key := archiveNameKey(name)
			if _, ok := names[key]; ok {
				return domain.ErrConflict
			}
			for parent := path.Dir(key); parent != "."; parent = path.Dir(parent) {
				if directory, ok := names[parent]; ok && !directory {
					return domain.ErrConflict
				}
			}
			names[key] = st.IsDir()
			if !st.IsDir() {
				if st.Size() < 0 || st.Size() > archiveBytesLimit-bytes {
					return domain.ErrLimit
				}
				bytes += st.Size()
			}
			entries = append(entries, archiveEntry{root: root, path: p, name: name, info: st})
			return nil
		})
		if e != nil {
			return nil, e
		}
	}
	return entries, nil
}

func (h *Handler) directArchive(w http.ResponseWriter, r *http.Request, c capability) error {
	if r.Method != http.MethodPost {
		return errHTTP{405, "method_not_allowed"}
	}
	if len(c.Operations) != 1 || c.Operations[0] != "read" {
		return errHTTP{403, "operation_not_allowed"}
	}
	mediaType, _, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if e != nil || mediaType != "application/x-www-form-urlencoded" {
		return errHTTP{415, "unsupported_media_type"}
	}
	r.Body = http.MaxBytesReader(w, r.Body, 512<<10)
	if e = r.ParseForm(); e != nil {
		return domain.ErrInvalid
	}
	values := r.PostForm["manifest"]
	if len(r.PostForm) != 1 || len(values) != 1 {
		return domain.ErrInvalid
	}
	manifest := values[0]
	if len(manifest) > archiveManifestLimit {
		return domain.ErrLimit
	}
	hash := sha256.Sum256([]byte(manifest))
	if hex.EncodeToString(hash[:]) != c.ManifestHash {
		return errHTTP{403, "invalid_archive_manifest"}
	}
	var items []archiveSelection
	if e = json.Unmarshal([]byte(manifest), &items); e != nil {
		return domain.ErrInvalid
	}
	select {
	case archiveSlots <- struct{}{}:
		defer func() { <-archiveSlots }()
	default:
		return errHTTP{503, "archive_capacity"}
	}
	entries, e := h.archiveEntries(r.Context(), items)
	if e != nil {
		return e
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="files.zip"`)
	w.WriteHeader(http.StatusOK)
	if e = streamArchive(r.Context(), w, entries); e != nil {
		// A partial ZIP must never be completed or followed by a JSON error body.
		panic(http.ErrAbortHandler)
	}
	return nil
}

type archiveReader struct {
	ctx context.Context
	r   io.Reader
}

func (r archiveReader) Read(p []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.r.Read(p)
}

func streamArchive(ctx context.Context, out io.Writer, entries []archiveEntry) error {
	zw := zip.NewWriter(out)
	for _, entry := range entries {
		if e := ctx.Err(); e != nil {
			return e
		}
		header := &zip.FileHeader{Name: entry.name, Method: zip.Store, Modified: entry.info.ModTime()}
		header.SetMode(entry.info.Mode())
		if entry.info.IsDir() {
			header.Name += "/"
			if _, e := zw.CreateHeader(header); e != nil {
				return e
			}
			continue
		}
		f, e := entry.root.Open(entry.path)
		if e != nil {
			return e
		}
		e = func() error {
			defer f.Close()
			st, e := f.Stat()
			if e != nil {
				return e
			}
			if !st.Mode().IsRegular() || st.Size() != entry.info.Size() {
				return domain.ErrConflict
			}
			w, e := zw.CreateHeader(header)
			if e != nil {
				return e
			}
			r := archiveReader{ctx: ctx, r: f}
			if _, e := io.CopyN(w, r, st.Size()); e != nil {
				return e
			}
			var extra [1]byte
			if _, e := r.Read(extra[:]); e != io.EOF {
				return domain.ErrConflict
			}
			return nil
		}()
		if e != nil {
			return e
		}
	}
	return zw.Close()
}
