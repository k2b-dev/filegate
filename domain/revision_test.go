package domain

import (
	"fmt"
	"sort"
	"testing"
)

func TestPreconditionOptions(t *testing.T) {
	for _, options := range []WriteOptions{
		{Precondition: &Precondition{}},
		{Precondition: &Precondition{IfMatch: "token", IfNoneMatch: true}},
		{OnConflict: "rename", Precondition: &Precondition{IfMatch: "token"}},
		{OnConflict: "rename", Precondition: &Precondition{IfNoneMatch: true}},
		{OnConflict: "overwrite", Precondition: &Precondition{IfNoneMatch: true}},
		{Precondition: &Precondition{IfMatch: "\"token\""}},
	} {
		if err := ValidateOptions(options); err == nil {
			t.Fatalf("accepted %+v", options)
		}
	}
	for _, options := range []WriteOptions{
		{},
		{Precondition: &Precondition{IfNoneMatch: true}},
		{OnConflict: "overwrite", Precondition: &Precondition{IfMatch: "opaque-token"}},
	} {
		if err := ValidateOptions(options); err != nil {
			t.Fatalf("rejected %+v: %v", options, err)
		}
	}
}

func TestRevisionMutationStreamsBoundedRecoverableBatches(t *testing.T) {
	state := newVersionState()
	const count = 1000
	for i := 0; i < count; i++ {
		rec := managedRevision{Token: fmt.Sprintf("token-%d", i), Fingerprint: fileFingerprint{Device: 1, Inode: uint64(i + 1)}}
		state.seed(fmt.Sprintf("%stree/%04d", managedRevisionPrefix, i), rec)
		state.seed(fmt.Sprintf("%stree-other/%04d", managedRevisionPrefix, i), rec)
	}
	sort.Strings(state.keys)
	root := &Root{State: state}
	state.failAtBatch = 3
	if err := root.finishRevisionMutation("tree", "moved", false); err == nil {
		t.Fatal("failure not injected")
	}
	state.failAtBatch = 0
	state.reset()
	if err := root.finishRevisionMutation("tree", "moved", false); err != nil {
		t.Fatal(err)
	}
	if state.maxBatch > 256 || state.visited != count-256 {
		t.Fatalf("unbounded or repeated work: %+v", state)
	}
	for i := 0; i < count; i++ {
		var rec managedRevision
		if err := state.Get(fmt.Sprintf("%smoved/%04d", managedRevisionPrefix, i), &rec); err != nil || rec.Token != fmt.Sprintf("token-%d", i) {
			t.Fatal(rec, err)
		}
		if _, ok := state.values[fmt.Sprintf("%stree/%04d", managedRevisionPrefix, i)]; ok {
			t.Fatal("source record retained")
		}
	}
	state.reset()
	if err := root.finishRevisionMutation("moved", "", true); err != nil {
		t.Fatal(err)
	}
	if state.visited != count || state.maxBatch > 256 || len(state.values) != count {
		t.Fatalf("delete escaped subtree/bound: visited=%d max=%d remaining=%d", state.visited, state.maxBatch, len(state.values))
	}
}

func TestManagedSettingRotatesGenerationOnlyAtContractChanges(t *testing.T) {
	state := newVersionState()
	root := &Root{State: state, Config: RootConfig{Managed: true}}
	if err := root.initializeManagedSetting(); err != nil {
		t.Fatal(err)
	}
	first, err := root.writeGeneration()
	if err != nil || first == "" {
		t.Fatal(first, err)
	}
	if err = root.initializeManagedSetting(); err != nil {
		t.Fatal(err)
	}
	same, _ := root.writeGeneration()
	if same != first {
		t.Fatal("normal restart invalidated proof")
	}
	root.Config.Managed = false
	state.failBatch = true
	if err = root.initializeManagedSetting(); err == nil {
		t.Fatal("failed setting transition accepted")
	}
	var old bool
	if err = state.Get("root/managed", &old); err != nil || !old {
		t.Fatal("partial marker write", old, err)
	}
	state.failBatch = false
	if err = root.initializeManagedSetting(); err != nil {
		t.Fatal(err)
	}
	second, _ := root.writeGeneration()
	if second == first {
		t.Fatal("leaving managed mode retained proof")
	}
	root.Config.Managed = true
	if err = root.initializeManagedSetting(); err != nil {
		t.Fatal(err)
	}
	third, _ := root.writeGeneration()
	if third == first || third == second {
		t.Fatal("returning to managed mode retained proof")
	}
}
