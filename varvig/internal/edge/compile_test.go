package edge_test

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"
)

const pkgPath = "github.com/dividebyzero/claude-experiments/varvig/internal/edge"

// typeCheck compiles src against the real edge package and returns the errors it
// produced. It uses the source importer, so it sees this package exactly as the
// compiler would.
func typeCheck(t *testing.T, src string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "snippet.go", src, 0)
	if err != nil {
		t.Fatalf("parse snippet: %v", err)
	}
	var errs []string
	conf := types.Config{
		Importer: importer.ForCompiler(fset, "source", nil),
		Error:    func(err error) { errs = append(errs, err.Error()) },
	}
	_, _ = conf.Check("snippet", fset, []*ast.File{f}, nil)
	return errs
}

// TestPersistingADerivedEdgeDoesNotCompile is the single most important
// structural property in the design (GRAPH.md §11.1, builder G2): a derived edge
// must have no route into the object store, and the failure must be a compile
// error rather than something a reviewer has to catch.
//
// This does not assert it by inspection. It type-checks a program that tries,
// against the real package, and requires the compiler to reject it.
func TestPersistingADerivedEdgeDoesNotCompile(t *testing.T) {
	const src = `package snippet

import (
	"github.com/dividebyzero/claude-experiments/varvig/internal/edge"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

func tryToPersist(r *repo.Repo, d edge.DerivedEdge) {
	_, _ = edge.Put(r, d, "someone", 0)
}
`
	errs := typeCheck(t, src)
	if len(errs) == 0 {
		t.Fatal("a derived edge was accepted by the note writer; §11.1 requires this to be a compile error")
	}
	joined := strings.Join(errs, "; ")
	if !strings.Contains(joined, "DerivedEdge") {
		t.Errorf("compilation failed, but not because of the edge type: %s", joined)
	}
}

// TestEncodingADerivedEdgeDoesNotCompile: the note writer is not the only way
// bytes reach the store. Encode is the other door, and it must be shut too.
func TestEncodingADerivedEdgeDoesNotCompile(t *testing.T) {
	const src = `package snippet

import "github.com/dividebyzero/claude-experiments/varvig/internal/edge"

func tryToEncode(d edge.DerivedEdge) ([]byte, error) {
	return edge.Encode(d)
}
`
	if errs := typeCheck(t, src); len(errs) == 0 {
		t.Fatal("a derived edge was accepted by Encode; it must not be encodable at all")
	}
}

// TestTheControlCompiles proves the two tests above fail for the right reason.
// The identical program with a StoredEdge type-checks cleanly, so the rejection
// is about the derived type and not about a broken snippet or a bad import path.
func TestTheControlCompiles(t *testing.T) {
	const src = `package snippet

import (
	"github.com/dividebyzero/claude-experiments/varvig/internal/edge"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

func persist(r *repo.Repo, s edge.StoredEdge) {
	_, _ = edge.Put(r, s, "someone", 0)
	_, _ = edge.Encode(s)
}
`
	if errs := typeCheck(t, src); len(errs) != 0 {
		t.Fatalf("the StoredEdge control must compile, but did not: %s", strings.Join(errs, "; "))
	}
}
