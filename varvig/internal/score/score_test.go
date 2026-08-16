package score

import (
	"reflect"
	"testing"
)

// TestFitLearnsSeparableOrdering: a corpus where the winner always has more
// Unblocks must be learnable, and the fitted scorer must then agree with every
// comparison it was trained on (zero disagreements on a separable corpus).
func TestFitLearnsSeparableOrdering(t *testing.T) {
	corpus := []Comparison{
		{Winner: Features{Unblocks: 3}, Loser: Features{Unblocks: 1}},
		{Winner: Features{Unblocks: 5}, Loser: Features{Unblocks: 2}},
		{Winner: Features{Unblocks: 9}, Loser: Features{Unblocks: 0}},
		{Winner: Features{Unblocks: 4}, Loser: Features{Unblocks: 3}},
	}
	w := Fit(corpus, 20)
	if w.Unblocks <= 0 {
		t.Fatalf("expected a positive weight on Unblocks, got %+v", w)
	}
	rep := Backtest(w, corpus)
	if rep.Disagree != 0 {
		t.Fatalf("fitted scorer disagrees on its own separable corpus: %d/%d, %+v",
			rep.Disagree, rep.Total, rep.Disagreements)
	}
}

// TestFitIsDeterministic: same corpus and epochs always yield identical weights.
func TestFitIsDeterministic(t *testing.T) {
	corpus := []Comparison{
		{Winner: Features{BlastRadius: 1, Unblocks: 3, AgeSeconds: 100}, Loser: Features{BlastRadius: 4, Unblocks: 1, AgeSeconds: 10}},
		{Winner: Features{BlastRadius: 2, Unblocks: 5, AgeSeconds: 50}, Loser: Features{BlastRadius: 3, Unblocks: 2, AgeSeconds: 20}},
	}
	a := Fit(corpus, 15)
	b := Fit(corpus, 15)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("Fit not deterministic: %+v vs %+v", a, b)
	}
}

// TestBacktestReportsDisagreements: a scorer with the wrong sign disagrees with
// every comparison, and the report lists them.
func TestBacktestReportsDisagreements(t *testing.T) {
	corpus := []Comparison{
		{Winner: Features{Unblocks: 5}, Loser: Features{Unblocks: 1}},
		{Winner: Features{Unblocks: 8}, Loser: Features{Unblocks: 2}},
	}
	// A scorer that prefers *fewer* unblocks: wrong on both.
	wrong := Weights{Unblocks: -1}
	rep := Backtest(wrong, corpus)
	if rep.Disagree != 2 || rep.Agree != 0 {
		t.Fatalf("report = %+v, want 2 disagreements", rep)
	}
	if len(rep.Disagreements) != 2 {
		t.Fatalf("disagreements not listed: %+v", rep.Disagreements)
	}
	// The correct scorer agrees on both.
	if r := Backtest(Weights{Unblocks: 1}, corpus); r.Disagree != 0 {
		t.Fatalf("correct scorer disagreed: %+v", r)
	}
}

// TestBacktestTieIsDisagreement: a scorer that cannot separate winner from loser
// did not reproduce the decision.
func TestBacktestTieIsDisagreement(t *testing.T) {
	corpus := []Comparison{{Winner: Features{Unblocks: 2}, Loser: Features{Unblocks: 2}}}
	if rep := Backtest(Weights{Unblocks: 1}, corpus); rep.Disagree != 1 {
		t.Fatalf("a tie must count as a disagreement: %+v", rep)
	}
}

func TestWeightsMarshalRoundTrip(t *testing.T) {
	w := Weights{BlastRadius: -0.5, Unblocks: 2, AgeSeconds: 1e-6}
	b, err := w.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := UnmarshalWeights(b)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != w {
		t.Fatalf("round-trip: got %+v want %+v", got, w)
	}
}

func TestScoreIsDotProduct(t *testing.T) {
	w := Weights{BlastRadius: 2, Unblocks: 3, AgeSeconds: 0.5}
	f := Features{BlastRadius: 1, Unblocks: 2, AgeSeconds: 10}
	want := 2*1 + 3*2 + 0.5*10
	if got := w.Score(f); got != want {
		t.Fatalf("Score = %v, want %v", got, want)
	}
}
