package main

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestShellHasNoObjectStoreAccess is the U1 anti-drift guard (design addendum):
// the CLI is a thin shell over internal/core, so it must not construct or decode
// core objects, nor reach into the object store. It parses every non-test source
// file in this package and fails if one imports a forbidden low-level package —
// so a future verb that reintroduces object construction or store access in the
// shell, instead of routing through internal/core, fails the build rather than
// review.
func TestShellHasNoObjectStoreAccess(t *testing.T) {
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
					t.Errorf("%s imports %s — a shell must not %s; route it through internal/core", f, p, why)
				}
			}
		}
	}
}
