package score

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/attest"
	"github.com/dividebyzero/claude-experiments/varvig/internal/deps"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

func corpusSigner(t *testing.T) attestSigner {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return attestSigner{priv: priv}
}

type attestSigner struct{ priv ed25519.PrivateKey }

func (s attestSigner) Public() ed25519.PublicKey     { return s.priv.Public().(ed25519.PublicKey) }
func (s attestSigner) Sign(b []byte) ([]byte, error) { return ed25519.Sign(s.priv, b), nil }

func scopedTicket(t *testing.T, r *repo.Repo, msg string, writes []string) object.Attestation {
	t.Helper()
	id, err := r.Objects.Put(object.NewChange(object.Change{Message: msg, Timestamp: 1, Author: "d"}))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := deps.SetScope(r, id, deps.Scope{Writes: writes}, "d", 1); err != nil {
		t.Fatalf("SetScope: %v", err)
	}
	return object.Attestation{Target: id}
}

func decide(t *testing.T, r *repo.Repo, s attestSigner, target object.Attestation, d object.Decision) {
	t.Helper()
	obj, err := attest.Sign(s, object.Attestation{Target: target.Target, Decision: d, Strength: object.StrengthStrong})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := attest.Attach(r, obj, "d", 1); err != nil {
		t.Fatalf("Attach: %v", err)
	}
}

// TestBuildCorpusFromDecisions: approved tickets pair against vetoed ones, and a
// pending ticket contributes nothing.
func TestBuildCorpusFromDecisions(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	s := corpusSigner(t)

	// Two approved (high contention), one vetoed (disjoint), one pending.
	a1 := scopedTicket(t, r, "approve-1", []string{"src/auth"})
	a2 := scopedTicket(t, r, "approve-2", []string{"src/auth/x.go"})
	v1 := scopedTicket(t, r, "veto-1", []string{"src/web"})
	_ = scopedTicket(t, r, "pending", []string{"src/api"}) // no decision

	decide(t, r, s, a1, object.DecisionApprove)
	decide(t, r, s, a2, object.DecisionApprove)
	decide(t, r, s, v1, object.DecisionVeto)

	corpus, err := BuildCorpus(r, 100)
	if err != nil {
		t.Fatalf("BuildCorpus: %v", err)
	}
	// 2 winners x 1 loser = 2 comparisons.
	if len(corpus) != 2 {
		t.Fatalf("corpus = %d comparisons, want 2", len(corpus))
	}
	// Every comparison's winner beats its loser on contention here.
	for _, c := range corpus {
		if c.Winner.Unblocks <= c.Loser.Unblocks {
			t.Fatalf("winner not more contended than loser: %+v", c)
		}
	}
}

// TestFitFromHistory: a scorer fitted from real decisions agrees with the
// decisions it was trained on, and BuildCorpus is deterministic.
func TestFitFromHistory(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	s := corpusSigner(t)
	a := scopedTicket(t, r, "approve", []string{"src/auth"})
	b := scopedTicket(t, r, "approve2", []string{"src/auth/y.go"})
	v := scopedTicket(t, r, "veto", []string{"src/web"})
	decide(t, r, s, a, object.DecisionApprove)
	decide(t, r, s, b, object.DecisionApprove)
	decide(t, r, s, v, object.DecisionVeto)

	w, rep, err := FitFromHistory(r, 30, 100)
	if err != nil {
		t.Fatalf("FitFromHistory: %v", err)
	}
	if rep.Total == 0 {
		t.Fatal("no comparisons built from history")
	}
	if rep.Disagree != 0 {
		t.Fatalf("fitted scorer disagrees with its training corpus: %d/%d", rep.Disagree, rep.Total)
	}
	// The learned scorer prefers contention (the approved tickets had it).
	if w.Unblocks <= 0 {
		t.Fatalf("expected a positive learned weight on contention, got %+v", w)
	}

	// Determinism: same repo, same corpus.
	c1, _ := BuildCorpus(r, 100)
	c2, _ := BuildCorpus(r, 100)
	if len(c1) != len(c2) {
		t.Fatalf("BuildCorpus not deterministic: %d vs %d", len(c1), len(c2))
	}
}

// TestEmptyCorpus: no decisions means no corpus, and fitting yields the zero
// scorer rather than erroring.
func TestEmptyCorpus(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	_ = scopedTicket(t, r, "pending", []string{"a"}) // scoped but undecided
	corpus, err := BuildCorpus(r, 100)
	if err != nil {
		t.Fatalf("BuildCorpus: %v", err)
	}
	if len(corpus) != 0 {
		t.Fatalf("corpus = %d, want 0 (no decisions)", len(corpus))
	}
	if _, rep, err := FitFromHistory(r, 10, 100); err != nil || rep.Total != 0 {
		t.Fatalf("FitFromHistory on empty = rep %+v err %v", rep, err)
	}
}
