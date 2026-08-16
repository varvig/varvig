package deps

import (
	"reflect"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

func newRepo(t *testing.T) *repo.Repo {
	t.Helper()
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	return r
}

// ticket stores a distinct unmaterialized change and returns its id.
func ticket(t *testing.T, r *repo.Repo, msg string) multihash.Multihash {
	t.Helper()
	id, err := r.Objects.Put(object.NewChange(object.Change{Message: msg, Timestamp: 1, Author: "d"}))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	return id
}

func TestBlocksMirrorsConflict(t *testing.T) {
	// write/write overlap blocks.
	if !Blocks(Scope{Writes: []string{"src/auth"}}, Scope{Writes: []string{"src/auth"}}) {
		t.Fatal("overlapping writes should block")
	}
	// write/read overlap blocks.
	if !Blocks(Scope{Writes: []string{"src/auth"}}, Scope{Reads: []string{"src/auth/token.go"}}) {
		t.Fatal("write over another's read should block")
	}
	// read/read never blocks.
	if Blocks(Scope{Reads: []string{"src/auth"}}, Scope{Reads: []string{"src/auth"}}) {
		t.Fatal("read/read must not block")
	}
	// disjoint paths never block.
	if Blocks(Scope{Writes: []string{"src/web"}}, Scope{Writes: []string{"src/api"}}) {
		t.Fatal("disjoint writes must not block")
	}
}

func TestScopeRoundTrip(t *testing.T) {
	r := newRepo(t)
	id := ticket(t, r, "add rate limiting")

	if _, ok, _ := GetScope(r, id); ok {
		t.Fatal("a fresh ticket should have no scope")
	}
	want := Scope{Reads: []string{"src/auth"}, Writes: []string{"src/auth/ratelimit.go"}}
	if _, err := SetScope(r, id, want, "director", 1); err != nil {
		t.Fatalf("SetScope: %v", err)
	}
	got, ok, err := GetScope(r, id)
	if err != nil || !ok {
		t.Fatalf("GetScope ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scope round-trip: got %+v want %+v", got, want)
	}
}

func TestRescopeNewestWins(t *testing.T) {
	r := newRepo(t)
	id := ticket(t, r, "t")
	if _, err := SetScope(r, id, Scope{Writes: []string{"a"}}, "d", 1); err != nil {
		t.Fatalf("SetScope 1: %v", err)
	}
	if _, err := SetScope(r, id, Scope{Writes: []string{"b"}}, "d", 2); err != nil {
		t.Fatalf("SetScope 2: %v", err)
	}
	got, _, _ := GetScope(r, id)
	if !reflect.DeepEqual(got.Writes, []string{"b"}) {
		t.Fatalf("rescope newest-wins failed: %+v", got)
	}
}

// TestDerivedBlockingGraph is the §3.2/§7.4 guarantee: blocking is derived from
// declared write/read overlap, with no hand-declared links. Three tickets: two
// touch the same path (block each other), the third is disjoint (ready).
func TestDerivedBlockingGraph(t *testing.T) {
	r := newRepo(t)
	a := ticket(t, r, "a: edit auth")
	b := ticket(t, r, "b: also edit auth")
	c := ticket(t, r, "c: edit web")
	mustScope(t, r, a, Scope{Writes: []string{"src/auth"}})
	mustScope(t, r, b, Scope{Reads: []string{"src/auth/token.go"}, Writes: []string{"src/auth/ratelimit.go"}})
	mustScope(t, r, c, Scope{Writes: []string{"src/web"}})

	all, err := ScopedTickets(r)
	if err != nil {
		t.Fatalf("ScopedTickets: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("scoped tickets = %d, want 3", len(all))
	}

	g := Graph(all)
	// a and b block each other (a writes src/auth, b touches under src/auth).
	if !containsID(g[a.Hex()], b) || !containsID(g[b.Hex()], a) {
		t.Fatalf("a and b should block each other: a->%v b->%v", hexes(g[a.Hex()]), hexes(g[b.Hex()]))
	}
	// c is disjoint: blocks nobody and is blocked by nobody.
	if len(g[c.Hex()]) != 0 {
		t.Fatalf("c should be ready, got blockers %v", hexes(g[c.Hex()]))
	}
	if containsID(g[a.Hex()], c) || containsID(g[b.Hex()], c) {
		t.Fatal("disjoint ticket c should not appear as a blocker")
	}
}

func TestBlockersExcludesSelf(t *testing.T) {
	r := newRepo(t)
	a := ticket(t, r, "a")
	mustScope(t, r, a, Scope{Writes: []string{"src/auth"}})
	all, _ := ScopedTickets(r)
	if bl := Blockers(Ticket{ID: a, Scope: Scope{Writes: []string{"src/auth"}}}, all); len(bl) != 0 {
		t.Fatalf("a ticket must not block itself: %v", hexes(bl))
	}
}

func mustScope(t *testing.T, r *repo.Repo, id multihash.Multihash, s Scope) {
	t.Helper()
	if _, err := SetScope(r, id, s, "d", 1); err != nil {
		t.Fatalf("SetScope: %v", err)
	}
}

func containsID(ids []multihash.Multihash, want multihash.Multihash) bool {
	for _, id := range ids {
		if id.Equal(want) {
			return true
		}
	}
	return false
}

func hexes(ids []multihash.Multihash) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.Hex()[:12]
	}
	return out
}
