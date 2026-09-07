package spec

import (
	"context"
	"fmt"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/edge"
	"github.com/dividebyzero/claude-experiments/varvig/internal/graphnode"
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

// candidate creates a distinct change (via a unique blob/tree) and returns it.
func candidate(t *testing.T, r *repo.Repo, content string) multihash.Multihash {
	t.Helper()
	blob, err := r.Objects.Put(object.NewBlob([]byte(content)))
	if err != nil {
		t.Fatalf("put blob: %v", err)
	}
	tree, err := r.Objects.Put(object.NewTree([]object.Entry{
		{Name: "out.txt", Mode: 0o100644, Kind: object.TypeBlob, ID: blob},
	}))
	if err != nil {
		t.Fatalf("put tree: %v", err)
	}
	id, err := r.Objects.Put(object.NewChange(object.Change{Tree: tree, Message: content}))
	if err != nil {
		t.Fatalf("put change: %v", err)
	}
	return id
}

func TestAddListScoreBest(t *testing.T) {
	r := newRepo(t)
	p := Open(r.GitDir())
	a := candidate(t, r, "attempt-a")
	b := candidate(t, r, "attempt-b")
	c := candidate(t, r, "attempt-c")
	for _, id := range []multihash.Multihash{a, b, c} {
		if err := p.Add("task1", id, 0); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	entries, err := p.List("task1")
	if err != nil || len(entries) != 3 {
		t.Fatalf("List = %d entries, err=%v", len(entries), err)
	}
	_ = p.SetScore("task1", a, 0.5)
	_ = p.SetScore("task1", b, 0.9)
	_ = p.SetScore("task1", c, 0.1)

	best, ok, err := p.Best("task1")
	if err != nil || !ok {
		t.Fatalf("Best: ok=%v err=%v", ok, err)
	}
	if !best.Change.Equal(b) {
		t.Fatalf("best = %s, want b", best.Change.Hex()[4:16])
	}
}

func TestAddIsIdempotent(t *testing.T) {
	r := newRepo(t)
	p := Open(r.GitDir())
	a := candidate(t, r, "x")
	_ = p.Add("t", a, 0)
	_ = p.SetScore("t", a, 0.7)
	_ = p.Add("t", a, 0) // must not clobber the score
	entries, _ := p.List("t")
	if len(entries) != 1 || !entries[0].Scored || entries[0].Score != 0.7 {
		t.Fatalf("re-add clobbered entry: %+v", entries)
	}
}

func TestPruneKeepsTopK(t *testing.T) {
	r := newRepo(t)
	p := Open(r.GitDir())
	ids := map[string]multihash.Multihash{}
	scores := map[string]float64{"a": 0.9, "b": 0.5, "c": 0.7, "d": 0.1}
	for name, sc := range scores {
		id := candidate(t, r, name)
		ids[name] = id
		_ = p.Add("t", id, 0)
		_ = p.SetScore("t", id, sc)
	}
	removed, err := p.Prune("t", 2)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed = %d, want 2", len(removed))
	}
	// Top 2 by score are a (0.9) and c (0.7); b and d must be gone.
	remaining, _ := p.List("t")
	keep := map[string]bool{}
	for _, e := range remaining {
		keep[e.Change.Hex()] = true
	}
	if !keep[ids["a"].Hex()] || !keep[ids["c"].Hex()] {
		t.Fatal("prune removed a top-K candidate")
	}
	if keep[ids["b"].Hex()] || keep[ids["d"].Hex()] {
		t.Fatal("prune kept a below-threshold candidate")
	}
}

func TestScoreAllAndPromote(t *testing.T) {
	r := newRepo(t)
	p := Open(r.GitDir())
	short := candidate(t, r, "hi")
	long := candidate(t, r, "a much longer attempt")
	_ = p.Add("t", short, 0)
	_ = p.Add("t", long, 0)

	// Objective: prefer the change whose message is longer.
	scorer := ScorerFunc(func(_ context.Context, r *repo.Repo, id multihash.Multihash) (float64, error) {
		obj, err := r.Objects.Get(id)
		if err != nil {
			return 0, err
		}
		c, _ := obj.AsChange()
		return float64(len(c.Message)), nil
	})
	if err := p.ScoreAll(context.Background(), "t", r, scorer); err != nil {
		t.Fatalf("ScoreAll: %v", err)
	}
	promoted, err := Promote(p, r, "t", "refs/heads/main", "tester")
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if !promoted.Equal(long) {
		t.Fatalf("promoted the wrong candidate")
	}
	tip, err := r.Refs.Resolve("refs/heads/main")
	if err != nil || !tip.Equal(long) {
		t.Fatalf("ref not advanced to winner: %v", err)
	}
}

func TestAllChanges(t *testing.T) {
	r := newRepo(t)
	p := Open(r.GitDir())
	_ = p.Add("t1", candidate(t, r, "1"), 0)
	_ = p.Add("t2", candidate(t, r, "2"), 0)
	all, err := p.AllChanges()
	if err != nil || len(all) != 2 {
		t.Fatalf("AllChanges = %d, err=%v", len(all), err)
	}
}

// refusePolicy refuses exactly the changes in its set — a stand-in for a real
// promotion policy (a veto gate, a wasm module) so the store can be tested
// without depending on governance.
type refusePolicy struct{ deny map[string]bool }

func (p refusePolicy) Admit(_ *repo.Repo, change multihash.Multihash) error {
	if p.deny[change.Hex()] {
		return errRefused
	}
	return nil
}

var errRefused = fmt.Errorf("refused by test policy")

// TestPromoteWithPolicyRefusalNotOutranked is the §7.4 guarantee: a policy
// refusal cannot be outranked by a high score. The top-scored candidate is
// refused, so the lower-scored admissible one is promoted instead.
func TestPromoteWithPolicyRefusalNotOutranked(t *testing.T) {
	r := newRepo(t)
	p := Open(r.GitDir())
	top := candidate(t, r, "top-scored-but-refused")
	ok := candidate(t, r, "lower-scored-but-clean")
	_ = p.Add("t", top, 0)
	_ = p.Add("t", ok, 0)
	_ = p.SetScore("t", top, 0.99)
	_ = p.SetScore("t", ok, 0.10)

	policy := refusePolicy{deny: map[string]bool{top.Hex(): true}}
	promoted, err := PromoteWithPolicy(p, r, "t", "refs/heads/main", "tester", policy)
	if err != nil {
		t.Fatalf("PromoteWithPolicy: %v", err)
	}
	if !promoted.Equal(ok) {
		t.Fatal("a refused high-scored candidate was promoted over a clean one")
	}
}

// TestPromoteWithPolicyAllRefused: when every scored candidate is refused,
// promotion fails rather than falling back to a disqualified candidate.
func TestPromoteWithPolicyAllRefused(t *testing.T) {
	r := newRepo(t)
	p := Open(r.GitDir())
	a := candidate(t, r, "a")
	b := candidate(t, r, "b")
	_ = p.Add("t", a, 0)
	_ = p.Add("t", b, 0)
	_ = p.SetScore("t", a, 0.5)
	_ = p.SetScore("t", b, 0.6)

	policy := refusePolicy{deny: map[string]bool{a.Hex(): true, b.Hex(): true}}
	if _, err := PromoteWithPolicy(p, r, "t", "refs/heads/main", "tester", policy); err == nil {
		t.Fatal("PromoteWithPolicy promoted a candidate when all were refused")
	}
	// The ref must not have moved.
	if _, err := r.Refs.Resolve("refs/heads/main"); err == nil {
		t.Fatal("ref advanced despite all candidates being refused")
	}
}

// TestPruneWithEdgesForgetsCollectableEdges is the "by construction" half of
// GRAPH.md §11.5: discarding a speculation candidate deletes the edges about it
// in the same operation, so there is no window in which they outlive their
// subject and no sweep that has to remember them.
func TestPruneWithEdgesForgetsCollectableEdges(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pool := Open(r.GitDir())

	// Two candidates, each carrying an edge about the attempt.
	var states []multihash.Multihash
	for i, body := range []string{"attempt one", "attempt two"} {
		id, err := r.Objects.Put(object.NewBlob([]byte(body)))
		if err != nil {
			t.Fatal(err)
		}
		if err := pool.Add("t1", id, int64(100+i)); err != nil {
			t.Fatal(err)
		}
		if err := pool.SetScore("t1", id, float64(10-i)); err != nil {
			t.Fatal(err)
		}
		states = append(states, id)
		eph, err := graphnode.Ephemeral(id)
		if err != nil {
			t.Fatal(err)
		}
		far, err := graphnode.External("agent", "belief/1")
		if err != nil {
			t.Fatal(err)
		}
		e, err := edge.New(edge.Spec{
			Source: eph, Target: far, Type: "agent:couples-with", ObservedUnder: id,
			Provenance: edge.Provenance{
				Class: edge.Asserted, Principal: "planner", Strength: object.StrengthDelegated,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := edge.Put(r, e, "planner", 100); err != nil {
			t.Fatal(err)
		}
	}

	total, err := edge.CountAllEphemeral(r)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Fatalf("stored %d collectable edges, want 2", total)
	}

	// Keep the best one; the other is discarded along with its edge.
	removed, err := pool.PruneWithEdges(r, "t1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 {
		t.Fatalf("pruned %d candidates, want 1", len(removed))
	}
	if n, err := edge.CountEphemeral(r, removed[0]); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Errorf("the discarded candidate still carries %d edges", n)
	}

	// The surviving candidate keeps its edge: retention follows the endpoint,
	// not the sweep's mood.
	survivor := states[0]
	if removed[0].Equal(survivor) {
		survivor = states[1]
	}
	if n, err := edge.CountEphemeral(r, survivor); err != nil {
		t.Fatal(err)
	} else if n != 1 {
		t.Errorf("the surviving candidate lost its edge: %d remain", n)
	}
	if total, err := edge.CountAllEphemeral(r); err != nil {
		t.Fatal(err)
	} else if total != 1 {
		t.Errorf("total collectable edges = %d, want 1", total)
	}
}
