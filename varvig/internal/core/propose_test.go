package core

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/provenance"
	"github.com/dividebyzero/claude-experiments/varvig/internal/readapi"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// TestProposeFinalization pins the single write-finalization contract both shells
// depend on: a proposal is signed by the proposer's key, carries the message,
// reasoning, and access set in its provenance, is recorded in the speculation
// pool, and reads its provenance back from the store — never a ref move.
func TestProposeFinalization(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	// A base change and a proposed tree over it.
	blob, _ := r.Objects.Put(object.NewBlob([]byte("hello")))
	tree, _ := r.Objects.Put(object.NewTree([]object.Entry{
		{Name: "f.txt", Mode: 0o100644, Kind: object.TypeBlob, ID: blob},
	}))
	base, _ := r.Objects.Put(object.NewChange(object.Change{Tree: tree, Message: "base", Timestamp: 1}))

	res, err := Propose(r, CLICapabilities(), ProposeParams{
		Base:        base,
		Tree:        tree,
		Message:     "add greeting",
		Reasoning:   "chose hello over hi for the test",
		Authority:   "task:abc",
		Author:      "task:abc",
		ContextRead: []string{blob.Hex()},
		Signer:      priv,
		SpecTask:    "task-1",
		Now:         100,
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}

	// Parents point at the base.
	if len(res.Parents) != 1 || !res.Parents[0].Equal(base) {
		t.Fatalf("parents = %v, want [base]", res.Parents)
	}

	// The change is signed by the proposer's key and verifies.
	changeObj, err := r.Objects.Get(res.Change)
	if err != nil {
		t.Fatal(err)
	}
	gotPub, err := provenance.Verify(changeObj)
	if err != nil {
		t.Fatalf("proposed change does not verify: %v", err)
	}
	if !gotPub.Equal(pub) {
		t.Fatal("change signed by a different key than the proposer's")
	}

	// Read-back reflects what was stored, not what was sent.
	if res.Stored.TaskSpec != "add greeting" || res.Stored.Reasoning != "chose hello over hi for the test" {
		t.Fatalf("stored intent = %+v", res.Stored)
	}
	if !strings.Contains(res.Stored.ContextRead, blob.Hex()) {
		t.Fatalf("access set not recorded in provenance: %q", res.Stored.ContextRead)
	}

	// Recorded in the speculation pool under its task — and no ref moved.
	props, err := readapi.New(r).Proposals("task-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(props) != 1 || props[0].Change != res.Change.Hex() {
		t.Fatalf("proposal not recorded in the pool: %+v", props)
	}
	if refs, _ := r.Refs.List(); len(refs) != 0 {
		t.Fatalf("propose moved a ref (%v); it must never promote", refs)
	}
}

// TestProposeUnbornBase: a proposal from an empty repo has no parents.
func TestProposeUnbornBase(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	tree, _ := r.Objects.Put(object.NewTree(nil))

	res, err := Propose(r, CLICapabilities(), ProposeParams{
		Base: nil, Tree: tree, Message: "first", Authority: "a", Author: "a",
		Signer: priv, SpecTask: "t", Now: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Parents) != 0 {
		t.Fatalf("an unborn base must yield no parents, got %v", res.Parents)
	}
	var _ multihash.Multihash = res.Change
}
