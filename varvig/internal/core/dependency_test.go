package core

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// blobIn stores a blob and returns its id.
func blobIn(t *testing.T, r *repo.Repo, s string) multihash.Multihash {
	t.Helper()
	id, err := r.Objects.Put(object.NewBlob([]byte(s)))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// treeChange stores a two-file tree {a.txt, b.txt} and a change over it, and
// returns the change id and the blob id of a.txt.
func treeChange(t *testing.T, r *repo.Repo, a, b string) (change, aBlob multihash.Multihash) {
	t.Helper()
	ha := blobIn(t, r, a)
	hb := blobIn(t, r, b)
	tree, err := r.Objects.Put(object.NewTree([]object.Entry{
		{Name: "a.txt", Mode: 0o100644, Kind: object.TypeBlob, ID: ha},
		{Name: "b.txt", Mode: 0o100644, Kind: object.TypeBlob, ID: hb},
	}))
	if err != nil {
		t.Fatal(err)
	}
	ch, err := r.Objects.Put(object.NewChange(object.Change{Tree: tree, Message: "m", Timestamp: 1}))
	if err != nil {
		t.Fatal(err)
	}
	return ch, ha
}

// TestDependencyStaleChangedAndRead: a read whose content changed between bases is
// stale; a read whose content is unchanged is not.
func TestDependencyStaleChangedAndRead(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	oldBase, aBlob := treeChange(t, r, "a1", "b1")
	changedA, _ := treeChange(t, r, "a2", "b1") // a.txt changed
	changedB, _ := treeChange(t, r, "a1", "b2") // only b.txt changed

	// The change read a.txt (aBlob).
	reads := []string{aBlob.Hex()}

	// a.txt changed → the read is stale.
	stale, changed, err := DependencyStale(r, reads, oldBase, changedA)
	if err != nil {
		t.Fatal(err)
	}
	if !stale || len(changed) != 1 || changed[0] != aBlob.Hex() {
		t.Fatalf("changed-and-read must be stale, naming the read: stale=%v changed=%v", stale, changed)
	}

	// only b.txt changed, a.txt (the read) is unchanged → not stale.
	stale, _, err = DependencyStale(r, reads, oldBase, changedB)
	if err != nil {
		t.Fatal(err)
	}
	if stale {
		t.Fatal("changed-and-unread must not be stale — this is the parallelism observation buys")
	}
}

// TestDependencyStaleNeverRead: with no recorded reads, nothing is stale however
// the base moved.
func TestDependencyStaleNeverRead(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	oldBase, _ := treeChange(t, r, "a1", "b1")
	newBase, _ := treeChange(t, r, "a2", "b2") // everything changed

	stale, _, err := DependencyStale(r, nil, oldBase, newBase)
	if err != nil {
		t.Fatal(err)
	}
	if stale {
		t.Fatal("a change that read nothing depends on nothing and is never stale")
	}
}

// TestCheckReadStaleness ties it to a real proposed change: it reads a.txt, the
// base then moves so a.txt changes, and promotion-time validation flags it; when
// the base has not moved it is fine.
func TestCheckReadStaleness(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	oldBase, aBlob := treeChange(t, r, "a1", "b1")
	newTree, _ := r.Objects.Put(object.NewTree(nil))

	// A change built on oldBase that recorded reading a.txt.
	pr, err := Propose(r, CLICapabilities(), ProposeParams{
		Base: oldBase, Tree: newTree, Message: "work", Author: "t",
		ContextRead: []string{aBlob.Hex()}, Signer: priv, SpecTask: "t", Now: 2,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Base has not moved: not stale.
	if stale, _, err := CheckReadStaleness(r, pr.Change, oldBase); err != nil || stale {
		t.Fatalf("unchanged base must not be stale: stale=%v err=%v", stale, err)
	}

	// Base moves and a.txt changes: the change's read is stale.
	movedBase, _ := treeChange(t, r, "a2", "b1")
	stale, changed, err := CheckReadStaleness(r, pr.Change, movedBase)
	if err != nil {
		t.Fatal(err)
	}
	if !stale || len(changed) != 1 {
		t.Fatalf("a read that changed under a moved base must be stale: stale=%v changed=%v", stale, changed)
	}
}
