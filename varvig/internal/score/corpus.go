package score

import (
	"github.com/dividebyzero/claude-experiments/varvig/internal/attest"
	"github.com/dividebyzero/claude-experiments/varvig/internal/deps"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// BuildCorpus derives a training corpus of pairwise comparisons from recorded
// governance decisions (tickets §3.3, §3.4). The signal available structurally
// is the director's own approve/veto history: a ticket that was approved should
// rank above one that was vetoed. Each such pair becomes a labelled Comparison
// whose features come from the same ExtractFeatures the live scorer uses, so a
// scorer fitted here is judged on exactly the inputs it will see in production.
//
// Only scoped tickets carry features, so only they contribute. A ticket with
// neither an approval nor a veto is pending — no signal — and is skipped.
//
// This is the base signal. Two further signals named in §3.3 — promotion order
// ("do this one first") and explicit signed overrides (§3.4) — are additional
// comparison sources that fold into the same corpus shape as they are recorded.
func BuildCorpus(r *repo.Repo, now int64) ([]Comparison, error) {
	tickets, err := deps.ScopedTickets(r)
	if err != nil {
		return nil, err
	}

	var winners, losers []Features
	for _, t := range tickets {
		atts, err := attest.Attestations(r, t.ID)
		if err != nil {
			return nil, err
		}
		cls := classify(atts)
		if cls == 0 {
			continue // pending: no decision to learn from
		}
		f := ExtractFeatures(r, t, tickets, now)
		if cls > 0 {
			winners = append(winners, f)
		} else {
			losers = append(losers, f)
		}
	}

	// Every approved ticket outranks every vetoed one. The nested order is fixed
	// (winners and losers arrive in ScopedTickets' hash order), so the corpus is
	// deterministic.
	var corpus []Comparison
	for _, w := range winners {
		for _, l := range losers {
			corpus = append(corpus, Comparison{Winner: w, Loser: l})
		}
	}
	return corpus, nil
}

// classify reduces a revision's attestations to a preference sign: +1 approved,
// -1 vetoed, 0 undecided. A veto is decisive (it outranks any approval), mirroring
// attest.Derive, so a vetoed-then-approved revision still labels as a loser.
func classify(atts []object.Attestation) int {
	approved := false
	for _, a := range atts {
		switch a.Decision {
		case object.DecisionVeto:
			return -1
		case object.DecisionApprove:
			approved = true
		}
	}
	if approved {
		return 1
	}
	return 0
}

// FitFromHistory builds the corpus from recorded decisions and fits a scorer to
// it (tickets §3.3 Stage 3). It returns the learned weights, the corpus size,
// and a backtest of the fitted scorer against its own corpus — the training-set
// agreement a reviewer reads before promoting the scorer.
func FitFromHistory(r *repo.Repo, epochs int, now int64) (Weights, BacktestReport, error) {
	corpus, err := BuildCorpus(r, now)
	if err != nil {
		return Weights{}, BacktestReport{}, err
	}
	w := Fit(corpus, epochs)
	return w, Backtest(w, corpus), nil
}
