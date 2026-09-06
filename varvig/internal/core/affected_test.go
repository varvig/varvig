package core

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// flatTree stores a flat tree of files (path -> content) plus a change over it,
// and returns the change id.
func flatTree(t *testing.T, r *repo.Repo, files map[string]string, parents ...multihash.Multihash) multihash.Multihash {
	t.Helper()
	entries := make([]object.Entry, 0, len(files))
	for name, content := range files {
		id, err := r.Objects.Put(object.NewBlob([]byte(content)))
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, object.Entry{
			Name: name, Mode: 0o100644, Kind: object.TypeBlob, ID: id,
		})
	}
	tree, err := r.Objects.Put(object.NewTree(entries))
	if err != nil {
		t.Fatal(err)
	}
	ch, err := r.Objects.Put(object.NewChange(object.Change{
		Tree: tree, Parents: parents, Message: "m", Timestamp: 1,
	}))
	if err != nil {
		t.Fatal(err)
	}
	return ch
}

// TestAffectedPullsInDependents is the property the CLI verb has always had and
// the gate could not reach: a changed file drags its importers in with it.
// Path-style imports are used because they resolve to a single file, so a flat
// tree is enough to exercise a real edge.
func TestAffectedPullsInDependents(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := flatTree(t, r, map[string]string{
		"lib.js":   "export const x = 1\n",
		"app.js":   "import { x } from './lib.js'\n",
		"other.js": "export const y = 2\n",
	})
	next := flatTree(t, r, map[string]string{
		"lib.js":   "export const x = 2\n", // edited
		"app.js":   "import { x } from './lib.js'\n",
		"other.js": "export const y = 2\n",
	}, base)

	res, err := Affected(r, base, next)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changed) != 1 || res.Changed[0] != "lib.js" {
		t.Fatalf("Changed = %v, want [lib.js]", res.Changed)
	}
	// app.js imports lib.js, so editing lib.js affects app.js too.
	if !contains(res.Affected, "app.js") {
		t.Fatalf("Affected = %v, want app.js pulled in as a dependent of lib.js", res.Affected)
	}
	if !res.Pulled("app.js") {
		t.Error("Pulled(app.js) = false; it is in the set by dependency, not by edit")
	}
	// A changed file is never "pulled in" — it was edited directly.
	if res.Pulled("lib.js") {
		t.Error("Pulled(lib.js) = true for a directly changed file")
	}
	// An unrelated file is not affected at all.
	if contains(res.Affected, "other.js") {
		t.Errorf("Affected = %v, must not contain the unrelated other.js", res.Affected)
	}
}

// TestAffectedNilBaseIsAllAdditions: a nil base is the empty tree, so every file
// in the new tree is an addition rather than an error.
func TestAffectedNilBaseIsAllAdditions(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ch := flatTree(t, r, map[string]string{"a.go": "package a\n", "b.go": "package b\n"})
	res, err := Affected(r, nil, ch)
	if err != nil {
		t.Fatalf("Affected with a nil base must succeed: %v", err)
	}
	if len(res.Changed) != 2 {
		t.Fatalf("Changed = %v, want both files as additions", res.Changed)
	}
}

// TestAffectedIsIncrementalAndRebuildable pins the §4.3 property the whole design
// rests on: the index is a cache. Deleting it entirely must change no answer.
func TestAffectedIsIncrementalAndRebuildable(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := flatTree(t, r, map[string]string{"a.go": "package a\n"})
	next := flatTree(t, r, map[string]string{"a.go": "package a\n// edit\n", "b.go": "package b\n"}, base)

	first, err := Affected(r, base, next)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(IndexDir(r), "deps")); err != nil {
		t.Fatalf("the specifier index was not written to IndexDir: %v", err)
	}

	// Warm: the same answer must come back off the cache.
	warm, err := Affected(r, base, next)
	if err != nil {
		t.Fatal(err)
	}
	if !equal(first.Affected, warm.Affected) {
		t.Errorf("warm run disagreed with cold: %v vs %v", warm.Affected, first.Affected)
	}

	// Cold: delete the whole index and rebuild from scratch. Nothing is lost.
	if err := os.RemoveAll(IndexDir(r)); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := Affected(r, base, next)
	if err != nil {
		t.Fatalf("rebuild after deleting the index failed: %v", err)
	}
	if !equal(first.Affected, rebuilt.Affected) {
		t.Errorf("from-scratch rebuild lost information: %v vs %v", rebuilt.Affected, first.Affected)
	}
	if !equal(first.Changed, rebuilt.Changed) {
		t.Errorf("from-scratch rebuild changed the change set: %v vs %v", rebuilt.Changed, first.Changed)
	}
}

// TestAnalyzersEmptyWithoutManifest: a repo that has registered no analyzer
// reports an empty analyzer set rather than failing. The built-in extractors
// still run, so the graph is not empty — the analyzer set records only what was
// supplied as wasm.
func TestAnalyzersEmptyWithoutManifest(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	as, err := Analyzers(r)
	if err != nil {
		t.Fatalf("Analyzers on a repo with no hook manifest: %v", err)
	}
	if len(as) != 0 {
		t.Errorf("Analyzers = %v, want none registered", as)
	}
	ch := flatTree(t, r, map[string]string{"a.go": "package a\n"})
	res, err := Affected(r, nil, ch)
	if err != nil {
		t.Fatal(err)
	}
	if res.Analyzers != nil && len(res.Analyzers) != 0 {
		t.Errorf("AffectedResult.Analyzers = %v, want empty", res.Analyzers)
	}
}

// TestFirstParent covers the three cases a shell needs and cannot compute itself.
func TestFirstParent(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := flatTree(t, r, map[string]string{"a.go": "package a\n"})
	child := flatTree(t, r, map[string]string{"a.go": "package a\n// e\n"}, root)

	got, err := FirstParent(r, child)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || !got.Equal(root) {
		t.Errorf("FirstParent(child) = %v, want %v", got, root)
	}

	got, err = FirstParent(r, root)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("FirstParent(root change) = %v, want nil", got)
	}

	if got, err = FirstParent(r, nil); err != nil || got != nil {
		t.Errorf("FirstParent(nil) = %v, %v; want nil, nil", got, err)
	}

	// A tree is not a change: no parent, and not an error either.
	tree, err := TreeOf(r, root)
	if err != nil {
		t.Fatal(err)
	}
	if got, err = FirstParent(r, tree); err != nil || got != nil {
		t.Errorf("FirstParent(tree) = %v, %v; want nil, nil", got, err)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
