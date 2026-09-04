package core

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/provenance"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// TestCommitAdvancesRefSignedWithFulfills pins the commit finalization: it signs
// and stores the change (carrying its Fulfills link), attaches provenance, and
// advances the ref by compare-and-swap.
func TestCommitAdvancesRefSignedWithFulfills(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const ref = "refs/heads/main"

	blob, _ := r.Objects.Put(object.NewBlob([]byte("v1")))
	tree, _ := r.Objects.Put(object.NewTree([]object.Entry{
		{Name: "f.txt", Mode: 0o100644, Kind: object.TypeBlob, ID: blob},
	}))
	fulfills, _ := r.Objects.Put(object.NewBlob([]byte("intent revision")))

	res, err := Commit(r, CommitParams{
		Ref: ref, ExpectedOld: nil, // unborn
		Tree: tree, Message: "first", Author: "eng",
		Fulfills: fulfills, Provenance: provenance.Build("eng"),
		Signer: priv, Now: 100,
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// The ref advanced to the committed change.
	tip, err := r.Refs.Resolve(ref)
	if err != nil || !tip.Equal(res.Change) {
		t.Fatalf("ref = %v (err %v), want the committed change %s", tip, err, res.Change.Hex())
	}
	// Signed by the committer's key.
	if !res.SignerKey.Equal(pub) {
		t.Fatal("commit signed by a different key")
	}
	changeObj, _ := r.Objects.Get(res.Change)
	if _, err := provenance.Verify(changeObj); err != nil {
		t.Fatalf("committed change does not verify: %v", err)
	}
	// Carries its Fulfills link and its provenance.
	c, _ := changeObj.AsChange()
	if c.Fulfills == nil || !c.Fulfills.Equal(fulfills) {
		t.Fatalf("Fulfills = %v, want %s", c.Fulfills, fulfills.Hex())
	}
	if c.Provenance == nil || !c.Provenance.Equal(res.Provenance) {
		t.Fatal("change does not point at its stored provenance")
	}

	// A second commit with the correct lease advances again.
	tree2, _ := r.Objects.Put(object.NewTree(nil))
	if _, err := Commit(r, CommitParams{
		Ref: ref, ExpectedOld: res.Change, Tree: tree2, Message: "second",
		Author: "eng", Provenance: provenance.Build("eng"), Signer: priv, Now: 200,
	}); err != nil {
		t.Fatalf("second commit: %v", err)
	}
}

// TestCommitStaleLeaseConflicts: a commit whose ExpectedOld no longer matches the
// ref is refused, so a concurrent move is a clean conflict rather than a lost
// update.
func TestCommitStaleLeaseConflicts(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	const ref = "refs/heads/main"
	tree, _ := r.Objects.Put(object.NewTree(nil))

	first, err := Commit(r, CommitParams{
		Ref: ref, ExpectedOld: nil, Tree: tree, Message: "first", Author: "a",
		Provenance: provenance.Build("a"), Signer: priv, Now: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A commit that still thinks the ref is unborn must lose the CAS.
	if _, err := Commit(r, CommitParams{
		Ref: ref, ExpectedOld: nil, Tree: tree, Message: "racing", Author: "b",
		Provenance: provenance.Build("b"), Signer: priv, Now: 2,
	}); err == nil {
		t.Fatal("a stale lease must conflict, not silently overwrite")
	}
	// The ref still points at the first commit.
	if tip, _ := r.Refs.Resolve(ref); !tip.Equal(first.Change) {
		t.Fatal("the losing commit moved the ref")
	}
}
