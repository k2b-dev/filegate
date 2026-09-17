package httpadapter

import (
	"github.com/disintegration/imaging"
	"github.com/k2b-dev/filegate/v5/domain"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
)

var thumbnailSlots = make(chan struct{}, 4)

type thumbnailSize struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

func thumbnail(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
	return thumbnailContent(w, r, root, r.URL.Query().Get("path"), thumbnailSize{Width: paramInt(r, "width", 256), Height: paramInt(r, "height", 256)})
}

func thumbnailContent(w http.ResponseWriter, r *http.Request, root *domain.Root, p string, size thumbnailSize) error {
	select {
	case thumbnailSlots <- struct{}{}:
		defer func() { <-thumbnailSlots }()
	default:
		return errHTTP{503, "thumbnail_capacity"}
	}
	f, e := openThumbnail(root, p, size)
	if e != nil {
		return e
	}
	defer f.Close()
	img, e := imaging.Decode(io.LimitReader(f, 64<<20), imaging.AutoOrientation(true))
	if e != nil {
		return domain.ErrInvalid
	}
	out := imaging.Fit(img, size.Width, size.Height, imaging.Lanczos)
	w.Header().Set("Content-Type", "image/jpeg")
	return jpeg.Encode(w, out, &jpeg.Options{Quality: 85})
}

// Check the same source limits during issuance and rendering, without decoding
// all pixels just to issue a lease. Rendering validates the complete image.
func openThumbnail(root *domain.Root, p string, size thumbnailSize) (*os.File, error) {
	if size.Width < 1 || size.Height < 1 || size.Width > 2048 || size.Height > 2048 {
		return nil, domain.ErrInvalid
	}
	f, err := root.Open(p)
	if err != nil {
		return nil, err
	}
	if err = validateThumbnailSource(f); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func validateThumbnailSource(f *os.File) error {
	st, e := f.Stat()
	if e != nil {
		return e
	}
	if st.Size() > 64<<20 {
		return domain.ErrLimit
	}
	cfg, _, e := image.DecodeConfig(io.LimitReader(f, 64<<20))
	if e != nil {
		return domain.ErrInvalid
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 40000000 {
		return domain.ErrLimit
	}
	_, e = f.Seek(0, io.SeekStart)
	return e
}
