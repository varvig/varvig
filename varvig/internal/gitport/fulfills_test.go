package gitport

import (
	"path/filepath"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/worktree"
)

// TestFulfillsGitRoundTrip: a native change's ticket→commit link survives export
// to Git and re-import, carried as a commit trailer (C3). The trailer is stripped
// from the re-imported change's message, leaving it clean.
func TestFulfillsGitRoundTrip(t *testing.T) {
	src := t.TempDir()
	r, err := repo.Init(src)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	writeFile(t, src, "app.txt", "code\n")
	tree, err := worktree.WriteTree(r.Objects, r.Root())
	if err != nil {
		t.Fatalf("WriteTree: %v", err)
	}
	rev, err := multihash.Sum(multihash.SHA2_256, []byte("approved-intent-revision"))
	if err != nil {
		t.Fatal(err)
	}
	head, err := r.Objects.Put(object.NewChange(object.Change{
		Tree: tree, Message: "implement the thing", Timestamp: 1723100000, Author: "agent", Fulfills: rev,
	}))
	if err != nil {
		t.Fatalf("Put change: %v", err)
	}
	if err := r.Refs.CompareAndSwap("refs/heads/main", nil, head, "agent", "commit"); err != nil {
		t.Fatalf("CAS: %v", err)
	}

	gitDir := filepath.Join(t.TempDir(), ".git")
	if _, err := Export(r, gitDir, "main", head); err != nil {
		t.Fatalf("Export: %v", err)
	}

	r2, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatalf("Init r2: %v", err)
	}
	imported, err := Import(r2, gitDir, "main")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	obj, err := r2.Objects.Get(imported)
	if err != nil {
		t.Fatalf("get imported: %v", err)
	}
	c, err := obj.AsChange()
	if err != nil {
		t.Fatalf("AsChange: %v", err)
	}
	if c.Fulfills == nil || !c.Fulfills.Equal(rev) {
		t.Fatalf("imported Fulfills = %v, want %s", c.Fulfills, rev.Hex())
	}
	if c.Message != "implement the thing" {
		t.Fatalf("imported message = %q; trailer should be stripped", c.Message)
	}
}
