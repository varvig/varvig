package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/readapi"
	"github.com/dividebyzero/claude-experiments/varvig/internal/worktree"
)

// subtreeHash resolves a repo-relative subtree of the fixture's base to its tree
// id, so a test can materialize a sparse checkout the way `varvig task start`
// does — only the scope subtree, at its repo-relative path.
func subtreeHash(t *testing.T, f *gateFixture, path string) multihash.Multihash {
	t.Helper()
	listing, err := readapi.New(f.repo).Tree(f.base, path)
	if err != nil {
		t.Fatalf("resolve subtree %q: %v", path, err)
	}
	id, err := multihash.ParseHex(listing.Hash)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func proposeResult(t *testing.T, r response) struct {
	Change string   `json:"change"`
	Tree   string   `json:"tree"`
	Paths  []string `json:"paths"`
} {
	t.Helper()
	tr := decodeTool(t, r)
	if tr.IsError {
		t.Fatalf("propose errored: %s", tr.Content[0].Text)
	}
	var prop struct {
		Change string   `json:"change"`
		Tree   string   `json:"tree"`
		Paths  []string `json:"paths"`
	}
	if err := json.Unmarshal(tr.StructuredContent, &prop); err != nil {
		t.Fatal(err)
	}
	return prop
}

// TestProposeFromCheckoutObservesSparseTree is the gate half of the observed-set
// loop (build spec P1.1): a task minted for src/auth gets only that subtree
// materialized, edits it, and proposes with no file contents. The gate must
// diff the checkout against the base, propose the edits it finds, and — the
// sparse-checkout correctness point — leave README.md and src/web (which were
// never materialized) untouched rather than reading them as deletions.
func TestProposeFromCheckoutObservesSparseTree(t *testing.T) {
	f := newGateFixture(t)
	dir := t.TempDir()
	authDir := filepath.Join(dir, "src", "auth")
	if err := worktree.Checkout(f.repo.Objects, subtreeHash(t, f, "src/auth"), authDir); err != nil {
		t.Fatalf("materialize checkout: %v", err)
	}

	// Edit the observed tree: change one file, add another. A hand-listed
	// proposal could forget the new file; the observed set cannot.
	if err := os.WriteFile(filepath.Join(authDir, "login.go"), []byte("package auth // patched\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "helper.go"), []byte("package auth\n\nfunc H() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	gate, _ := newGate(f, "src/auth", time.Hour)
	gate.SetCheckout(dir)
	resps := drive(t, gate, call(1, "varvig_propose", `{"message":"patch auth"}`))
	prop := proposeResult(t, resps[0])

	if got := map[string]bool{}; true {
		for _, p := range prop.Paths {
			got[p] = true
		}
		if !got["src/auth/login.go"] || !got["src/auth/helper.go"] {
			t.Fatalf("observed set = %v, want the edited and the forgotten-but-changed file", prop.Paths)
		}
		if len(prop.Paths) != 2 {
			t.Fatalf("observed set = %v, want exactly the two in-scope changes", prop.Paths)
		}
	}

	// Inspect the proposed tree: the two edits landed, and the unmaterialized
	// paths survived unchanged — a sparse checkout is not a whole-repo delete.
	treeID, err := multihash.ParseHex(prop.Tree)
	if err != nil {
		t.Fatal(err)
	}
	states, err := worktree.FlattenStates(f.repo.Objects, treeID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := states["src/auth/helper.go"]; !ok {
		t.Error("added file missing from proposed tree")
	}
	if states["src/auth/login.go"].Hash.Equal(f.loginBlob) {
		t.Error("edited file was not updated in the proposed tree")
	}
	if !states["README.md"].Hash.Equal(f.readme) {
		t.Error("README.md (never materialized) must survive a sparse-checkout proposal unchanged")
	}
	if !states["src/web/index.html"].Hash.Equal(f.webBlob) {
		t.Error("src/web (out of scope, never materialized) must survive unchanged, not be deleted")
	}
}

// TestProposeFromCheckoutRefusesOutOfScope: if the observed tree carries a change
// outside the grant's scope, the gate refuses the whole proposal rather than
// silently dropping it — the same reconciliation rule the CLI enforces.
func TestProposeFromCheckoutRefusesOutOfScope(t *testing.T) {
	f := newGateFixture(t)
	dir := t.TempDir()
	// Materialize the whole tree but scope the task to src/auth. The materialized
	// src/web edit is outside scope and must trip the refusal.
	if err := worktree.Checkout(f.repo.Objects, subtreeHash(t, f, ""), dir); err != nil {
		t.Fatalf("materialize checkout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "web", "index.html"), []byte("<html>evil</html>\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	gate, _ := newGate(f, "src/auth", time.Hour)
	gate.SetCheckout(dir)
	resps := drive(t, gate, call(1, "varvig_propose", `{"message":"sneak web edit"}`))
	tr := decodeTool(t, resps[0])
	if !tr.IsError {
		t.Fatal("a change outside the task scope must be refused, not proposed")
	}
}

// TestProposeFromCheckoutEmptyIsError: a clean checkout (nothing changed within
// scope) yields a named refusal, never a silent empty proposal.
func TestProposeFromCheckoutEmptyIsError(t *testing.T) {
	f := newGateFixture(t)
	dir := t.TempDir()
	if err := worktree.Checkout(f.repo.Objects, subtreeHash(t, f, "src/auth"), filepath.Join(dir, "src", "auth")); err != nil {
		t.Fatalf("materialize checkout: %v", err)
	}
	gate, _ := newGate(f, "src/auth", time.Hour)
	gate.SetCheckout(dir)
	resps := drive(t, gate, call(1, "varvig_propose", `{"message":"noop"}`))
	if tr := decodeTool(t, resps[0]); !tr.IsError {
		t.Fatal("proposing an unchanged checkout must be a named error, not an empty success")
	}
}

// TestProposeNoFilesNoCheckoutIsError: with neither file contents nor a checkout,
// the gate explains that it has nothing to observe.
func TestProposeNoFilesNoCheckoutIsError(t *testing.T) {
	f := newGateFixture(t)
	gate, _ := newGate(f, "src/auth", time.Hour)
	resps := drive(t, gate, call(1, "varvig_propose", `{"message":"nothing"}`))
	tr := decodeTool(t, resps[0])
	if !tr.IsError {
		t.Fatal("propose with no files and no checkout must error")
	}
	if code := errCode(t, tr); code != codeInvalidArgs {
		t.Errorf("code = %q, want %q", code, codeInvalidArgs)
	}
}
