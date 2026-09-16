package httpadapter

import (
	"github.com/disintegration/imaging"
	"github.com/k2b-dev/filegate/v4/domain"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"
)

var thumbnailSlots = make(chan struct{}, 4)

func thumbnail(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
	select {
	case thumbnailSlots <- struct{}{}:
		defer func() { <-thumbnailSlots }()
	default:
		return errHTTP{503, "thumbnail_capacity"}
	}
	width, height := paramInt(r, "width", 256), paramInt(r, "height", 256)
	if width < 1 || height < 1 || width > 2048 || height > 2048 {
		return domain.ErrInvalid
	}
	f, e := root.Open(r.URL.Query().Get("path"))
	if e != nil {
		return e
	}
	defer f.Close()
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
	if _, e = f.Seek(0, io.SeekStart); e != nil {
		return e
	}
	img, e := imaging.Decode(io.LimitReader(f, 64<<20), imaging.AutoOrientation(true))
	if e != nil {
		return domain.ErrInvalid
	}
	out := imaging.Fit(img, width, height, imaging.Lanczos)
	w.Header().Set("Content-Type", "image/jpeg")
	return jpeg.Encode(w, out, &jpeg.Options{Quality: 85})
}
