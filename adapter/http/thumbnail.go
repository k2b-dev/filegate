package httpadapter

import (
	"bytes"
	"context"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"

	"github.com/disintegration/imaging"
	"github.com/k2b-dev/filegate/v5/domain"
)

var thumbnailSlots = make(chan struct{}, 4)
var thumbnailAdmissions = make(chan struct{}, 36)
var thumbnailRenders = thumbnailCoordinator{flights: make(map[thumbnailKey]*thumbnailFlight), slots: thumbnailSlots, waiters: make(chan struct{}, 32)}

const thumbnailOutputLimit = 16 << 20

type thumbnailSize struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

type thumbnailKey struct {
	root                    string
	device, inode           uint64
	size, modified, changed int64
	bounds                  thumbnailSize
}

type thumbnailFlight struct {
	done   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	users  int
	data   []byte
	err    error
}

type thumbnailCoordinator struct {
	mu      sync.Mutex
	flights map[thumbnailKey]*thumbnailFlight
	slots   chan struct{}
	waiters chan struct{}
}

func (c *thumbnailCoordinator) acquire(key thumbnailKey) (*thumbnailFlight, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if f := c.flights[key]; f != nil {
		if f.users == 0 {
			return nil, false, errHTTP{503, "thumbnail_capacity"}
		}
		select {
		case c.waiters <- struct{}{}:
		default:
			return nil, false, errHTTP{503, "thumbnail_capacity"}
		}
		f.users++
		return f, false, nil
	}
	select {
	case c.slots <- struct{}{}:
	default:
		return nil, false, errHTTP{503, "thumbnail_capacity"}
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := &thumbnailFlight{done: make(chan struct{}), ctx: ctx, cancel: cancel, users: 1}
	c.flights[key] = f
	return f, true, nil
}

func (c *thumbnailCoordinator) complete(key thumbnailKey, f *thumbnailFlight, data []byte, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f.data, f.err = data, err
	delete(c.flights, key)
	<-c.slots
	f.cancel()
	close(f.done)
}

func (c *thumbnailCoordinator) wait(ctx context.Context, f *thumbnailFlight, leader bool) ([]byte, error) {
	defer func() {
		c.mu.Lock()
		f.users--
		if f.users == 0 {
			f.cancel()
		}
		c.mu.Unlock()
		if !leader {
			<-c.waiters
		}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.done:
		return f.data, f.err
	}
}

func thumbnail(w http.ResponseWriter, r *http.Request, root *domain.Root) error {
	return thumbnailContent(w, r, root, r.URL.Query().Get("path"), thumbnailSize{Width: paramInt(r, "width", 256), Height: paramInt(r, "height", 256)})
}

func thumbnailCapacity(w http.ResponseWriter) error {
	w.Header().Set("Retry-After", "1")
	return errHTTP{503, "thumbnail_capacity"}
}

func thumbnailContent(w http.ResponseWriter, r *http.Request, root *domain.Root, p string, size thumbnailSize) error {
	select {
	case thumbnailAdmissions <- struct{}{}:
		defer func() { <-thumbnailAdmissions }()
	default:
		return thumbnailCapacity(w)
	}
	// Every caller opens and validates under its own execution identity before it
	// may join a render. In-flight results never bypass a denied source open.
	source, err := openThumbnail(root, p, size)
	if err != nil {
		return err
	}
	key, err := sourceThumbnailKey(root, source, size)
	if err != nil {
		source.Close()
		return err
	}
	flight, leader, err := thumbnailRenders.acquire(key)
	if err != nil {
		source.Close()
		return thumbnailCapacity(w)
	}
	if leader {
		go func() {
			data, renderErr := renderThumbnail(flight.ctx, source, size)
			if renderErr == nil {
				after, statErr := sourceThumbnailKey(root, source, size)
				if statErr != nil {
					renderErr = statErr
				} else if after != key {
					renderErr = domain.ErrConflict
				}
			}
			if closeErr := source.Close(); renderErr == nil {
				renderErr = closeErr
			}
			thumbnailRenders.complete(key, flight, data, renderErr)
		}()
	} else {
		source.Close()
	}
	data, err := thumbnailRenders.wait(r.Context(), flight, leader)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if r.Method == http.MethodHead {
		return nil
	}
	_, err = w.Write(data)
	return err
}

func sourceThumbnailKey(root *domain.Root, f *os.File, size thumbnailSize) (thumbnailKey, error) {
	st, err := f.Stat()
	if err != nil {
		return thumbnailKey{}, err
	}
	dev, ino, _, _, _ := root.Files.Identity(st)
	return thumbnailKey{root: root.Config.Name, device: dev, inode: ino, size: st.Size(), modified: st.ModTime().UnixNano(), changed: thumbnailChangeTime(st), bounds: size}, nil
}

type thumbnailReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r thumbnailReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

type thumbnailBuffer struct{ bytes.Buffer }

func (b *thumbnailBuffer) Write(p []byte) (int, error) {
	if len(p) > thumbnailOutputLimit-b.Len() {
		return 0, domain.ErrLimit
	}
	return b.Buffer.Write(p)
}

func renderThumbnail(ctx context.Context, f *os.File, size thumbnailSize) ([]byte, error) {
	// Freeze at most 64 MiB before parsing dimensions and decoding. An external
	// writer cannot replace an already-validated header with a larger image in
	// between these two decoder passes.
	source, err := io.ReadAll(thumbnailReader{ctx, io.LimitReader(f, (64<<20)+1)})
	if err != nil {
		return nil, err
	}
	if len(source) > 64<<20 {
		return nil, domain.ErrLimit
	}
	if err = validateThumbnailConfig(bytes.NewReader(source)); err != nil {
		return nil, err
	}
	img, err := imaging.Decode(thumbnailReader{ctx, bytes.NewReader(source)}, imaging.AutoOrientation(true))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, domain.ErrInvalid
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	out := imaging.Fit(img, size.Width, size.Height, imaging.Lanczos)
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	var buffer thumbnailBuffer
	if err = jpeg.Encode(&buffer, out, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	return buffer.Bytes(), ctx.Err()
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
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() > 64<<20 {
		return domain.ErrLimit
	}
	if err = validateThumbnailConfig(io.LimitReader(f, 64<<20)); err != nil {
		return err
	}
	_, err = f.Seek(0, io.SeekStart)
	return err
}

func validateThumbnailConfig(reader io.Reader) error {
	cfg, _, err := image.DecodeConfig(reader)
	if err != nil {
		return domain.ErrInvalid
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 40000000 {
		return domain.ErrLimit
	}
	return nil
}
