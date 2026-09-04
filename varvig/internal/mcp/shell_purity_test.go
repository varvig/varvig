package mcp

import (
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
