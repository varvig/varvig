package mcp

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestGateHasNoObjectStoreAccess is the U1 anti-drift guard for the gate shell
// (design addendum): the gate is a thin surface over internal/core and the query
// layer, so it must not construct or decode core objects, nor reach into the
// object store, itself. It parses every non-test source file in this package and
// fails if one imports internal/object or internal/store — so a future tool that
// reintroduces object construction or store access in the gate, instead of
// routing through internal/core, fails the build rather than review.
func TestGateHasNoObjectStoreAccess(t *testing.T) {
	forbidden := map[string]string{
		"internal/object": "construct or decode core objects",
		"internal/store":  "reach into the object store",
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, imp := range af.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			for bad, why := range forbidden {
				if p == "github.com/dividebyzero/claude-experiments/varvig/"+bad {
					t.Errorf("%s imports %s — the gate must not %s; route it through internal/core", f, p, why)
				}
			}
		}
	}
}

// objectMethods are the object-format methods a shell must never call. The
// import guard above cannot see them: a shell that never imports internal/object
// can still call AsChange on a value the store handed back, so "the gate holds no
// object-store vocabulary" was true of its import list and false of its code.
// Each name here had a real call site in the gate that this test now prevents
// from returning.
//
// The replacement is always a helper in internal/core (ChangeTrees, BlobBytes,
// ObjectKind, TreeOf, FirstParent). If a new one is genuinely needed, add it to
// core and call that — do not add a name to this list's exceptions, because the
// absence of exceptions is what makes the rule hold.
var objectMethods = map[string]string{
	"AsChange":      "core.ChangeTrees / core.TreeOf / core.FirstParent",
	"AsNote":        "a core helper",
	"AsAttestation": "a core helper",
	"AsProvenance":  "a core helper",
	"AsHookConfig":  "a core helper",
	"BlobContent":   "core.BlobBytes",
	"TreeEntries":   "the core or readapi tree surface",
	"SignableBytes": "a core helper",
}

// TestGateDecodesNoObjects fails if any non-test file in the gate calls an
// object-format method. This is the U1 property the import guard only half
// covered.
func TestGateDecodesNoObjects(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
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
			if use, bad := objectMethods[sel.Sel.Name]; bad {
				t.Errorf("%s calls %s() — the gate must not decode core objects; use %s",
					fset.Position(sel.Sel.Pos()), sel.Sel.Name, use)
			}
			return true
		})
	}
}
