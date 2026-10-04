//go:build linux

package integration_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k2b-dev/filegate/v7/domain"
)

type countedSearchFiles struct {
	domain.Files
	directories map[string]int
	metadata    int
	stats       int
}

func (f *countedSearchFiles) Open(p string, flags int, mode os.FileMode) (*os.File, error) {
	file, err := f.Files.Open(p, flags, mode)
	if err == nil {
		if st, statErr := file.Stat(); statErr == nil && st.IsDir() {
			f.directories[p]++
		}
	}
	return file, err
}
func (f *countedSearchFiles) Stat(p string) (os.FileInfo, error) {
	f.stats++
	return f.Files.Stat(p)
}
func (f *countedSearchFiles) MetadataOpen(p string) (*os.File, error) {
	f.metadata++
	if files, ok := f.Files.(interface {
		MetadataOpen(string) (*os.File, error)
	}); ok {
		return files.MetadataOpen(p)
	}
	return f.Files.Open(p, os.O_RDONLY, 0)
}

func TestExecutionSearchUsesLiveSnapshotOnIndexedRoot(t *testing.T) {
	x := executionFixture(t, true, false)
	if err := os.MkdirAll(filepath.Join(x.data, "visible/nested"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b", "c"} {
		if err := os.WriteFile(filepath.Join(x.data, "visible/nested", name), []byte(name), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// These files were created externally after the initial empty index rebuild.
	indexed, err := x.r.Search(ctx, "", "visible", domain.ListingOptions{Type: "files"})
	if err != nil || len(indexed.Items) != 0 {
		t.Fatalf("unexpected indexed state: %+v %v", indexed, err)
	}
	actor := executionView(t, x)
	counted := &countedSearchFiles{Files: actor.Files, directories: map[string]int{}}
	actor.Files = counted
	options := domain.ListingOptions{Limit: 1, Type: "files", Sort: "name"}
	first, err := actor.Search(ctx, "", "visible", options)
	if err != nil || len(first.Items) != 1 || first.Items[0].Path != "visible/nested/a" || first.Next == "" {
		t.Fatalf("live search: %+v %v", first, err)
	}
	metadataAfterScan := counted.metadata
	statsAfterFirst := counted.stats
	options.After = first.Next
	second, err := actor.Search(ctx, "", "visible", options)
	if err != nil || len(second.Items) != 1 || second.Items[0].Path != "visible/nested/b" {
		t.Fatalf("second page: %+v %v", second, err)
	}
	if counted.metadata != metadataAfterScan {
		t.Fatalf("second page rescanned nodes: before=%d after=%d", metadataAfterScan, counted.metadata)
	}
	if counted.stats-statsAfterFirst != len(second.Items) {
		t.Fatalf("page authorization exceeded returned nodes: stats=%d items=%d", counted.stats-statsAfterFirst, len(second.Items))
	}
	if err := os.WriteFile(filepath.Join(x.data, "visible/nested/new"), []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	fresh, err := actor.Search(ctx, "new", "visible", domain.ListingOptions{})
	if err != nil || len(fresh.Items) != 1 || fresh.Items[0].Path != "visible/nested/new" {
		t.Fatalf("external change missing without rebuild: %+v %v", fresh, err)
	}
}

func TestExecutionSearchDoesNotReturnPartialResultsOnTraversalFailure(t *testing.T) {
	x := executionFixture(t, true, false)
	if err := os.MkdirAll(filepath.Join(x.data, "visible/blocked"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(x.data, "visible/public"), []byte("public"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(x.data, "visible/blocked"), 0700); err != nil {
		t.Fatal(err)
	}
	actor := executionView(t, x)
	page, err := actor.Search(ctx, "public", "visible", domain.ListingOptions{Limit: 1})
	if !errors.Is(err, os.ErrPermission) || len(page.Items) != 0 || page.Next != "" {
		t.Fatalf("incomplete success: %+v %v", page, err)
	}
	if err := os.Chmod(filepath.Join(x.data, "visible/blocked"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(x.data, "visible/unreadable"), []byte("secret bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	// Directory listing/stat permissions may reveal a filename without authorizing
	// file bytes. Search must not silently invent a content-read authorization.
	page, err = actor.Search(ctx, "unreadable", "visible", domain.ListingOptions{})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("metadata search: %+v %v", page, err)
	}
	f, err := actor.Open("visible/unreadable")
	if err == nil {
		f.Close()
		t.Fatal("metadata search granted byte access")
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Fatal(err)
	}
}

func TestExecutionSearchCursorChecksIdentityAndCurrentAncestors(t *testing.T) {
	x := executionFixture(t, true, false)
	if err := os.MkdirAll(filepath.Join(x.data, "visible/nested"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(x.data, "visible/nested", name), []byte(name), 0644); err != nil {
			t.Fatal(err)
		}
	}
	actor := executionView(t, x)
	options := domain.ListingOptions{Limit: 1, Type: "files"}
	first, err := actor.Search(ctx, "", "visible", options)
	if err != nil || first.Next == "" {
		t.Fatal(first, err)
	}
	options.After = first.Next
	other, closeOther, err := x.r.WithExecution(ctx, &domain.ExecutionIdentity{UID: executionUID + 1, GID: executionGID})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOther()
	if _, err := other.Search(ctx, "", "visible", options); !errors.Is(err, domain.ErrCursorInvalid) {
		t.Fatalf("cross-identity cursor accepted: %v", err)
	}
	// Keep execute permission, but revoke directory enumeration. A cached page
	// must check read access as well as pathname traversal, without rescanning.
	if err := os.Chmod(filepath.Join(x.data, "visible/nested"), 0711); err != nil {
		t.Fatal(err)
	}
	page, err := actor.Search(ctx, "", "visible", options)
	if !errors.Is(err, os.ErrPermission) || len(page.Items) != 0 {
		t.Fatalf("cached metadata bypassed revoked parent read: %+v %v", page, err)
	}
}

func TestExecutionSearchHTTPAllowsScopedSearchAndRejectsOtherCursorIdentity(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root daemon")
	}
	x, server, _ := server(t)
	x.r.Config.Execution = true
	if err := os.Chmod(x.data, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"external-a", "external-b"} {
		if err := os.WriteFile(filepath.Join(x.data, name), []byte(name), 0644); err != nil {
			t.Fatal(err)
		}
	}
	actor := &domain.ExecutionIdentity{UID: executionUID, GID: executionGID}
	endpoint := server.URL + "/v1/roots/test/search?q=external&limit=1&type=files"
	response := executionRequest(t, "GET", endpoint, "", true, actor)
	var page domain.Page
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 || len(page.Items) != 1 || page.Next == "" {
		t.Fatalf("scoped search rejected: %s %+v", response.Status, page)
	}
	response = executionRequest(t, "GET", endpoint+"&after="+url.QueryEscape(page.Next), "", true, &domain.ExecutionIdentity{UID: executionUID + 1, GID: executionGID})
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 409 || !strings.Contains(string(body), "cursor_invalid") {
		t.Fatalf("cursor identity: %s %s %v", response.Status, body, err)
	}
	if err := os.Mkdir(filepath.Join(x.data, "blocked"), 0700); err != nil {
		t.Fatal(err)
	}
	response = executionRequest(t, "GET", endpoint, "", true, actor)
	body, err = io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 403 || strings.Contains(string(body), "external-a") {
		t.Fatalf("traversal failure returned partial result: %s %s %v", response.Status, body, err)
	}
}

func TestExecutionSearchCachedPageRequiresCurrentSearchPermission(t *testing.T) {
	for _, revoked := range []string{"visible", "visible/nested"} {
		t.Run(revoked, func(t *testing.T) {
			x := executionFixture(t, true, false)
			if err := os.MkdirAll(filepath.Join(x.data, "visible/nested"), 0755); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"a", "b"} {
				if err := os.WriteFile(filepath.Join(x.data, "visible/nested", name), nil, 0644); err != nil {
					t.Fatal(err)
				}
			}
			actor := executionView(t, x)
			options := domain.ListingOptions{Limit: 1, Type: "files"}
			first, err := actor.Search(ctx, "", "visible", options)
			if err != nil || first.Next == "" {
				t.Fatal(first, err)
			}
			// Directory read remains allowed; only pathname search is revoked.
			if err := os.Chmod(filepath.Join(x.data, revoked), 0744); err != nil {
				t.Fatal(err)
			}
			options.After = first.Next
			page, err := actor.Search(ctx, "", "visible", options)
			if !errors.Is(err, os.ErrPermission) || len(page.Items) != 0 || page.Next != "" {
				t.Fatalf("cached search bypassed revoked search permission: %+v %v", page, err)
			}
		})
	}
}

func TestExecutionListCachedPageRechecksNodes(t *testing.T) {
	for _, change := range []string{"revoke-search", "remove-leaf"} {
		t.Run(change, func(t *testing.T) {
			x := executionFixture(t, true, false)
			if err := os.Mkdir(filepath.Join(x.data, "visible"), 0755); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"a", "b"} {
				if err := os.WriteFile(filepath.Join(x.data, "visible", name), nil, 0644); err != nil {
					t.Fatal(err)
				}
			}
			actor := executionView(t, x)
			options := domain.ListingOptions{Limit: 1}
			first, err := actor.List(ctx, "visible", options)
			if err != nil || first.Next == "" {
				t.Fatal(first, err)
			}
			want := os.ErrPermission
			if change == "revoke-search" {
				err = os.Chmod(filepath.Join(x.data, "visible"), 0744)
			} else {
				err = os.Remove(filepath.Join(x.data, "visible/b"))
				want = os.ErrNotExist
			}
			if err != nil {
				t.Fatal(err)
			}
			options.After = first.Next
			page, err := actor.List(ctx, "visible", options)
			if !errors.Is(err, want) || len(page.Items) != 0 || page.Next != "" {
				t.Fatalf("cached list bypassed node recheck: %+v %v", page, err)
			}
		})
	}
}
