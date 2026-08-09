package task

import (
	"testing"
	"time"
)

func TestNewGrantMintsEphemeralScopedKey(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	g, err := New("src/auth", true, time.Hour, now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !g.ProposeOnly {
		t.Fatal("grant must be propose-only")
	}
	if g.NotAfter != now.Add(time.Hour).Unix() {
		t.Fatalf("NotAfter = %d, want %d", g.NotAfter, now.Add(time.Hour).Unix())
	}
	if g.Fingerprint() == "" {
		t.Fatal("grant has no fingerprint")
	}
	if len(g.ID) != 8 {
		t.Fatalf("ID = %q, want 8 hex chars", g.ID)
	}
	// Scope must be the read set: src/auth covers itself, not a sibling.
	if !g.Covers("src/auth/login.go") {
		t.Error("grant should cover a path inside its scope")
	}
	if g.Covers("src/web/index.html") {
		t.Error("grant must not cover a path outside its scope")
	}
	if g.Covers("src/authz/x") {
		t.Error("scope must respect component boundaries (src/auth ≠ src/authz)")
	}
}

func TestNewGrantRootScopeCoversEverything(t *testing.T) {
	g, err := New("/", true, time.Minute, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !g.Covers("anything/at/all.txt") {
		t.Error("root scope must cover every path")
	}
}

func TestNewGrantRejectsPromote(t *testing.T) {
	// A task key can never promote (§6.2); a non-propose-only grant is refused.
	if _, err := New("/", false, time.Hour, time.Unix(0, 0)); err == nil {
		t.Fatal("New must reject a non-propose-only grant")
	}
}

func TestNewGrantRejectsNonPositiveTTL(t *testing.T) {
	if _, err := New("/", true, 0, time.Unix(0, 0)); err == nil {
		t.Fatal("New must reject a non-positive ttl")
	}
}

func TestGrantSignerRoundTrips(t *testing.T) {
	g, err := New("/", true, time.Hour, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s := g.Signer()
	msg := []byte("proposal bytes")
	sig, err := s.Sign(msg)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	// The signer's public key is the grant's ephemeral key.
	if got := s.Public(); string(got) != string(g.PublicKey().Key) {
		t.Fatal("signer public key differs from grant key")
	}
	if len(sig) == 0 {
		t.Fatal("empty signature")
	}
}

func TestGrantValidityFollowsExpiry(t *testing.T) {
	now := time.Unix(500, 0)
	g, err := New("/", true, 100*time.Second, now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !g.Valid(now) {
		t.Error("grant should be valid at mint time")
	}
	if !g.Valid(time.Unix(600, 0)) {
		t.Error("grant should be valid exactly at not_after")
	}
	if g.Valid(time.Unix(601, 0)) {
		t.Error("grant should be invalid past not_after")
	}
}

func TestTableGetPrunesExpired(t *testing.T) {
	tbl := NewTable()
	now := time.Unix(0, 0)
	g, err := New("/", true, time.Second, now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tbl.Add(g)

	if _, ok := tbl.Get(g.ID, now); !ok {
		t.Fatal("grant should be retrievable while valid")
	}
	// After expiry, Get reports absent and drops the entry — expiry is the
	// revocation mechanism (§6.2).
	if _, ok := tbl.Get(g.ID, time.Unix(2, 0)); ok {
		t.Fatal("expired grant must not be retrievable")
	}
	if len(tbl.grants) != 0 {
		t.Fatal("expired grant should have been pruned from the table")
	}
}

func TestTableActiveFiltersExpired(t *testing.T) {
	tbl := NewTable()
	base := time.Unix(0, 0)
	live, _ := New("/", true, time.Hour, base)
	dead, _ := New("/", true, time.Second, base)
	tbl.Add(live)
	tbl.Add(dead)

	active := tbl.Active(time.Unix(10, 0))
	if len(active) != 1 || active[0].ID != live.ID {
		t.Fatalf("Active returned %d grants, want only the live one", len(active))
	}
}

func TestReadSetDedupsPreservingOrder(t *testing.T) {
	rs := NewReadSet()
	rs.Record("a")
	rs.Record("b")
	rs.Record("a") // dup
	rs.Record("")  // ignored
	rs.Record("c")
	got := rs.Hashes()
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("Hashes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Hashes = %v, want %v", got, want)
		}
	}
	if rs.Len() != 3 {
		t.Fatalf("Len = %d, want 3", rs.Len())
	}
}
