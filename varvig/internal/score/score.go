// Package score is the ticket scorer of tickets §3.3 — the throughput half of
// scheduling, kept strictly separate from the safety half (the promotion
// policy of §2/M1). A score only reorders admissible work; it never decides
// whether work may run.
//
// The design's three stages: Stage 1 is a manual priority queue (the M2
// PriorityOrder). Stage 2 is the hard constraints (the promotion policy). Stage
// 2.5 is an out-of-process model that scores each ticket with a rationale —
// model inference lives outside the binary by design, so it enters through the
// same Scorer boundary rather than being embedded here. This package implements
// Stage 3: learn the ordering from decisions already made. Every override,
// veto, and "do this one first" is a labelled pairwise comparison; features are
// already computable from the repo; fit weights to the comparisons.
//
// Because a scorer is a small, serializable weight vector and the full history
// is retained, backtesting is native (§3.3): replay past decisions, rank with a
// candidate scorer, and report every case where it disagreed. A scorer is
// promoted only if that review passes — a scorer is governed as code.
package score

import "encoding/json"

// Features is the computed feature vector for a ticket (tickets §3.3). Each
// field is derivable from repository state — no human labels the features, only
// the outcomes. Add fields over time; Fit and Score adapt to the vector.
type Features struct {
	// BlastRadius is the size of the declared write footprint: how much of the
	// tree the ticket may change.
	BlastRadius float64 `json:"blast_radius"`
	// Unblocks is how many other tickets this one contends with — landing it
	// frees that much conflicting work.
	Unblocks float64 `json:"unblocks"`
	// AgeSeconds is how long the ticket has waited; older work ages upward.
	AgeSeconds float64 `json:"age_seconds"`
}

func (f Features) vec() []float64 { return []float64{f.BlastRadius, f.Unblocks, f.AgeSeconds} }

// Weights is a linear scorer over Features: score is their dot product. The
// weights are the whole scorer, so a scorer serializes to a few numbers, is
// content-addressable, and is governed as code (§3.3).
type Weights struct {
	BlastRadius float64 `json:"blast_radius"`
	Unblocks    float64 `json:"unblocks"`
	AgeSeconds  float64 `json:"age_seconds"`
}

func (w Weights) vec() []float64 { return []float64{w.BlastRadius, w.Unblocks, w.AgeSeconds} }

func weightsFromVec(v []float64) Weights {
	return Weights{BlastRadius: v[0], Unblocks: v[1], AgeSeconds: v[2]}
}

// Score returns the linear score of a feature vector.
func (w Weights) Score(f Features) float64 {
	return dot(w.vec(), f.vec())
}

// Marshal serializes weights to canonical JSON — a scorer as a content-
// addressable artifact.
func (w Weights) Marshal() ([]byte, error) { return json.Marshal(w) }

// UnmarshalWeights parses a serialized scorer.
func UnmarshalWeights(b []byte) (Weights, error) {
	var w Weights
	err := json.Unmarshal(b, &w)
	return w, err
}

// Comparison is a labelled pairwise preference: Winner should rank strictly
// above Loser (tickets §3.3). Overrides, vetoes, and "do this one first" all
// reduce to this shape, which is what makes the corpus free to collect.
type Comparison struct {
	Winner Features `json:"winner"`
	Loser  Features `json:"loser"`
}

// Fit learns weights from comparisons with an averaged perceptron over the
// pairwise feature differences. It is fully deterministic: zero initialization,
// a fixed iteration order, and no randomness, so the same corpus and epoch
// count always yield the same weights — the property the backtest and
// deterministic replay (§7.4) depend on.
func Fit(comparisons []Comparison, epochs int) Weights {
	if epochs < 1 {
		epochs = 1
	}
	dim := len(Features{}.vec())
	w := make([]float64, dim)
	acc := make([]float64, dim)
	steps := 0
	for e := 0; e < epochs; e++ {
		for _, c := range comparisons {
			d := sub(c.Winner.vec(), c.Loser.vec())
			// Perceptron: nudge toward ranking the winner above the loser
			// whenever the current weights do not already do so.
			if dot(w, d) <= 0 {
				w = add(w, d)
			}
			acc = add(acc, w)
			steps++
		}
	}
	if steps == 0 {
		return Weights{}
	}
	return weightsFromVec(scale(acc, 1/float64(steps)))
}

// BacktestReport summarizes a candidate scorer against a corpus of past
// decisions (tickets §3.3, §7.4): how many comparisons it agrees with and the
// exact ones it got wrong, so a human reviews the disagreements before the
// scorer is promoted.
type BacktestReport struct {
	Total         int
	Agree         int
	Disagree      int
	Disagreements []Comparison
}

// Backtest replays comparisons against w and reports where the scorer disagrees
// with the recorded decision — i.e. where it does not rank the winner strictly
// above the loser. A tie counts as a disagreement: an ordering that cannot tell
// the two apart did not reproduce the decision.
func Backtest(w Weights, comparisons []Comparison) BacktestReport {
	rep := BacktestReport{Total: len(comparisons)}
	for _, c := range comparisons {
		if w.Score(c.Winner) > w.Score(c.Loser) {
			rep.Agree++
		} else {
			rep.Disagree++
			rep.Disagreements = append(rep.Disagreements, c)
		}
	}
	return rep
}

func dot(a, b []float64) float64 {
	var s float64
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

func add(a, b []float64) []float64 {
	out := make([]float64, len(a))
	for i := range a {
		out[i] = a[i] + b[i]
	}
	return out
}

func sub(a, b []float64) []float64 {
	out := make([]float64, len(a))
	for i := range a {
		out[i] = a[i] - b[i]
	}
	return out
}

func scale(a []float64, k float64) []float64 {
	out := make([]float64, len(a))
	for i := range a {
		out[i] = a[i] * k
	}
	return out
}
