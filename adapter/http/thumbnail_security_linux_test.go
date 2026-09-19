//go:build linux

package httpadapter

import (
	"context"
	"errors"
	"image"
	"image/png"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/k2b-dev/filegate/v5/domain"
	"github.com/k2b-dev/filegate/v5/infra/filesystem"
	"github.com/k2b-dev/filegate/v5/infra/pebble"
)

type deniedThumbnailFiles struct{ domain.Files }

func (deniedThumbnailFiles) Open(string, int, os.FileMode) (*os.File, error) {
	return nil, os.ErrPermission
}

func TestThumbnailDeniedOpenCannotJoinExistingRender(t *testing.T) {
	data := t.TempDir()
	files, err := filesystem.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	state, err := pebble.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	root, err := domain.NewRoot(domain.RootConfig{Name: "test", Path: data}, files, state, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.Create(filepath.Join(data, "image.png"))
	if err != nil {
		t.Fatal(err)
	}
	if err = png.Encode(source, image.NewRGBA(image.Rect(0, 0, 10, 10))); err != nil {
		t.Fatal(err)
	}
	source.Close()
	bounds := thumbnailSize{Width: 10, Height: 10}
	f, err := openThumbnail(root, "image.png", bounds)
	if err != nil {
		t.Fatal(err)
	}
	key, err := sourceThumbnailKey(root, f, bounds)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	flight, leader, err := thumbnailRenders.acquire(key)
	if err != nil || !leader {
		t.Fatal(err)
	}
	defer func() {
		thumbnailRenders.complete(key, flight, []byte("result"), nil)
		thumbnailRenders.wait(context.Background(), flight, true)
	}()
	root.Files = deniedThumbnailFiles{root.Files}
	recorder := httptest.NewRecorder()
	if err = thumbnailContent(recorder, httptest.NewRequest("GET", "/", nil), root, "image.png", bounds); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("denied open bypassed: %v", err)
	}
	if recorder.Body.Len() != 0 || flight.users != 1 {
		t.Fatal("denied caller received or joined a render")
	}
}
