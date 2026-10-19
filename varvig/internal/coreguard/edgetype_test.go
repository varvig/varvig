package coreguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The edge-type vocabulary must not ossify (GRAPH.md §11.2). The core stores,
// syncs, and retains edges without ever looking at what an edge type says; only
// analyzer modules and query callers interpret it. There is no registry and no
// enumeration, and the absence of a list is the mitigation — "resist the pressure
// to add types" would not survive contact with a deadline, but "there is no list
// to add to" does.
//
// This guard makes that mechanical. If core behavior ever needs to switch on an
// edge type, that is the ossification event, and the build should stop so the
// change is refused deliberately rather than merged quietly (builder §5: report
// it, do not resolve it locally).

// edgeTypeLiteral matches a producer-qualified edge type: "producer:verb". The
// leading letter requirement keeps it away from things that merely contain a
// colon — a "https://…" URL (the colon is followed by a slash), a "15:04:05"
// time layout (leading digit), or a bare prefix like "analyze:" (no verb).
var edgeTypeLiteral = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]*:[A-Za-z][A-Za-z0-9_.-]*$`)

// TestCoreNeverBranchesOnEdgeType fails if any core file compares a value
// against an edge-type-shaped literal, or uses one as a switch case.
func TestCoreNeverBranchesOnEdgeType(t *testing.T) {
	root := moduleRoot(t)
	var violations []string

	for _, sub := range scanRoots {
		err := filepath.WalkDir(filepath.Join(root, sub), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			// The guard names the shape by necessity; edge tests construct edge
			// types as data, which is use, not a branch.
			if strings.Contains(path, "coreguard") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			fset := token.NewFileSet()
			af, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(af, func(n ast.Node) bool {
				switch v := n.(type) {
				case *ast.BinaryExpr:
					if v.Op != token.EQL && v.Op != token.NEQ {
						return true
					}
					for _, side := range []ast.Expr{v.X, v.Y} {
						if lit, ok := edgeTypeString(side); ok {
							violations = append(violations, rel+":"+
								itoa(fset.Position(side.Pos()).Line)+
								": compares against edge type "+lit)
						}
					}
				case *ast.CaseClause:
					for _, e := range v.List {
						if lit, ok := edgeTypeString(e); ok {
							violations = append(violations, rel+":"+
								itoa(fset.Position(e.Pos()).Line)+
								": switch case on edge type "+lit)
						}
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", sub, err)
		}
	}

	if len(violations) > 0 {
		t.Fatalf("the core must never branch on an edge-type value (GRAPH.md §11.2) — "+
			"this is the ossification event; report it rather than resolving it locally. Found %d:\n%s",
			len(violations), strings.Join(violations, "\n"))
	}
}

// edgeTypeString reports whether e is a string literal shaped like an edge type.
func edgeTypeString(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s := strings.Trim(lit.Value, "`\"")
	if !edgeTypeLiteral.MatchString(s) {
		return "", false
	}
	return s, true
}
