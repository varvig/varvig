package core

import (
	"fmt"
	"os"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/edge"
	"github.com/dividebyzero/claude-experiments/varvig/internal/notes"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/reserved"
)

// fingerprint renders a derived edge for comparison. It lives in the test, not
// on the type: giving DerivedEdge an encoder would be the first step toward
// something that can write one down, and there must be no such step.
func fingerprint(e edge.DerivedEdge) string {
	return fmt.Sprintf("%s|%s|%s|%s|%s|%v",
		e.Type(), e.SourcePath(), e.TargetPath(),
		e.Source().Key(), e.Target().Key(), e.ObservedUnder().Hex())
}

func fingerprints(es []edge.DerivedEdge) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = fingerprint(e)
	}
	return out
}

// corpus is the set of trees the rebuild invariants are checked against. Each
// exercises something different: a plain chain, a diamond, a cycle, a file that
// imports nothing, and a language no analyzer covers.
func corpus() []map[string]string {
	return []map[string]string{
		{ // a chain
			"a.js": "import './b.js'\n",
			"b.js": "import './c.js'\n",
			"c.js": "export const c = 1\n",
		},
		{ // a diamond
			"top.js":  "import './l.js'\nimport './r.js'\n",
			"l.js":    "import './base.js'\n",
			"r.js":    "import './base.js'\n",
			"base.js": "export const b = 1\n",
		},
		{ // a cycle: the closure must terminate
			"x.js": "import './y.js'\n",
			"y.js": "import './x.js'\n",
		},
		{ // leaves only
			"one.js": "export const a = 1\n",
			"two.js": "export const b = 2\n",
		},
		{ // mixed, including a language nothing covers
			"app.ts":  "import './util.ts'\n",
			"util.ts": "export const u = 1\n",
			"main.rb": "require_relative 'other'\n",
			"README":  "docs\n",
		},
		{}, // the empty tree
	}
}

