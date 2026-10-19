package coreguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An assertion may inform an agent's planning; it may never change a merge
// outcome (GRAPH.md §11.3). The design is explicit that the affected-set index
// used for conflict detection, scheduling and promotion consumes derived edges
// only — an unreproducible claim cannot deliver safety, so it must not be
// allowed to withhold it either.
//
// The type system carries part of this already: a consumer that takes
// []edge.DerivedEdge cannot be handed an assertion. What it cannot carry is a
// gating package deciding to read stored edges out of the note store directly,
// which would reintroduce the boundary leak one call at a time. This guard
// closes that.

// gatingPackages decide whether work may proceed: scheduling, conflict
// serialization, ref promotion, blocking dependencies, and speculation scoring.
var gatingPackages = []string{
	"internal/txn",
	"internal/deps",
	"internal/merge",
	"internal/refupdate",
	"internal/spec",
	"internal/score",
}

// forbiddenInGating are the ways a gating package could come to consume a
// non-derived edge. Reading the stored-edge notes at all is the leak: every
// stored edge is imported or asserted, by construction, since a derived edge has
// no stored form.
var forbiddenInGating = map[string]string{
	"edge.List":          "reads stored edges, all of which are imported or asserted",
	"edge.PutOnce":       "writes an edge; gating decides, it does not assert",
	"edge.Put":           "writes an edge; gating decides, it does not assert",
	"assertion.Promoted": "consults promotion; a promoted assertion is still not reproducible",
}

// TestGatingNeverConsumesAssertions fails if a package that decides whether work
// may proceed reads or writes stored edges.
func TestGatingNeverConsumesAssertions(t *testing.T) {
	root := moduleRoot(t)
	var violations []string

	for _, pkg := range gatingPackages {
		dir := filepath.Join(root, pkg)
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("gating package %s not found; the guard is checking nothing", pkg)
			continue
		}
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			fset := token.NewFileSet()
			af, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(af, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkgIdent, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				name := pkgIdent.Name + "." + sel.Sel.Name
				if why, bad := forbiddenInGating[name]; bad {
					violations = append(violations, rel+":"+
						itoa(fset.Position(sel.Pos()).Line)+": calls "+name+" — "+why)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", pkg, err)
		}
	}

	if len(violations) > 0 {
		t.Fatalf("an assertion must never change a merge outcome (GRAPH.md §11.3); "+
			"gating consumes derived edges only. Found %d:\n%s",
			len(violations), strings.Join(violations, "\n"))
	}
}
