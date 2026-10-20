package domain

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestPersistedPathsRoundTrip(t *testing.T) {
	for _, p := range []string{"plain/€", "\xff/mid-\xfe", "�/mid-�"} {
		for _, original := range []any{
			claim{Device: 1, Inode: 2, Path: p},
			mutation{Kind: "move", From: p, To: p + "-target"},
			publication{Path: p, Node: Node{Path: p}, Claim: claim{Path: p}, Receipt: &sessionReceipt{Session: Session{Path: p}}},
			transferReceipt{SourcePath: p, TargetPath: p + "-target"},
			sessionReceipt{Session: Session{Path: p}, CleanupPending: true},
		} {
			encoded, err := json.Marshal(original)
			if err != nil {
				t.Fatal(err)
			}
			restored := reflect.New(reflect.TypeOf(original))
			if err := json.Unmarshal(encoded, restored.Interface()); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(original, restored.Elem().Interface()) {
				t.Errorf("%T lost path %q: %s", original, p, encoded)
			}
		}
	}
}
func TestPersistedPathsLegacyUTF8(t *testing.T) {
	// No byte fields existed in these durable records. Existing UTF-8 paths
	// remain readable without changing their request hashes or public wire types.
	for _, tc := range []struct {
		data   string
		target any
	}{
		{`{"Device":1,"Inode":2,"Path":"plain/€"}`, &claim{}},
		{`{"Kind":"move","From":"plain/€","To":"target"}`, &mutation{}},
		{`{"Path":"plain/€","Node":{"path":"plain/€"},"Claim":{"Path":"plain/€"}}`, &publication{}},
		{`{"SourcePath":"plain/€","TargetPath":"target"}`, &transferReceipt{}},
		{`{"path":"plain/€","state":"open"}`, &sessionReceipt{}},
	} {
		if err := json.Unmarshal([]byte(tc.data), tc.target); err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(tc.target)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(b, &fields); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"PathBytes", "FromBytes", "ToBytes", "SourcePathBytes", "TargetPathBytes", "pathBytes"} {
			if _, ok := fields[name]; ok {
				t.Errorf("%T adds %s for UTF-8", tc.target, name)
			}
		}
	}
}
func TestRawPathTransferRequestHash(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range []string{"\xff", "\xfe", "�"} {
		for _, to := range []string{"\xff", "\xfe", "�"} {
			h, err := transferRequestHash("source", "target", p, to, WriteOptions{}, nil, nil)
			if err != nil || seen[h] {
				t.Fatalf("hash collision for %q -> %q: %v", p, to, err)
			}
			seen[h] = true
		}
	}
}

func TestRawPathIndexNodeReplacement(t *testing.T) {
	old := Node{Path: "mid-\xff.pdf", ID: "raw", Size: 1}
	sibling := Node{Path: "mid-�.pdf", ID: "sibling", Size: 1}
	r := indexedListingRoot(t, []Node{old, sibling})
	updated := old
	updated.Size = 2
	if err := r.indexNode(updated); err != nil {
		t.Fatal(err)
	}
	rows := r.State.(*listingState).rows
	for _, key := range nodeIndexKeys(r.generation, old) {
		if key == "i/"+r.generation+"/"+old.Path {
			continue
		}
		// Path/name rows are rewritten at the same key; size rows must disappear.
		if strings.Contains(key, "/size/") {
			if _, ok := rows[key]; ok {
				t.Errorf("stale size key %q", key)
			}
		}
	}
	for _, n := range []Node{updated, sibling} {
		for _, key := range nodeIndexKeys(r.generation, n) {
			if _, ok := rows[key]; !ok {
				t.Errorf("missing key %q", key)
			}
		}
	}
}

func TestPersistedSessionLegacySegments(t *testing.T) {
	for _, p := range []string{"plain", "target-\xff"} {
		s := Session{ID: "session", Path: p, State: SessionOpen, Size: 1, ChunkSize: 1, Received: 1}
		b, err := json.Marshal(sessionReceipt{Session: s})
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(b, &fields); err != nil {
			t.Fatal(err)
		}
		fields["segments"] = json.RawMessage(`{"0":"sha256:2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"}`)
		b, err = json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		state := newVersionState()
		if err := state.Put("session/session", json.RawMessage(b)); err != nil {
			t.Fatal(err)
		}
		root := &Root{State: state}
		got, err := root.loadSessionRecord("session/session")
		if err != nil || got.Path != p || got.UploadedSegments != 1 {
			t.Fatalf("migration lost data: %+v %v", got, err)
		}
		var chunk SessionSegment
		if err := state.Get(segmentKey("session", 0), &chunk); err != nil || chunk.Hash != "sha256:2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881" {
			t.Fatal(chunk, err)
		}
		got, err = root.loadSessionRecord("session/session")
		if err != nil || got.Path != p {
			t.Fatalf("migration rewrite lost path: %+v %v", got, err)
		}
		public, err := json.Marshal(got.Session)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(public), "pathBytes") {
			t.Fatalf("private path bytes leaked into public Session: %s", public)
		}
	}
}
