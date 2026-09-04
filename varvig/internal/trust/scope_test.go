package trust

import "testing"

func TestScopeSetUnionCoversBoth(t *testing.T) {
	s := NewScopeSet("src/auth", "src/api")
	if !s.Covers("src/auth/login.go") {
		t.Error("first scope not covered")
	}
	if !s.Covers("src/api/handler.go") {
		t.Error("second scope not covered — a declared scope was dropped")
	}
	if s.Covers("src/other/x.go") {
		t.Error("an undeclared path is covered")
	}
}

func TestScopeSetCommaListEquivalent(t *testing.T) {
	a := NewScopeSet("src/auth", "src/api")
	b := NewScopeSet("src/auth,src/api")
	if a.String() != b.String() {
		t.Fatalf("comma-list %q != repeated %q", b.String(), a.String())
	}
}

func TestScopeSetDedupsAndSorts(t *testing.T) {
	s := NewScopeSet("src/api", "src/auth", "src/api")
	if len(s) != 2 {
		t.Fatalf("expected 2 unique scopes, got %d (%v)", len(s), s)
	}
	if s[0] >= s[1] {
		t.Errorf("scopes not sorted: %v", s)
	}
}

func TestScopeSetRootCollapses(t *testing.T) {
	s := NewScopeSet("src/auth", "/")
	if len(s) != 1 || s[0] != "/" {
		t.Fatalf("a set containing the whole repo must collapse to {/}, got %v", s)
	}
	if !s.Covers("anything/at/all.go") {
		t.Error("root scope must cover everything")
	}
}

func TestScopeSetEmptyIsWholeRepo(t *testing.T) {
	s := NewScopeSet("")
	if len(s) != 1 || s[0] != "/" {
		t.Fatalf("empty scope = whole repo {/}, got %v", s)
	}
}

func TestScopeSetStringRoundTrips(t *testing.T) {
	s := NewScopeSet("src/api", "src/auth")
	if NewScopeSet(s.String()).String() != s.String() {
		t.Fatalf("String()/NewScopeSet do not round-trip: %q", s.String())
	}
}
