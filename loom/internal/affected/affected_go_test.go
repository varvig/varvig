package affected

import (
	"reflect"
	"testing"
)

// TestGoPackageImports resolves Go package-path imports to the repo files of
// the imported package, using the module path from go.mod.
func TestGoPackageImports(t *testing.T) {
	r, tree := treeFromFiles(t, map[string]string{
		"go.mod": "module example.com/proj\n\ngo 1.24\n",
		"main.go": `package main
import (
	"fmt"
	"example.com/proj/pkg/a"
)
func main() { fmt.Println(a.X) }
`,
		"pkg/a/a.go": `package a
import "example.com/proj/pkg/b"
var X = b.Y
`,
		"pkg/b/b.go":       "package b\n\nvar Y = 1\n",
		"pkg/b/extra.go":   "package b\n\nvar Z = 2\n", // same package: also a dep of a
		"pkg/unrelated.go": "package unrelated\n",
	})
	g, err := BuildGraph(r.Objects, tree, Options{})
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}

	// main imports package a -> the file(s) of package a.
	if got := g.Deps("main.go"); !reflect.DeepEqual(got, []string{"pkg/a/a.go"}) {
		t.Fatalf("main deps = %v, want [pkg/a/a.go]", got)
	}
	// a imports package b -> every .go file in pkg/b.
	if got := g.Deps("pkg/a/a.go"); !reflect.DeepEqual(got, []string{"pkg/b/b.go", "pkg/b/extra.go"}) {
		t.Fatalf("a deps = %v, want both pkg/b files", got)
	}
	// Changing one file of package b affects its dependents (a, then main) —
	// not its sibling b.go, which no one imports through extra.go. The external
	// "fmt" import and the unrelated package create no edges.
	got := g.Affected([]string{"pkg/b/extra.go"})
	want := []string{"main.go", "pkg/a/a.go", "pkg/b/extra.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("affected = %v, want %v", got, want)
	}
	if contains(got, "pkg/unrelated.go") {
		t.Fatal("unrelated package wrongly affected")
	}
}

// TestGoWithoutModule degrades gracefully: no go.mod means no package
// resolution, so a changed Go file affects only itself (no false edges).
func TestGoWithoutModule(t *testing.T) {
	r, tree := treeFromFiles(t, map[string]string{
		"main.go":    "package main\nimport \"example.com/proj/pkg/a\"\n",
		"pkg/a/a.go": "package a\n",
	})
	g, err := BuildGraph(r.Objects, tree, Options{})
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	if got := g.Deps("main.go"); len(got) != 0 {
		t.Fatalf("deps = %v, want none without a module path", got)
	}
}
