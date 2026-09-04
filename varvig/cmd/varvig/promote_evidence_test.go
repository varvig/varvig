package main

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/check"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// change1File stores a one-file tree and a change over it.
func change1File(t *testing.T, r *repo.Repo, content string) (tree, change multihash.Multihash) {
	t.Helper()
	blob, err := r.Objects.Put(object.NewBlob([]byte(content)))
	if err != nil {
		t.Fatal(err)
	}
	tree, err = r.Objects.Put(object.NewTree([]object.Entry{
		{Name: "f.txt", Mode: 0o100644, Kind: object.TypeBlob, ID: blob},
	}))
	if err != nil {
		t.Fatal(err)
	}
	change, err = r.Objects.Put(object.NewChange(object.Change{Tree: tree, Message: "m", Timestamp: 1}))
	if err != nil {
		t.Fatal(err)
	}
	return tree, change
}

// TestCheckPromotionEvidenceNotStale is the promotion-side of §1.3's evidence
// invariant (build spec P1.3): fresh passing evidence promotes; evidence for a
// tree the proposal has since moved off is stale and refused, with the staleness
// named; an unchecked proposal is unaffected.
func TestCheckPromotionEvidenceNotStale(t *testing.T) {
	if _, err := exec.LookPath("true"); err != nil {
		t.Skip("true not on PATH")
	}
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// A proposal never checked -> unaffected.
	_, unchecked := change1File(t, r, "never-checked")
	if err := checkPromotionEvidenceNotStale(r, unchecked); err != nil {
		t.Fatalf("an unchecked proposal must not be blocked: %v", err)
	}

	// A checked proposal with fresh passing evidence -> allowed.
	treeA, change := change1File(t, r, "checked")
	ev, err := check.Run(r, treeA, change, []string{"true"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := check.Attach(r, ev); err != nil {
		t.Fatal(err)
	}
	if err := checkPromotionEvidenceNotStale(r, change); err != nil {
		t.Fatalf("fresh passing evidence must promote: %v", err)
	}

	// Now attach evidence for a *different* tree to a fresh change, simulating a
	// proposal edited after checking: the only evidence is stale -> refused, and
	// the refusal names the staleness.
	treeStale, _ := change1File(t, r, "old-content")
	editedTree, edited := change1File(t, r, "new-content")
	stale := ev
	stale.Tree = treeStale.Hex()
	stale.Change = edited.Hex()
	if _, err := check.Attach(r, stale); err != nil {
		t.Fatal(err)
	}
	err = checkPromotionEvidenceNotStale(r, edited)
	if err == nil {
		t.Fatal("a proposal with only stale evidence must be refused")
	}
	msg := err.Error()
	if !strings.Contains(msg, "stale") || !strings.Contains(msg, treeStale.Hex()) || !strings.Contains(msg, editedTree.Hex()) {
		t.Fatalf("refusal must name the staleness and both trees: %v", msg)
	}
}
