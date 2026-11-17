package txn

import (
	"context"
	"testing"

	"github.com/varvig/varvig/varvig/internal/merge"
	"github.com/varvig/varvig/varvig/internal/object"
)

// TestLandMergesDisjointLines: the proposal and a concurrent change both edit the
// same file, but different lines. The first (transactional) phase refuses — it
// compares whole files and sees a genuine overlap — and the merge phase resolves
// it textually with no regenerator, landing a merge change that carries both
// edits.
func TestLandMergesDisjointLines(t *testing.T) {
	r := newRepo(t)
	seed(t, r, "a", "L1\nL2\nL3\n")
	base := tip(t, r)

	p := proposalFrom(t, r, base, map[string]string{"a": "X1\nL2\nL3\n"}) // edits line 1
	seed(t, r, "a", "L1\nL2\nX3\n")                                       // concurrent: edits line 3

	res, err := LandProposal(context.Background(), r, mainRef, p, nil)
	if err != nil {
		t.Fatalf("land (textual merge): %v", err)
	}
	if got, _ := readFile(t, r, "a"); got != "X1\nL2\nX3\n" {
		t.Fatalf("a = %q; want both disjoint edits merged", got)
	}
	// The landed change is a merge: two parents (the tip it merged onto and the
	// proposal it merged in).
	obj, err := r.Objects.Get(res.Change)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := obj.AsChange()
	if len(c.Parents) != 2 {
		t.Fatalf("landed change has %d parents; a merge has two", len(c.Parents))
	}
}

// recordingReg is a fake Regenerator: it resolves every conflicted path to a
// fixed output and records the intent it was handed, so a test can prove the
// proposal's provenance reached regeneration (the PR-3 → PR-4 link).
type recordingReg struct {
	out       []byte
	called    int
	gotIntent object.Provenance
	gotPath   string
}

func (g *recordingReg) Regenerate(ctx context.Context, req merge.RegenRequest) ([]byte, error) {
	g.called++
	g.gotIntent = req.Intent
	g.gotPath = req.Path
	return g.out, nil
}

// TestLandRegeneratesSameLineConflict: the proposal and the tip changed the same
// line, which the textual fallback cannot resolve. A regenerator is supplied, so
// the merge phase re-derives the path — and it is handed the proposal's recorded
// intent (its provenance), which is the whole reason PR-3 threaded provenance
// onto the proposal in the first place.
func TestLandRegeneratesSameLineConflict(t *testing.T) {
	r := newRepo(t)
	seed(t, r, "a", "base\n")
	base := tip(t, r)
	prov := putProvenance(t, r, object.Provenance{Authority: "agent-a", Model: "m1"})

	p := proposalWithProvenance(t, r, base, prov, map[string]string{"a": "theirs\n"})
	seed(t, r, "a", "ours\n") // same line, diverged

	reg := &recordingReg{out: []byte("resolved\n")}
	res, err := LandProposal(context.Background(), r, mainRef, p, reg)
	if err != nil {
		t.Fatalf("land (regenerated merge): %v", err)
	}
	if got, _ := readFile(t, r, "a"); got != "resolved\n" {
		t.Fatalf("a = %q; want the regenerator's output", got)
	}
	if reg.called == 0 {
		t.Fatal("regenerator was never called for a same-line conflict")
	}
	if reg.gotPath != "a" {
		t.Fatalf("regenerator path = %q; want a", reg.gotPath)
	}
	if reg.gotIntent.Model != "m1" {
		t.Fatalf("regenerator intent model = %q; want the proposal's m1 — provenance did not reach regeneration", reg.gotIntent.Model)
	}
	if res.Change == nil {
		t.Fatal("no merge change was committed")
	}
}
