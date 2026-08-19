package score

import (
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/bridge"
	"github.com/dividebyzero/claude-experiments/varvig/internal/deps"
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

func ticket(t *testing.T, r *repo.Repo, msg string, ts int64) multihash.Multihash {
	t.Helper()
	id, err := r.Objects.Put(object.NewChange(object.Change{Message: msg, Timestamp: ts, Author: "d"}))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	return id
}

// TestExtractFeatures checks features come from repo state: blast radius from
// the write set, contention from the derived dependency graph, age from the
// ticket's timestamp.
func TestExtractFeatures(t *testing.T) {
	r := newRepo(t)
	a := ticket(t, r, "a", 100)
	b := ticket(t, r, "b", 100)
	c := ticket(t, r, "c", 100)
	// a and b share src/auth (conflict); c is disjoint.
	tickets := []deps.Ticket{
		{ID: a, Scope: deps.Scope{Writes: []string{"src/auth", "src/auth/x.go"}}},
		{ID: b, Scope: deps.Scope{Writes: []string{"src/auth/y.go"}}},
		{ID: c, Scope: deps.Scope{Writes: []string{"src/web"}}},
	}

	fa := ExtractFeatures(r, tickets[0], tickets, 400)
	if fa.BlastRadius != 2 {
		t.Fatalf("a blast radius = %v, want 2", fa.BlastRadius)
	}
	if fa.Unblocks != 1 { // conflicts only with b
		t.Fatalf("a unblocks = %v, want 1", fa.Unblocks)
	}
	if fa.AgeSeconds != 300 {
		t.Fatalf("a age = %v, want 300", fa.AgeSeconds)
	}
	fc := ExtractFeatures(r, tickets[2], tickets, 400)
	if fc.Unblocks != 0 {
		t.Fatalf("c unblocks = %v, want 0 (disjoint)", fc.Unblocks)
	}
}

// TestPriorityNudgeFeature covers §5.2: a linked tracker's priority nudge is the
// one non-derived feature. It is picked up from the bridge link, defaults to
// zero (so it is inert until weighted), and only shifts the ranking when the
// scorer is given a nonzero weight for it — a nudge, never an override.
func TestPriorityNudgeFeature(t *testing.T) {
	r := newRepo(t)
	a := ticket(t, r, "a", 100)
	b := ticket(t, r, "b", 100)
	tickets := []deps.Ticket{
		{ID: a, Scope: deps.Scope{Writes: []string{"src/x"}}},
		{ID: b, Scope: deps.Scope{Writes: []string{"src/y"}}},
	}

	// No link: nudge feature is zero.
	if f := ExtractFeatures(r, tickets[0], tickets, 400); f.PriorityNudge != 0 {
		t.Fatalf("unlinked nudge = %v, want 0", f.PriorityNudge)
	}

	// Link a with a high nudge; the feature now reflects it.
	if err := bridge.SetLink(r, a, bridge.Link{System: "ext", ForeignID: "T-1", PriorityNudge: 0.9}, "bridge", 100); err != nil {
		t.Fatalf("SetLink: %v", err)
	}
	if f := ExtractFeatures(r, tickets[0], tickets, 400); f.PriorityNudge != 0.9 {
		t.Fatalf("linked nudge = %v, want 0.9", f.PriorityNudge)
	}

	// With a zero nudge weight the nudge cannot change the order (a and b are
	// otherwise identical, so the tie breaks by hash, deterministically).
	inert := Weights{PriorityNudge: 0}
	r0 := RankTickets(r, inert, tickets, 400)
	if r0[0].ID.Hex() >= r0[1].ID.Hex() {
		t.Fatalf("with zero nudge weight, order should be by hash: %s then %s", r0[0].ID.Hex(), r0[1].ID.Hex())
	}

	// Give the nudge a positive weight and a now outranks b regardless of hash.
	w := Weights{PriorityNudge: 1}
	ranked := RankTickets(r, w, tickets, 400)
	if !ranked[0].ID.Equal(a) {
		t.Fatalf("weighted nudge did not lift the linked ticket: top = %s, want %s", ranked[0].ID.Hex(), a.Hex())
	}
}

// TestRankTicketsDeterministic: a scorer favoring contention ranks the
// conflicting tickets above the disjoint one, and the order is stable.
func TestRankTicketsDeterministic(t *testing.T) {
	r := newRepo(t)
	a := ticket(t, r, "a", 100)
	b := ticket(t, r, "b", 100)
	c := ticket(t, r, "c", 100)
	tickets := []deps.Ticket{
		{ID: a, Scope: deps.Scope{Writes: []string{"src/auth"}}},
		{ID: b, Scope: deps.Scope{Writes: []string{"src/auth/y.go"}}},
		{ID: c, Scope: deps.Scope{Writes: []string{"src/web"}}},
	}
	w := Weights{Unblocks: 1}

	r1 := RankTickets(r, w, tickets, 200)
	r2 := RankTickets(r, w, tickets, 200)
	if len(r1) != 3 {
		t.Fatalf("ranked %d, want 3", len(r1))
	}
	// c (0 contention) must rank last; a and b (1 each) rank above it.
	if r1[2].ID.Hex() != c.Hex() {
		t.Fatalf("disjoint ticket should rank last, got %s", r1[2].ID.Hex()[:12])
	}
	for i := range r1 {
		if r1[i].ID.Hex() != r2[i].ID.Hex() {
			t.Fatalf("ranking not deterministic at %d", i)
		}
	}
}

// TestLearnedScorerRanks ties it together: fit weights from decisions, then use
// them to rank real tickets — the Stage 3 loop (§3.3).
func TestLearnedScorerRanks(t *testing.T) {
	r := newRepo(t)
	high := ticket(t, r, "high-contention", 100)
	low := ticket(t, r, "low-contention", 100)
	tickets := []deps.Ticket{
		{ID: high, Scope: deps.Scope{Writes: []string{"src/auth"}}},
		{ID: low, Scope: deps.Scope{Writes: []string{"src/auth/y.go"}}},
		{ID: ticket(t, r, "lonely", 100), Scope: deps.Scope{Writes: []string{"src/web"}}},
	}
	// Past decisions preferred higher contention.
	corpus := []Comparison{
		{Winner: Features{Unblocks: 2}, Loser: Features{Unblocks: 0}},
		{Winner: Features{Unblocks: 3}, Loser: Features{Unblocks: 1}},
	}
	w := Fit(corpus, 20)
	ranked := RankTickets(r, w, tickets, 200)
	if ranked[len(ranked)-1].Features.Unblocks != 0 {
		t.Fatalf("least-contended ticket should rank last, got features %+v", ranked[len(ranked)-1].Features)
	}
}
