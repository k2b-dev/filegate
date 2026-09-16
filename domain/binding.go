package domain

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Binding prevents a fresh or unrelated state store from collecting another
// store's versions as orphans. The filesystem adapter also holds a root lock.
func (r *Root) bindState() error {
	var id string
	e := r.State.Get("root/store-id", &id)
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	f, e := r.Files.Open(".filegate/state-id", os.O_RDONLY, 0)
	if e == nil {
		b, re := io.ReadAll(io.LimitReader(f, 128))
		f.Close()
		if re != nil {
			return re
		}
		if id == "" || strings.TrimSpace(string(b)) != id {
			return fmt.Errorf("root belongs to another state store; restore matching state before starting")
		}
		return nil
	}
	if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	for _, p := range []string{".filegate/versions", ".filegate/staging"} {
		d, e := r.Files.Open(p, os.O_RDONLY, 0)
		if e != nil {
			return e
		}
		names, e := d.Readdirnames(1)
		d.Close()
		if e != nil && !errors.Is(e, io.EOF) {
			return e
		}
		if len(names) > 0 {
			return fmt.Errorf("unbound root contains private data; refusing cleanup")
		}
	}
	if id == "" {
		id = newID()
		if e = r.State.Put("root/store-id", id); e != nil {
			return e
		}
	}
	f, e = r.Files.Open(".filegate/state-id", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	if _, e = f.WriteString(id + "\n"); e != nil {
		return e
	}
	if e = f.Sync(); e != nil {
		return e
	}
	return r.Files.Sync(".filegate")
}
