package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

func TestTransferIdentityLegacyHashAndReceipt(t *testing.T) {
	for _, actor := range []*ExecutionIdentity{nil, {UID: 1234, GID: 1234, Groups: []uint32{1234}}} {
		// This is the persisted v6 request encoding, before independent actors.
		old, err := json.Marshal(struct {
			SourceStore      string
			DestinationStore string
			Path             string
			Target           string
			Options          WriteOptions
			Execution        *ExecutionIdentity
		}{"source", "destination", "a", "b", WriteOptions{}, actor})
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(old)
		got, err := transferRequestHash("source", "destination", "a", "b", WriteOptions{}, actor, actor)
		if err != nil || got != hex.EncodeToString(sum[:]) {
			t.Fatalf("old request hash changed: %s %v", got, err)
		}
		oldReceipt, err := json.Marshal(struct{ Execution *ExecutionIdentity }{actor})
		if err != nil {
			t.Fatal(err)
		}
		var receipt transferReceipt
		if err := json.Unmarshal(oldReceipt, &receipt); err != nil {
			t.Fatal(err)
		}
		if !sameExecution(receipt.Execution, actor) || !sameExecution(receipt.destinationExecution(), actor) {
			t.Fatal("legacy receipt lost its shared identity")
		}
	}
}

func TestTransferIdentityDistinctBindings(t *testing.T) {
	a := &ExecutionIdentity{UID: 1234, GID: 1234, Groups: []uint32{1234}}
	b := &ExecutionIdentity{UID: 4321, GID: 4321, Groups: []uint32{4321}}
	seen := map[string]bool{}
	for _, pair := range [][2]*ExecutionIdentity{{nil, nil}, {a, a}, {a, b}, {a, nil}, {nil, a}, {b, a}} {
		hash, err := transferRequestHash("source", "destination", "a", "b", WriteOptions{}, pair[0], pair[1])
		if err != nil || seen[hash] {
			t.Fatalf("actor binding collision: %s %v", hash, err)
		}
		seen[hash] = true
		original := transferReceipt{Execution: pair[0], DestinationExecution: transferDestinationFor(pair[0], pair[1])}
		encoded, err := json.Marshal(original)
		if err != nil {
			t.Fatal(err)
		}
		var restored transferReceipt
		if err := json.Unmarshal(encoded, &restored); err != nil {
			t.Fatal(err)
		}
		if !sameExecution(restored.Execution, pair[0]) || !sameExecution(restored.destinationExecution(), pair[1]) {
			t.Fatalf("actor bindings changed after persistence: %s", encoded)
		}
	}
}
