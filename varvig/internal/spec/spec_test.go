package spec

import (
	"context"
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
