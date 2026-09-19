package httpadapter

import (
	"fmt"
	"net/http"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	api "github.com/k2b-dev/filegate/v6/api/v1"
	"github.com/k2b-dev/filegate/v6/domain"
)

func versionContent(w http.ResponseWriter, r *http.Request, root *domain.Root, p, version, fileName string) error {
	f, err := root.OpenVersion(p, version)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	name := defaultDownloadName(p)
	if fileName != "" {
		name = fileName
	}
	if err := setDownloadDisposition(w, name); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, path.Base(p), st.ModTime(), f)
	return nil
}

func (h *Handler) mintVersionDownload(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
	var q api.DownloadRequest
	if err := decode(w, r, &q); err != nil {
		return err
	}
	if _, err := leaseSeconds(q.ExpiresIn); err != nil {
		return err
	}
	if q.FileName != "" {
		if err := ValidateDownloadName(q.FileName); err != nil {
			return err
		}
	}
	p, err := domain.CleanPath(q.Path)
	if err != nil {
		return err
	}
	version := r.PathValue("version")
	f, err := root.OpenVersion(p, version)
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return h.issueDownload(w, capability{Execution: root.Execution(), Root: root.Config.Name, Path: p, Purpose: "version", Version: version, FileName: q.FileName}, q.ExpiresIn)
}

func (h *Handler) mintThumbnail(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
	var q api.ThumbnailRequest
	if err := decode(w, r, &q); err != nil {
		return err
	}
	if _, err := leaseSeconds(q.ExpiresIn); err != nil {
		return err
	}
	select {
	case thumbnailSlots <- struct{}{}:
		defer func() { <-thumbnailSlots }()
	default:
		return thumbnailCapacity(w)
	}
	size := thumbnailSize{Width: 256, Height: 256}
	if q.Width != nil {
		size.Width = *q.Width
	}
	if q.Height != nil {
		size.Height = *q.Height
	}
	p, err := domain.CleanPath(q.Path)
	if err != nil {
		return err
	}
	f, err := openThumbnail(root, p, size)
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return h.issueDownload(w, capability{Execution: root.Execution(), Root: root.Config.Name, Path: p, Purpose: "thumbnail", Thumbnail: &size}, q.ExpiresIn)
}

// Start the lease after target validation, which may wait on filesystem work.
func (h *Handler) issueDownload(w http.ResponseWriter, c capability, seconds int) error {
	expires, err := leaseExpiry(seconds)
	if err != nil {
		return err
	}
	c.Expires, c.Nonce, c.Operations = expires.Unix(), uuid.NewString(), []string{"read"}
	send(w, http.StatusCreated, api.DirectURL{URL: h.url(c), Method: "GET", Expires: expires})
	return nil
}

// ValidateDownloadName validates one presentation filename, never a filesystem
// path. Empty optional request fields select the original path's basename.
func ValidateDownloadName(name string) error {
	if name == "" || name == "." || name == ".." || len(name) > 255 || !utf8.ValidString(name) || strings.ContainsAny(name, "/\\") {
		return domain.ErrInvalid
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return domain.ErrInvalid
		}
	}
	return nil
}

func setDownloadDisposition(w http.ResponseWriter, name string) error {
	if err := ValidateDownloadName(name); err != nil {
		return err
	}
	var fallback, encoded strings.Builder
	for _, r := range name {
		if r >= 32 && r <= 126 && r != '"' && r != '\\' {
			fallback.WriteRune(r)
		} else {
			fallback.WriteByte('_')
		}
	}
	const hex = "0123456789ABCDEF"
	for i := 0; i < len(name); i++ {
		b := name[i]
		if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.ContainsRune("!#$&+-.^_`|~", rune(b)) {
			encoded.WriteByte(b)
		} else {
			encoded.WriteByte('%')
			encoded.WriteByte(hex[b>>4])
			encoded.WriteByte(hex[b&15])
		}
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"; filename*=UTF-8''%s", fallback.String(), encoded.String()))
	return nil
}

// defaultDownloadName preserves legitimate Unix names without allowing control
// characters or path separators in a browser's suggested destination filename.
func defaultDownloadName(p string) string {
	name := strings.ToValidUTF8(path.Base(p), "_")
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '/' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	for len(name) > 255 {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	if name == "" || name == "." || name == ".." {
		return "download"
	}
	return name
}
