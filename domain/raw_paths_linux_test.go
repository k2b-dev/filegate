//go:build linux

package domain_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/k2b-dev/filegate/v7/domain"
	"github.com/k2b-dev/filegate/v7/infra/filesystem"
	"github.com/k2b-dev/filegate/v7/infra/pebble"
)

type rawPathState struct {
	domain.State
	failFamily string
}

func (s *rawPathState) Batch(cs []domain.Change) error {
	for _, c := range cs {
		if s.failFamily != "" && c.Delete && strings.HasPrefix(c.Key, s.failFamily) {
			s.failFamily = ""
			return errors.New("injected finalization failure")
		}
	}
	return s.State.Batch(cs)
}
func rawPathRoot(t *testing.T, name string) (*domain.Root, *rawPathState) {
	t.Helper()
	data := t.TempDir()
	f, err := filesystem.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	s, err := pebble.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	state := &rawPathState{State: s}
	r, err := domain.NewRoot(domain.RootConfig{Name: name, Path: data, Index: true, Managed: true, Versioning: domain.Versioning{Enabled: true}}, f, state, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return r, state
}
func rawRestart(t *testing.T, r *domain.Root, s domain.State) (*domain.Root, error) {
	t.Helper()
	if err := r.Files.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := filesystem.Open(r.Config.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return domain.NewRoot(r.Config, f, s, 1<<20)
}
func rawPut(t *testing.T, r *domain.Root, p, content string) domain.Node {
	t.Helper()
	n, err := r.Put(context.Background(), p, strings.NewReader(content), domain.WriteOptions{OnConflict: "overwrite"})
	if err != nil {
		t.Fatal(err)
	}
	return n
}
func rawIndexPaths(t *testing.T, r *domain.Root) map[string]string {
	t.Helper()
	var gen string
	if err := r.State.Get("index/generation", &gen); err != nil {
		t.Fatal(err)
	}
	rows := map[string]string{}
	prefix := "i/" + gen + "/"
	if err := r.State.Scan(prefix, func(k string, _ []byte) error { rows[strings.TrimPrefix(k, prefix)] = k; return nil }); err != nil {
		t.Fatal(err)
	}
	return rows
}
func TestRawPathSubtreeMoveDeletePreservesHistory(t *testing.T) {
	r, s := rawPathRoot(t, "files")
	names := []string{"\xff.pdf", "mid-\xfe.pdf", "�.pdf", "mid-�.pdf"}
	ids := map[string]string{}
	for i, name := range names {
		n := rawPut(t, r, "a/"+name, fmt.Sprint(i))
		ids[name] = n.ID
		// Fixed history dates avoid retention depending on the day this runs.
		versionID := fmt.Sprintf("history-%d", i)
		blob, err := r.Files.Open(".filegate/versions/"+versionID, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := blob.WriteString(name)
		closeErr := blob.Close()
		if writeErr != nil || closeErr != nil {
			t.Fatal(writeErr, closeErr)
		}
		if err := s.Put("v/"+n.ID+"/"+versionID, domain.Version{ID: versionID, FileID: n.ID, Size: int64(len(name)), Created: time.Unix(1000, 0).UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Move("a", "b"); err != nil {
		t.Fatal(err)
	}
	rows := rawIndexPaths(t, r)
	for _, name := range names {
		if rows["b/"+name] == "" || rows["a/"+name] != "" {
			t.Errorf("incorrect moved rows for %q: %q", name, rows)
		}
	}
	if _, err := r.Mkdir("a", domain.DirectoryOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.Remove("a", true); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		n, err := r.Resolve(ids[name])
		if err != nil || n.Path != "b/"+name {
			t.Errorf("identity for %q: %+v %v", name, n, err)
		}
		vs, err := r.Versions("b/" + name)
		if err != nil || len(vs) != 1 || vs[0].FileID != ids[name] {
			t.Errorf("history for %q: %+v %v", name, vs, err)
			continue
		}
		blob, err := r.OpenVersion("b/"+name, vs[0].ID)
		if err != nil {
			t.Errorf("history blob for %q: %v", name, err)
			continue
		}
		content, readErr := io.ReadAll(blob)
		closeErr := blob.Close()
		if readErr != nil || closeErr != nil || string(content) != name {
			t.Errorf("history content for %q: %q %v %v", name, content, readErr, closeErr)
		}
	}
}
func TestRawPathIdentityAndIndexReplacement(t *testing.T) {
	r, _ := rawPathRoot(t, "files")
	for _, p := range []string{"\xff.pdf", "mid-\xfe.pdf", "�.pdf", "mid-�.pdf"} {
		n := rawPut(t, r, p, "a")
		got, err := r.Resolve(n.ID)
		if err != nil || got.Path != p {
			t.Errorf("resolve %q: %+v %v", p, got, err)
		}
		rawPut(t, r, p, "longer")
	}
	for _, sortBy := range []string{"path", "name", "size", "modified"} {
		page, err := r.List(context.Background(), ".", domain.ListingOptions{Sort: sortBy, Limit: 100})
		if err != nil || len(page.Items) != 4 {
			t.Errorf("%s stale secondary rows: %d %v", sortBy, len(page.Items), err)
		}
	}
}
func TestRawPathRecovery(t *testing.T) {
	for _, kind := range []string{"publication", "move", "delete"} {
		t.Run(kind, func(t *testing.T) {
			r, s := rawPathRoot(t, "files")
			p := "dir-\xff/mid-\xfe.pdf"
			if kind == "publication" {
				if _, err := r.Mkdir("dir-\xff", domain.DirectoryOptions{}); err != nil {
					t.Fatal(err)
				}
				s.failFamily = "pending/"
				if _, err := r.Put(context.Background(), p, strings.NewReader("data"), domain.WriteOptions{}); err == nil {
					t.Fatal("failure not injected")
				}
			} else {
				rawPut(t, r, p, "data")
				s.failFamily = "mutation/"
				var err error
				if kind == "move" {
					_, err = r.Move("dir-\xff", "moved-\xfe")
					p = "moved-\xfe/mid-\xfe.pdf"
				} else {
					err = r.Remove("dir-\xff", true)
				}
				if err == nil {
					t.Fatal("failure not injected")
				}
			}
			// NewRoot exercises durable JSON decoding and startup reconciliation.
			restarted, err := rawRestart(t, r, s)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "delete" {
				if rows := rawIndexPaths(t, restarted); len(rows) != 1 {
					t.Fatalf("deleted rows: %q", rows)
				}
				return
			}
			n, err := restarted.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			got, err := restarted.Resolve(n.ID)
			if err != nil || got.Path != p || rawIndexPaths(t, restarted)[p] == "" {
				t.Fatalf("recovery lost %q: %+v %v", p, got, err)
			}
			if n.Revision == "" {
				t.Fatal("missing recovered managed revision")
			}
		})
	}
}
func TestRawPathTreePublication(t *testing.T) {
	r, _ := rawPathRoot(t, "files")
	for _, name := range []string{"\xff.pdf", "mid-\xfe.pdf", "�.pdf", "mid-�.pdf"} {
		rawPut(t, r, "a/"+name, "data")
	}
	if _, err := domain.Transfer(context.Background(), r, "a", r, "copy-\xff", false, domain.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	rows := rawIndexPaths(t, r)
	for _, name := range []string{"\xff.pdf", "mid-\xfe.pdf", "�.pdf", "mid-�.pdf"} {
		p := "copy-\xff/" + name
		if rows[p] == "" {
			t.Errorf("missing manifest row %q", p)
		}
		n, err := r.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		got, err := r.Resolve(n.ID)
		if err != nil || got.Path != p {
			t.Errorf("copied identity: %+v %v", got, err)
		}
	}
}
func TestRawPathSessionTargetAndIdempotency(t *testing.T) {
	for _, content := range []string{"", "data"} {
		t.Run(fmt.Sprintf("bytes=%d", len(content)), func(t *testing.T) {
			r, _ := rawPathRoot(t, "files")
			session, err := r.CreateSession("target-\xff", int64(len(content)), domain.WriteOptions{}, "request")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.CreateSession("target-\xfe", int64(len(content)), domain.WriteOptions{}, "request"); !errors.Is(err, domain.ErrIdempotencyConflict) {
				t.Errorf("raw targets collide: %v", err)
			}
			if content != "" {
				if _, err := r.PutSegment(context.Background(), session.ID, 0, strings.NewReader(content)); err != nil {
					t.Fatal(err)
				}
			}
			n, err := r.CommitSession(context.Background(), session.ID)
			if err != nil || n.Path != "target-\xff" {
				t.Fatalf("commit: %+v %v", n, err)
			}
			stored, err := r.Session(session.ID)
			if err != nil || stored.Path != "target-\xff" {
				t.Errorf("terminal path: %+v %v", stored, err)
			}
			if _, err := r.Files.Stat("target-\xff"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestRawPathSessionPublicationRecovery(t *testing.T) {
	r, s := rawPathRoot(t, "files")
	session, err := r.CreateSession("target-\xff", 0, domain.WriteOptions{}, "")
	if err != nil {
		t.Fatal(err)
	}
	s.failFamily = "pending/"
	if _, err := r.CommitSession(context.Background(), session.ID); err == nil {
		t.Fatal("failure not injected")
	}
	restarted, err := rawRestart(t, r, s)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := restarted.Session(session.ID)
	if err != nil || receipt.Path != "target-\xff" || receipt.State != domain.SessionCommitted || receipt.Result == nil {
		t.Fatalf("recovered receipt: %+v %v", receipt, err)
	}
	if _, err := restarted.Files.Stat("target-\xff"); err != nil {
		t.Fatal(err)
	}
}
func TestRawPathRecoverableTransfer(t *testing.T) {
	src, _ := rawPathRoot(t, "source")
	dst, _ := rawPathRoot(t, "destination")
	rawPut(t, src, "source-\xff", "data")
	id := uuid.MustParse("018bcfe5-6800-7000-8000-000000000001").String()
	result, err := domain.TransferMove(context.Background(), src, "source-\xff", dst, "target-\xfe", domain.WriteOptions{}, id)
	if err != nil || result.State != domain.TransferCompleted {
		t.Fatalf("transfer: %+v %v", result, err)
	}
	if _, err := src.Files.Stat("source-\xff"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source remains: %v", err)
	}
	if _, err := dst.Files.Stat("target-\xfe"); err != nil {
		t.Fatal(err)
	}
}
func TestRawPathIndexFormatUpgrade(t *testing.T) {
	r, s := rawPathRoot(t, "files")
	n := rawPut(t, r, "raw-\xff", "data")
	if err := s.Put("v/"+n.ID+"/history", domain.Version{ID: "history", FileID: n.ID, Created: time.Unix(1000, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	const sessionID = "018bcfe5-6800-7000-8000-000000000002"
	if err := s.Put("session/"+sessionID, domain.Session{ID: sessionID, State: domain.SessionOpen, Path: "plain", Expires: time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), ChunkSize: domain.ChunkSize}); err != nil {
		t.Fatal(err)
	}
	var gen string
	if err := s.Get("index/generation", &gen); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"i/" + gen + "/orphan", "q/" + gen + "/orphan"} {
		if err := s.Put(key, domain.Node{Path: "orphan"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Put("index/format", 2); err != nil {
		t.Fatal(err)
	}
	restarted, err := rawRestart(t, r, s)
	if err != nil {
		t.Fatal(err)
	}
	var format int
	if err := s.Get("index/format", &format); err != nil || format != 3 {
		t.Errorf("format=%d: %v", format, err)
	}
	for _, key := range []string{"i/" + gen + "/orphan", "q/" + gen + "/orphan"} {
		var value any
		if err := s.Get(key, &value); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("retained old index row %q: %v", key, err)
		}
	}
	if got, err := restarted.Stat("raw-\xff"); err != nil || got.Revision != n.Revision {
		t.Errorf("upgrade changed managed revision: %+v %v", got, err)
	}
	if vs, err := restarted.Versions("raw-\xff"); err != nil || len(vs) != 1 {
		t.Errorf("upgrade lost history: %+v %v", vs, err)
	}
	if _, err := restarted.Session(sessionID); err != nil {
		t.Errorf("upgrade lost session: %v", err)
	}
	if rawIndexPaths(t, restarted)["orphan"] != "" {
		t.Error("orphan retained after upgrade")
	}
	if got, err := restarted.Resolve(n.ID); err != nil || got.Path != "raw-\xff" {
		t.Errorf("upgrade lost identity: %+v %v", got, err)
	}
}
