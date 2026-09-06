package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/trust"
	"github.com/dividebyzero/claude-experiments/varvig/internal/worktree"
)

// These are the code-level assertions of the checkout-scope regression suite
// (builder doc, "Regression suite"): the parts that can be pinned as unit tests,
// as opposed to the agent-harness metrics (boundary-hit counts across first-run
// tasks). Two things it names specifically — the write-only confinement boundary,
// and the two first-run near-misses being visible in a diff before proposal.

// oneFileDiff renders core.Diff (the exact function both shells present verbatim)
// over two one-file trees built from the given contents.
func oneFileDiff(t *testing.T, r *repo.Repo, path, before, after string) DiffResult {
	t.Helper()
	build := func(content string) map[string]worktree.FileState {
		h, err := PutBlob(r, []byte(content))
		if err != nil {
			t.Fatal(err)
		}
		return map[string]worktree.FileState{path: {Hash: h, Mode: 0o644}}
	}
	base, work := build(before), build(after)
	reader := func(states map[string]worktree.FileState) BytesFn {
		return func(p string) ([]byte, bool, error) {
			s, ok := states[p]
			if !ok || s.Hash == nil {
				return nil, false, nil
			}
			o, err := r.Objects.Get(s.Hash)
			if err != nil {
				return nil, false, err
			}
			c, ok := o.BlobContent()
			return c, ok, nil
		}
	}
	return Diff(base, work, reader(base), reader(work))
}

// TestDiffSurfacesReorderedImportOffLineOne: a reorder that pushes a file-level
// directive (which is only honored on line 1) below the imports is a silent
// breakage — and a diff must make it visible before the change is proposed
// (builder doc regression, first near-miss).
func TestDiffSurfacesReorderedImportOffLineOne(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	before := "//go:build linux\n\npackage p\n\nimport (\n\t\"a\"\n\t\"b\"\n)\n"
	// A reformat that moved the build directive off line 1 (now line 2).
	after := "\n//go:build linux\npackage p\n\nimport (\n\t\"b\"\n\t\"a\"\n)\n"
	res := oneFileDiff(t, r, "p.go", before, after)

	if len(res.Changed) != 1 || res.Changed[0] != "p.go" {
		t.Fatalf("changed = %v, want [p.go]", res.Changed)
	}
	// The moved directive is visible in the diff: the line-1 form is removed.
	if !strings.Contains(res.Unified, "-//go:build linux") {
		t.Fatalf("diff did not surface the directive leaving line 1:\n%s", res.Unified)
	}
}

// TestDiffSurfacesRemovedClosingBrace: an eaten closing brace is the second
// first-run near-miss; the diff must show the deletion before proposal,
// independent of whether a linter later catches it.
func TestDiffSurfacesRemovedClosingBrace(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	before := "func f() {\n\tg()\n}\n"
	after := "func f() {\n\tg()\n" // closing brace eaten
	res := oneFileDiff(t, r, "p.go", before, after)

	if len(res.Changed) != 1 || res.Changed[0] != "p.go" {
		t.Fatalf("changed = %v, want [p.go]", res.Changed)
	}
	if !strings.Contains(res.Unified, "-}") {
		t.Fatalf("diff did not surface the removed closing brace:\n%s", res.Unified)
	}
}

// TestFullCheckoutConfinesWritesNotReads is the Option B boundary, stated as one
// regression: in a full task checkout an out-of-scope path is present and
// readable (reads never miss — read-gating is gone), while a change to an
// out-of-scope path is refused at proposal, by path (the write set is the
// boundary). "Scope boundary hits are write-only" (builder doc regression).
func TestFullCheckoutConfinesWritesNotReads(t *testing.T) {
	src, base, _ := srcFixture(t)
	dir := filepath.Join(t.TempDir(), "task")
	if _, _, err := ProvisionCheckout(src, dir, base); err != nil {
		t.Fatal(err)
	}

	// READ: a path outside the task's scope is materialized in the checkout and
	// reads without a miss — the checkout is an ordinary, whole-tree repo.
	if _, err := os.ReadFile(filepath.Join(dir, "README.md")); err != nil {
		t.Fatalf("an out-of-scope path is not readable in a full checkout — read-gating survived: %v", err)
	}

	// WRITE: the same out-of-scope path, changed, is refused at proposal by path.
	scope := trust.NewScopeSet("src")
	d := worktree.TreeDiff{Modified: []string{"README.md"}}
	if _, err := worktree.SelectEdits(d, scope.Covers, scope.String(), nil); err == nil {
		t.Fatal("a write outside the declared scope must be refused, naming the path")
	} else if !strings.Contains(err.Error(), "README.md") {
		t.Fatalf("refusal should name the out-of-scope path, got %v", err)
	}

	// And a write within scope is allowed — the boundary is the write set, not the
	// tree's shape.
	din := worktree.TreeDiff{Modified: []string{"src/login.go"}}
	if _, err := worktree.SelectEdits(din, scope.Covers, scope.String(), nil); err != nil {
		t.Fatalf("an in-scope write must be allowed: %v", err)
	}
}
