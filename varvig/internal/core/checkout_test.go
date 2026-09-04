package core

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/worktree"
)

// srcFixture builds a repo with a nested base tree, a base change on
// refs/heads/main, and extra refs (a sibling branch and a ticket ref) that must
// NOT leak into a task checkout.
func srcFixture(t *testing.T) (*repo.Repo, multihash.Multihash, multihash.Multihash) {
	t.Helper()
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	put := func(o *object.Object) multihash.Multihash {
		id, err := r.Objects.Put(o)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	login := put(object.NewBlob([]byte("package auth\n")))
	authTree := put(object.NewTree([]object.Entry{
		{Name: "login.go", Mode: 0o100644, Kind: object.TypeBlob, ID: login},
	}))
	readme := put(object.NewBlob([]byte("# proj\n")))
	root := put(object.NewTree([]object.Entry{
		{Name: "src", Mode: 0o40000, Kind: object.TypeTree, ID: authTree},
		{Name: "README.md", Mode: 0o100644, Kind: object.TypeBlob, ID: readme},
	}))
	base := put(object.NewChange(object.Change{Tree: root, Message: "init", Author: "jan", Timestamp: 1}))
	if err := r.Refs.Create("refs/heads/main", base, "t", "seed"); err != nil {
		t.Fatal(err)
	}
	// Extra refs the checkout must not see (F2).
	sib := put(object.NewChange(object.Change{Tree: root, Message: "sibling", Author: "x", Timestamp: 2}))
	if err := r.Refs.Create("refs/heads/sibling", sib, "t", "seed"); err != nil {
		t.Fatal(err)
	}
	if err := r.Refs.Create("refs/varvig/tickets/abc", base, "t", "seed"); err != nil {
		t.Fatal(err)
	}
	return r, base, root
}

// TestProvisionCheckoutIsAnOrdinaryRepo is the F1 acceptance: the checkout is a
// full, ordinary repo whose base tree hash equals the upstream exactly and whose
// materialized working tree round-trips back to that same tree.
func TestProvisionCheckoutIsAnOrdinaryRepo(t *testing.T) {
	src, base, root := srcFixture(t)
	dir := filepath.Join(t.TempDir(), "task")

	dst, stat, err := ProvisionCheckout(src, dir, base)
	if err != nil {
		t.Fatalf("ProvisionCheckout: %v", err)
	}
	if stat.Objects == 0 {
		t.Error("provisioning must replicate objects and report the count")
	}

	// Base tree hash equals upstream, exactly — nothing narrowed the tree.
	dstTree, err := TreeOf(dst, base)
	if err != nil {
		t.Fatal(err)
	}
	if !dstTree.Equal(root) {
		t.Fatalf("checkout base tree %s != upstream %s", dstTree.Hex(), root.Hex())
	}

	// The working tree was materialized on disk.
	if b, err := os.ReadFile(filepath.Join(dir, "src", "login.go")); err != nil || string(b) != "package auth\n" {
		t.Fatalf("working tree not materialized: %q err=%v", b, err)
	}

	// It is an ordinary repo: re-scanning the working tree rebuilds the same tree
	// (a `diff`/`status` there would be clean), and the .varvig metadata is skipped.
	idx := worktree.OpenIndex(dst.GitDir())
	states, err := worktree.Scan(dst.Objects, dir, idx)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := worktree.BuildTree(dst.Objects, states)
	if err != nil {
		t.Fatal(err)
	}
	if !rebuilt.Equal(root) {
		t.Fatalf("rescanned working tree %s != base tree %s — not a faithful ordinary repo", rebuilt.Hex(), root.Hex())
	}
}

// TestProvisionCheckoutBaseRefOnly is the F2 acceptance: only the base ref is
// present; sibling and ticket refs do not leak into the checkout.
func TestProvisionCheckoutBaseRefOnly(t *testing.T) {
	src, base, _ := srcFixture(t)
	dir := filepath.Join(t.TempDir(), "task")
	dst, _, err := ProvisionCheckout(src, dir, base)
	if err != nil {
		t.Fatal(err)
	}
	refsList, err := dst.Refs.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(refsList) != 1 || refsList[0] != "refs/heads/main" {
		t.Fatalf("checkout refs = %v, want only refs/heads/main (base-only visibility)", refsList)
	}
}

// TestProvisionCheckoutEmptyBase: a nil base yields an ordinary, empty repo.
func TestProvisionCheckoutEmptyBase(t *testing.T) {
	src, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "task")
	dst, stat, err := ProvisionCheckout(src, dir, nil)
	if err != nil {
		t.Fatalf("ProvisionCheckout(nil base): %v", err)
	}
	if stat.Objects != 0 {
		t.Errorf("empty base should replicate nothing, got %d", stat.Objects)
	}
	if refsList, _ := dst.Refs.List(); len(refsList) != 0 {
		t.Errorf("empty checkout should have no refs, got %v", refsList)
	}
}
