package httpadapter

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/disintegration/imaging"
	_ "golang.org/x/image/webp"

	"github.com/valentinkolb/filegate/domain"
	"github.com/valentinkolb/filegate/infra/cache"
	"github.com/valentinkolb/filegate/infra/jobs"
)

type thumbnailCacheItem struct {
	etag  string
	data  []byte
	mtime int64
}

type thumbnailer struct {
	svc *domain.Service

	cache          *cache.LRU[string, thumbnailCacheItem]
	maxSourceBytes int64
	maxPixels      int64
	scheduler      *jobs.Scheduler
}

func newThumbnailer(
	svc *domain.Service,
	cacheSize int,
	maxSourceBytes int64,
	maxPixels int64,
	scheduler *jobs.Scheduler,
) *thumbnailer {
	if maxSourceBytes <= 0 {
		maxSourceBytes = int64(64 * 1024 * 1024)
	}
	if maxPixels <= 0 {
		maxPixels = int64(40 * 1024 * 1024)
	}
	lruCache, _ := cache.NewLRU[string, thumbnailCacheItem](cacheSize)
	return &thumbnailer{
		svc:            svc,
		cache:          lruCache,
		maxSourceBytes: maxSourceBytes,
		maxPixels:      maxPixels,
		scheduler:      scheduler,
	}
}

func parseThumbSize(v string) (int, error) {
	if strings.TrimSpace(v) == "" {
		return 256, nil
	}
	size, err := strconv.Atoi(v)
	if err != nil {
		return 0, domain.ErrInvalidArgument
	}
	switch size {
	case 128, 256, 512:
		return size, nil
	default:
		return 0, domain.ErrInvalidArgument
	}
}

func etagFor(data []byte) string {
	sum := sha1.Sum(data)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func (t *thumbnailer) getCache(key string) (thumbnailCacheItem, bool) {
	return t.cache.Get(key)
}

func (t *thumbnailer) setCache(key string, item thumbnailCacheItem) {
	t.cache.Add(key, item)
}

func (t *thumbnailer) removeCache(key string) {
	t.cache.Remove(key)
}

func (t *thumbnailer) handleGet(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r.PathValue("id"))
	if !ok {
		return
	}
	size, err := parseThumbSize(r.URL.Query().Get("size"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "size must be one of: 128, 256, 512")
		return
	}

	meta, err := t.svc.GetFile(id)
	if err != nil {
		statusFromErr(w, err)
		return
	}
	if meta.Type != "file" {
		writeErr(w, http.StatusBadRequest, "thumbnails are only available for files")
		return
	}

	cacheKey := fmt.Sprintf("%s:%d", id.String(), size)
	if item, ok := t.getCache(cacheKey); ok {
		if item.mtime == meta.Mtime {
			if strings.TrimSpace(r.Header.Get("If-None-Match")) == item.etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			writeThumbResponse(w, item)
			return
		}
		t.removeCache(cacheKey)
	}

	absPath, err := t.svc.ResolveAbsPath(id)
	if err != nil {
		statusFromErr(w, err)
		return
	}

	item, err := t.generate(r.Context(), id.String(), meta.Mtime, cacheKey, absPath, size)
	if err != nil {
		switch {
		case errors.Is(err, jobs.ErrQueueFull):
			w.Header().Set("Retry-After", "1")
			writeErr(w, http.StatusServiceUnavailable, "thumbnail queue is full")
		case errors.Is(err, errUnsupportedImage):
			writeErr(w, http.StatusUnsupportedMediaType, "unsupported image file")
		case errors.Is(err, errImageTooLarge):
			writeErr(w, http.StatusRequestEntityTooLarge, "image is too large")
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			writeErr(w, http.StatusRequestTimeout, "request canceled")
		default:
			writeErr(w, http.StatusInternalServerError, "failed to generate thumbnail")
		}
		return
	}

	if strings.TrimSpace(r.Header.Get("If-None-Match")) == item.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeThumbResponse(w, item)
}

func writeThumbResponse(w http.ResponseWriter, item thumbnailCacheItem) {
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", strconv.Itoa(len(item.data)))
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("ETag", item.etag)
	w.Header().Set("Last-Modified", time.UnixMilli(item.mtime).UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(item.data)
}

var errUnsupportedImage = errors.New("unsupported image")
var errImageTooLarge = errors.New("image too large")

func (t *thumbnailer) generate(ctx context.Context, id string, mtime int64, cacheKey, absPath string, size int) (thumbnailCacheItem, error) {
	jobKey := fmt.Sprintf("thumbnail:%s:%d:%d", id, size, mtime)
	if t.scheduler != nil {
		val, err := t.scheduler.Do(ctx, jobKey, func(context.Context) (any, error) {
			return t.generateOne(absPath, size, mtime)
		})
		if err != nil {
			return thumbnailCacheItem{}, err
		}
		item, ok := val.(thumbnailCacheItem)
		if !ok {
			return thumbnailCacheItem{}, errors.New("thumbnail scheduler type assertion failed")
		}
		t.setCache(cacheKey, item)
		return item, nil
	}
	item, err := t.generateOne(absPath, size, mtime)
	if err != nil {
		return thumbnailCacheItem{}, err
	}
	t.setCache(cacheKey, item)
	return item, nil
}

func (t *thumbnailer) generateOne(absPath string, size int, mtime int64) (thumbnailCacheItem, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return thumbnailCacheItem{}, err
	}
	defer f.Close()

	if t.maxSourceBytes > 0 {
		info, err := f.Stat()
		if err != nil {
			return thumbnailCacheItem{}, err
		}
		if info.Size() > t.maxSourceBytes {
			return thumbnailCacheItem{}, errImageTooLarge
		}
	}

	if t.maxPixels > 0 {
		cfg, _, err := image.DecodeConfig(f)
		if err != nil {
			return thumbnailCacheItem{}, errUnsupportedImage
		}
		if cfg.Width <= 0 || cfg.Height <= 0 {
			return thumbnailCacheItem{}, errUnsupportedImage
		}
		width := int64(cfg.Width)
		height := int64(cfg.Height)
		if width > 0 && height > 0 && width > math.MaxInt64/height {
			return thumbnailCacheItem{}, errImageTooLarge
		}
		if width*height > t.maxPixels {
			return thumbnailCacheItem{}, errImageTooLarge
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return thumbnailCacheItem{}, err
		}
	}

	src, err := imaging.Decode(f, imaging.AutoOrientation(true))
	if err != nil {
		return thumbnailCacheItem{}, errUnsupportedImage
	}
	thumb := imaging.Thumbnail(src, size, size, imaging.Linear)

	var buf bytes.Buffer
	if err := imaging.Encode(&buf, thumb, imaging.JPEG, imaging.JPEGQuality(82)); err != nil {
		return thumbnailCacheItem{}, err
	}
	item := thumbnailCacheItem{
		etag:  etagFor(buf.Bytes()),
		data:  buf.Bytes(),
		mtime: mtime,
	}
	return item, nil
}

// schedulerStats exposes thumbnail worker-pool pressure for /v1/system/runtime.
func (t *thumbnailer) schedulerStats() jobs.Stats {
	if t == nil {
		return jobs.Stats{}
	}
	return t.scheduler.Stats()
}

// cacheStats exposes thumbnail cache occupancy and effectiveness.
func (t *thumbnailer) cacheStats() cache.Stats {
	if t == nil {
		return cache.Stats{}
	}
	return t.cache.Stats()
}
