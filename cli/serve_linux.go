//go:build linux

package cli

import (
	"context"
	"errors"
	"fmt"
	httpadapter "github.com/k2b-dev/filegate/v3/adapter/http"
	"github.com/k2b-dev/filegate/v3/domain"
	"github.com/k2b-dev/filegate/v3/infra/filesystem"
	"github.com/k2b-dev/filegate/v3/infra/pebble"
	"golang.org/x/sys/unix"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

func serve(parent context.Context, c Config) error {
	token, e := c.token()
	if e != nil {
		return e
	}
	if e = os.MkdirAll(c.StateDir, 0700); e != nil {
		return e
	}
	resolved, e := filepath.EvalSymlinks(c.StateDir)
	if e != nil {
		return e
	}
	for _, r := range c.Roots {
		if overlap(resolved, r.Path) {
			return fmt.Errorf("state directory overlaps root")
		}
	}
	lock, e := os.OpenFile(filepath.Join(c.StateDir, "LOCK"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); e != nil {
		return fmt.Errorf("another daemon owns state_dir: %w", e)
	}
	roots := []*domain.Root{}
	defer func() {
		for _, r := range roots {
			r.State.Close()
			r.Files.Close()
		}
	}()
	for _, r := range c.Roots {
		f, e := filesystem.Open(r.Path)
		if e != nil {
			return e
		}
		s, e := pebble.Open(filepath.Join(c.StateDir, "roots", r.Name))
		if e != nil {
			f.Close()
			return e
		}
		duration, _ := time.ParseDuration(r.Versioning.Cooldown)
		root, e := domain.NewRoot(domain.RootConfig{Name: r.Name, Path: r.Path, Index: r.Index, Versioning: domain.Versioning{Enabled: r.Versioning.Enabled, Cooldown: duration, Keep: *r.Versioning.Keep}}, f, s, c.maxBytes)
		if e != nil {
			s.Close()
			f.Close()
			return e
		}
		roots = append(roots, root)
	}
	h := httpadapter.New(roots, httpadapter.Options{Token: token, PublicURL: c.Server.PublicURL, Origins: c.Server.AllowedOrigins, Version: Version})
	srv := &http.Server{Addr: c.Server.Listen, Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Minute, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 64 << 10}
	listener, e := net.Listen("tcp", srv.Addr)
	if e != nil {
		return e
	}
	ctx, stop := signal.NotifyContext(parent, syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.Maintain(ctx)
			}
		}
	}()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(listener) }()
	select {
	case e = <-errCh:
	case <-ctx.Done():
	}
	stop()
	shutdown, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if se := srv.Shutdown(shutdown); se != nil {
		srv.Close()
		if e == nil {
			e = se
		}
	}
	h.Drain()
	wg.Wait()
	if errors.Is(e, http.ErrServerClosed) {
		return nil
	}
	return e
}
