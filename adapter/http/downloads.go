package httpadapter

import (
	"net/http"
	"path"

	"github.com/google/uuid"
	api "github.com/k2b-dev/filegate/v5/api/v1"
	"github.com/k2b-dev/filegate/v5/domain"
)

func versionContent(w http.ResponseWriter, r *http.Request, root *domain.Root, p, version string) error {
	f, err := root.OpenVersion(p, version)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
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
	return h.issueDownload(w, capability{Root: root.Config.Name, Path: p, Purpose: "version", Version: version}, q.ExpiresIn)
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
		return errHTTP{503, "thumbnail_capacity"}
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
	return h.issueDownload(w, capability{Root: root.Config.Name, Path: p, Purpose: "thumbnail", Thumbnail: &size}, q.ExpiresIn)
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