// TestDeletingTheIndexLosesNothing is G4's headline acceptance and the invariant
// the whole design rests on (§4.3, §11.1): the index is a cache. Deleting it
// entirely and rebuilding from scratch must produce byte-identical edges and
// lose none.
//
// Rebuild is meant to be routine rather than exceptional. A rebuild path that is
// never exercised is one that has quietly stopped working, and "the index is
// disposable" would be aspiration rather than fact.
func TestDeletingTheIndexLosesNothing(t *testing.T) {
	for i, files := range corpus() {
		t.Run(fmt.Sprintf("tree%d", i), func(t *testing.T) {
			r, err := repo.Init(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ch := flatTree(t, r, files)

			warm, err := DerivedEdges(r, ch)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(IndexDir(r)); err != nil {
				t.Fatal(err)
			}
			cold, err := DerivedEdges(r, ch)
			if err != nil {
				t.Fatalf("rebuild after deleting the index failed: %v", err)
			}

			if len(cold.Edges) != len(warm.Edges) {
				t.Fatalf("rebuild produced %d edges, want %d", len(cold.Edges), len(warm.Edges))
			}
			w, c := fingerprints(warm.Edges), fingerprints(cold.Edges)
			for j := range w {
				if w[j] != c[j] {
					t.Errorf("edge %d differs after rebuild:\n  warm: %s\n  cold: %s", j, w[j], c[j])
				}
			}
			if cold.Coverage.Analyzed != warm.Coverage.Analyzed ||
				cold.Coverage.Unanalyzed != warm.Coverage.Unanalyzed ||
				!equal(cold.Coverage.UnanalyzedExts, warm.Coverage.UnanalyzedExts) {
				t.Errorf("coverage differs after rebuild: %+v vs %+v", cold.Coverage, warm.Coverage)
			}
		})
	}
}

// TestIncrementalAndFromScratchAgree: the incremental index (warmed by an
// earlier, different query) and a from-scratch one must agree. Drift between
// them is the failure that would otherwise accumulate for months before anyone
// noticed.
func TestIncrementalAndFromScratchAgree(t *testing.T) {
	for i, files := range corpus() {
		t.Run(fmt.Sprintf("tree%d", i), func(t *testing.T) {
			// Incremental: a repo whose index was warmed by an unrelated tree
			// first, so entries carry over.
			inc, err := repo.Init(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			warmup := flatTree(t, inc, map[string]string{
				"warm.js":  "import './files.js'\n",
				"files.js": "export const w = 1\n",
			})
			if _, err := DerivedEdges(inc, warmup); err != nil {
				t.Fatal(err)
			}
			target := flatTree(t, inc, files)
			incremental, err := DerivedEdges(inc, target)
			if err != nil {
				t.Fatal(err)
			}

			// From scratch: a fresh repo with the same tree and no index at all.
			fresh, err := repo.Init(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			freshTarget := flatTree(t, fresh, files)
			scratch, err := DerivedEdges(fresh, freshTarget)
			if err != nil {
				t.Fatal(err)
			}

			a, b := fingerprints(incremental.Edges), fingerprints(scratch.Edges)
			if len(a) != len(b) {
				t.Fatalf("incremental has %d edges, from-scratch %d:\n  %v\n  %v", len(a), len(b), a, b)
			}
			for j := range a {
				if a[j] != b[j] {
					t.Errorf("edge %d differs:\n  incremental:  %s\n  from-scratch: %s", j, a[j], b[j])
				}
			}
		})
	}
}

// TestComputingDerivedEdgesWritesNoNotes is the standing invariant of §11.1,
// checked at runtime rather than only in the type system: computing derived
// edges must leave the object store with zero edge notes.
//
// The type separation already makes this unwritable, and StoredEdge cannot even
// spell derived provenance — but the invariant is cheap to check and it also
// catches a future caller that reaches around the types.
func TestComputingDerivedEdgesWritesNoNotes(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ch := flatTree(t, r, map[string]string{
		"a.js": "import './b.js'\n",
		"b.js": "export const b = 1\n",
	})
	res, err := DerivedEdges(r, ch)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Edges) == 0 {
		t.Fatal("the fixture produced no edges; the invariant would be vacuous")
	}

	refs, err := r.Refs.List()
	if err != nil {
		t.Fatal(err)
	}
	prefix := "refs/notes/" + reserved.NoteEdge + "/"
	for _, name := range refs {
		if len(name) > len(prefix) && name[:len(prefix)] == prefix {
			t.Errorf("computing derived edges wrote an edge note: %s", name)
		}
	}

	// And directly on the change, in case a note landed somewhere the prefix
	// scan would not have looked.
	if chain, err := notes.New(r).List(reserved.NoteEdge, ch); err != nil {
		t.Fatal(err)
	} else if len(chain) != 0 {
		t.Errorf("%d edge notes landed on the change", len(chain))
	}
}

// TestDerivedEdgeCarriesItsReproductionRecipe: a derived edge's warrant is that
// anyone can recompute it, so it must name the analyzers that produced it and
// the tree they ran against.
func TestDerivedEdgeCarriesItsReproductionRecipe(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ch := flatTree(t, r, map[string]string{
		"a.js": "import './b.js'\n",
		"b.js": "export const b = 1\n",
	})
	tree, err := TreeOf(r, ch)
	if err != nil {
		t.Fatal(err)
	}
	res, err := DerivedEdges(r, ch)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Edges) != 1 {
		t.Fatalf("got %d edges, want 1", len(res.Edges))
	}
	e := res.Edges[0]
	if !e.ObservedUnder().Equal(tree) {
		t.Errorf("ObservedUnder = %s, want the analyzed tree %s", e.ObservedUnder().Hex(), tree.Hex())
	}
	if e.Type() != EdgeTypeImportsFile {
		t.Errorf("edge type = %q, want %q", e.Type(), EdgeTypeImportsFile)
	}
	if e.SourcePath() != "a.js" || e.TargetPath() != "b.js" {
		t.Errorf("resolution = %s -> %s, want a.js -> b.js", e.SourcePath(), e.TargetPath())
	}
	// The endpoints are content hashes, not paths: nothing can change underneath
	// the edge.
	if e.Source().Key() == e.SourcePath() {
		t.Error("the endpoint is a bare path; it must be a content-addressed node")
	}
}
