package mcp

import (
	"testing"
	"time"
)

// P0.5: a task declared with two scopes may read and propose within either; a
// path outside every scope is still refused. Exercises the write path (Covers)
// and the read path (the reachability union).
func TestProposeAndReadAcrossUnionedScopes(t *testing.T) {
	f := newGateFixture(t)
	gate, grant := newGate(f, "src/auth,src/web", time.Hour)
	if !grant.Covers("src/auth/x.go") || !grant.Covers("src/web/y.html") {
		t.Fatal("a unioned grant must cover both declared scopes")
	}
	if grant.Covers("src/other/z.go") {
		t.Fatal("a unioned grant must not cover an undeclared path")
	}

	// Propose into the *second* scope — the one a last-wins bug would have dropped.
	tr := decodeTool(t, drive(t, gate, call(1, "varvig_propose",
		`{"message":"add page","files":[{"path":"src/web/new.html","content":"<p>hi</p>\n"}]}`))[0])
	if tr.IsError {
		t.Fatalf("propose into a unioned scope errored: %s", tr.StructuredContent)
	}

	// Read a file in the second scope — reachability must be the union of subtrees.
	rf := decodeTool(t, drive(t, gate, call(2, "varvig_read_file", `{"path":"src/web/index.html"}`))[0])
	if rf.IsError {
		t.Fatalf("reading a file in the second scope errored: %s", rf.StructuredContent)
	}

	// A path outside every scope is refused.
	esc := decodeTool(t, drive(t, gate, call(3, "varvig_propose",
		`{"message":"escape","files":[{"path":"src/other/x.go","content":"x\n"}]}`))[0])
	if !esc.IsError {
		t.Fatal("a path outside every declared scope must be refused")
	}
	if c := errCode(t, esc); c != codeOutOfScope {
		t.Errorf("escape code = %q, want %q", c, codeOutOfScope)
	}
}
