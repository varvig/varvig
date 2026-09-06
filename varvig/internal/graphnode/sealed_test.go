package graphnode

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// The two properties below are what make "endpoint class is computed, not
// declared" (GRAPH.md §11.5) true of the code rather than of the documentation.
// Go cannot express either as a compile error on its own, so they are pinned
// here, in the style the repo already uses for the shell-purity guard.

// TestNodeInterfaceIsSealed: Node must carry an unexported method, so no type
// outside this package can implement it. Without it a caller could define a
// fifth class whose Class() returns anything it likes, and every guarantee about
// retention and identity would be advisory.
func TestNodeInterfaceIsSealed(t *testing.T) {
	iface := findInterface(t, "Node")
	sealed := false
	for _, m := range iface.Methods.List {
		for _, name := range m.Names {
			if !name.IsExported() {
				sealed = true
			}
		}
	}
	if !sealed {
		t.Error("Node has no unexported method — any package could implement it and mint a class of its own")
	}
}

// TestNodeFieldsAreUnexported: every node type's fields must be unexported, so a
// node can only come from its validating constructor. An exported field would
// let a writer build one by struct literal — skipping validation, and (worse)
// reintroducing the possibility of a field that disagrees with the type.
func TestNodeFieldsAreUnexported(t *testing.T) {
	for _, name := range []string{"ObjectNode", "IdentityNode", "ExternalNode", "EphemeralNode"} {
		st := findStruct(t, name)
		for _, f := range st.Fields.List {
			for _, fn := range f.Names {
				if fn.IsExported() {
					t.Errorf("%s.%s is exported — a node must be constructible only through its constructor",
						name, fn.Name)
				}
			}
		}
	}
}

func parsePackage(t *testing.T) []*ast.File {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var out []*ast.File
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		out = append(out, af)
	}
	return out
}

func findType(t *testing.T, name string) ast.Expr {
	t.Helper()
	for _, af := range parsePackage(t) {
		for _, d := range af.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if ok && ts.Name.Name == name {
					return ts.Type
				}
			}
		}
	}
	t.Fatalf("type %s not found", name)
	return nil
}

func findInterface(t *testing.T, name string) *ast.InterfaceType {
	t.Helper()
	it, ok := findType(t, name).(*ast.InterfaceType)
	if !ok {
		t.Fatalf("%s is not an interface", name)
	}
	return it
}

func findStruct(t *testing.T, name string) *ast.StructType {
	t.Helper()
	st, ok := findType(t, name).(*ast.StructType)
	if !ok {
		t.Fatalf("%s is not a struct", name)
	}
	return st
}
