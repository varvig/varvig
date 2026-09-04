package core

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/provenance"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// TestGateIsStrictSubset asserts the capability containment directly, not through
// the verb list (design addendum, U3): the gate holds strictly fewer capabilities
// than the CLI. A verb-list test would keep passing while the gate's underlying
// reach grew; this does not.
func TestGateIsStrictSubset(t *testing.T) {
	gate, cli := GateCapabilities(), CLICapabilities()
	if !gate.SubsetOf(cli) {
		t.Fatal("the gate's capabilities must be a subset of the CLI's")
	}
	if gate.Equal(cli) {
		t.Fatal("the gate must hold strictly fewer capabilities than the CLI, not the same set")
	}
	// Every capability the CLI holds that the gate must not: proven absent.
	for _, c := range []Capability{CapAdvanceRef, CapReadAnyPath, CapNetwork, CapRunHooks} {
		if !cli.Has(c) {
			t.Errorf("the CLI should hold %q", c)
		}
		if gate.Has(c) {
			t.Errorf("the gate must not hold %q", c)
		}
	}
	// The one it does hold: propose, the floor.
	if !gate.Has(CapPropose) {
		t.Error("the gate must be able to propose")
	}
}

// TestCommitRefusedWithoutAdvanceRef proves the gate cannot exercise the
// advance-ref capability: core.Commit with the gate's set is refused with a named
// error and no ref moves. Removing a capability makes the verb refuse, not
// silently do less.
func TestCommitRefusedWithoutAdvanceRef(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	tree, _ := r.Objects.Put(object.NewTree(nil))
	const ref = "refs/heads/main"

	_, err = Commit(r, GateCapabilities(), CommitParams{
		Ref: ref, ExpectedOld: nil, Tree: tree, Message: "sneak", Author: "gate",
		Provenance: provenance.Build("gate"), Signer: priv, Now: 1,
	})
	if !errors.Is(err, ErrCapability) {
		t.Fatalf("commit without advance-ref must be a named capability refusal, got %v", err)
	}
	// The refusal is total: no change stored as the ref, and the ref did not move.
	if _, err := r.Refs.Resolve(ref); err == nil {
		t.Fatal("a refused commit must not have advanced the ref")
	}
}

// TestProposeNeedsProposeCapability: propose refuses without the propose
// capability, and both shells' sets can propose.
func TestProposeNeedsProposeCapability(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	tree, _ := r.Objects.Put(object.NewTree(nil))
	params := ProposeParams{Tree: tree, Message: "m", Author: "a", Signer: priv, SpecTask: "t", Now: 1}

	if _, err := Propose(r, NewCapabilitySet(), params); !errors.Is(err, ErrCapability) {
		t.Fatalf("propose with no capabilities must be a named refusal, got %v", err)
	}
	for _, caps := range []CapabilitySet{GateCapabilities(), CLICapabilities()} {
		if _, err := Propose(r, caps, params); err != nil {
			t.Fatalf("propose with %v must succeed: %v", caps.List(), err)
		}
	}
}
