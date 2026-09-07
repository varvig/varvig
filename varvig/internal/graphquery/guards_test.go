package graphquery

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// The guards here pin the two properties GRAPH.md §11.4 asks for as type-level
// facts. Go gets partway on its own — unexported fields make a Result
// unconstructible by literal — and these cover the rest, in the style the repo
// already uses for shell purity and vendor neutrality.

func parseNonTest(t *testing.T) (*token.FileSet, []*ast.File) {
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
	return fset, out
}

// TestNoResultIsConstructedWithoutCoverage: every Result composite literal must
// be inside newResult, the one constructor that requires coverage. A Result{}
// built anywhere else would carry the zero Coverage — which reads as "nothing
// analyzed, no gaps", the most confidently wrong descriptor available.
func TestNoResultIsConstructedWithoutCoverage(t *testing.T) {
	fset, files := parseNonTest(t)
	for _, af := range files {
		var fn string
		ast.Inspect(af, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.FuncDecl:
				fn = v.Name.Name
			case *ast.CompositeLit:
				id, ok := v.Type.(*ast.Ident)
				if !ok || id.Name != "Result" {
					return true
				}
				// The empty Result{} accompanying an error return carries no
				// answer and is never read; a populated one outside the
				// constructor is the violation.
				if len(v.Elts) == 0 {
					return true
				}
				if fn != "newResult" {
					t.Errorf("%s: %s builds a populated Result outside newResult; "+
						"coverage must be a required constructor argument, not an optional field",
						fset.Position(v.Pos()), fn)
				}
			}
			return true
		})
	}
}

// TestAnswerHasNoBooleanAccessor: Answer must expose no method returning a bare
// bool except PresentOr.
//
// A Present() bool would read as a fact and be the wrong one a third of the
// time: it collapses "an analyzer looked and found nothing" together with
// "nothing has ever looked". PresentOr survives because its signature forces the
// caller to name what an unknown means for their purpose, at the call site where
// the consequence lives.
func TestAnswerHasNoBooleanAccessor(t *testing.T) {
	const allowed = "PresentOr"
	fset, files := parseNonTest(t)
	for _, af := range files {
		for _, d := range af.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 {
				continue
			}
			if recvTypeName(fn.Recv.List[0].Type) != "Answer" || !fn.Name.IsExported() {
				continue
			}
			if fn.Name.Name == allowed {
				continue
			}
			if fn.Type.Results == nil {
				continue
			}
			for _, res := range fn.Type.Results.List {
				if id, ok := res.Type.(*ast.Ident); ok && id.Name == "bool" {
					t.Errorf("%s: Answer.%s returns a bool; only %s may, because only its "+
						"signature makes the caller say what an unknown means",
						fset.Position(fn.Pos()), fn.Name.Name, allowed)
				}
			}
		}
	}
}

// TestQueryTypesHaveNoExportedFields: Answer and Result must be constructible
// only through their constructors, so their invariants cannot be sidestepped by
// struct literal.
func TestQueryTypesHaveNoExportedFields(t *testing.T) {
	_, files := parseNonTest(t)
	want := map[string]bool{"Answer": true, "Result": true}
	found := map[string]bool{}
	for _, af := range files {
		for _, d := range af.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || !want[ts.Name.Name] {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					t.Fatalf("%s is not a struct", ts.Name.Name)
				}
				found[ts.Name.Name] = true
				for _, f := range st.Fields.List {
					for _, fn := range f.Names {
						if fn.IsExported() {
							t.Errorf("%s.%s is exported; it must come from its constructor",
								ts.Name.Name, fn.Name)
						}
					}
				}
			}
		}
	}
	for name := range want {
		if !found[name] {
			t.Errorf("type %s not found; the guard is checking nothing", name)
		}
	}
}

func recvTypeName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.StarExpr:
		return recvTypeName(v.X)
	}
	return ""
}
