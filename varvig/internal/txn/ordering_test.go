package txn

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// appendTxn read-modify-writes shared.txt, appending token. Such transactions
// all conflict (same read+write set), so the scheduler serializes them and the
// resulting string records their admission order exactly.
func appendTxn(name, token string, prio int) *Txn {
	return &Txn{
		Name:     name,
		Priority: prio,
		Reads:    []string{"shared.txt"},
		Writes:   []string{"shared.txt"},
		Apply: func(ws *Workspace) error {
			cur, err := ws.Read("shared.txt")
			if err != nil && !errors.Is(err, ErrNotExist) {
				return err
			}
			return ws.Write("shared.txt", append(cur, token...))
		},
	}
}

func runAll(t *testing.T, s *Scheduler, txns []*Txn) {
	t.Helper()
	for _, res := range s.Run(context.Background(), txns) {
		if res.Err != nil {
			t.Fatalf("txn %s failed: %v", res.Name, res.Err)
		}
	}
}

// TestOrderingDeterminesAdmissionOrder: conflicting transactions are admitted in
// the order the Ordering ranks them, so the read-modify-write result records
// that order exactly (tickets M2).
func TestOrderingDeterminesAdmissionOrder(t *testing.T) {
	r := newRepo(t)
	s := NewScheduler(r, mainRef)
	s.SetOrdering(PriorityOrder{})

	txns := []*Txn{
		appendTxn("low", "[low]", 1),
		appendTxn("high", "[high]", 3),
		appendTxn("mid", "[mid]", 2),
	}
	runAll(t, s, txns)

	final, ok := readFile(t, r, "shared.txt")
	if !ok {
		t.Fatal("shared.txt missing")
	}
	if final != "[high][mid][low]" {
		t.Fatalf("admission order = %q, want [high][mid][low]", final)
	}
}

// TestOrderingSwapChangesOnlyOrder is the §7.4 guarantee: swapping the Ordering
// changes the admission sequence and nothing else — the same work is done, the
// same history length results, only the order differs.
func TestOrderingSwapChangesOnlyOrder(t *testing.T) {
	build := func() []*Txn {
		return []*Txn{
			appendTxn("a", "[a]", 1),
			appendTxn("b", "[b]", 3),
			appendTxn("c", "[c]", 2),
		}
	}

	r1 := newRepo(t)
	s1 := NewScheduler(r1, mainRef)
	s1.SetOrdering(InputOrder{})
	runAll(t, s1, build())
	in, _ := readFile(t, r1, "shared.txt")

	r2 := newRepo(t)
	s2 := NewScheduler(r2, mainRef)
	s2.SetOrdering(PriorityOrder{})
	runAll(t, s2, build())
	pr, _ := readFile(t, r2, "shared.txt")

	if in != "[a][b][c]" {
		t.Fatalf("input order = %q, want [a][b][c]", in)
	}
	if pr != "[b][c][a]" {
		t.Fatalf("priority order = %q, want [b][c][a]", pr)
	}
	if in == pr {
		t.Fatal("swapping the ordering did not change the admission order")
	}
	// Nothing else changed: same three contributions, same history length.
	for _, tok := range []string{"[a]", "[b]", "[c]"} {
		if !strings.Contains(in, tok) || !strings.Contains(pr, tok) {
			t.Fatalf("a contribution was lost: in=%q pr=%q", in, pr)
		}
	}
	if h1, h2 := historyLen(t, r1), historyLen(t, r2); h1 != 3 || h2 != 3 {
		t.Fatalf("history lengths = %d, %d, want 3, 3", h1, h2)
	}
}

// TestPlanIsDeterministic: the same transactions and ordering always produce the
// same admission plan — the deterministic-replay artifact of §7.4.
func TestPlanIsDeterministic(t *testing.T) {
	r := newRepo(t)
	txns := []*Txn{
		appendTxn("a", "[a]", 1),
		appendTxn("b", "[b]", 3),
		appendTxn("c", "[c]", 2),
	}

	s := NewScheduler(r, mainRef)
	s.SetOrdering(PriorityOrder{})
	p1 := s.Plan(txns)
	p2 := s.Plan(txns)
	if !reflect.DeepEqual(p1, p2) {
		t.Fatalf("plan not stable across calls: %v vs %v", p1, p2)
	}
	// A fresh scheduler with the same ordering replays the identical plan.
	s2 := NewScheduler(r, mainRef)
	s2.SetOrdering(PriorityOrder{})
	if !reflect.DeepEqual(p1, s2.Plan(txns)) {
		t.Fatalf("plan not reproducible on a fresh scheduler")
	}
	// PriorityOrder ranks by Priority desc: b(3), c(2), a(1) → indices 1,2,0.
	if !reflect.DeepEqual(p1, []int{1, 2, 0}) {
		t.Fatalf("plan = %v, want [1 2 0]", p1)
	}
}

// TestScoreOrderPluggable: a ScoreOrder is a module boundary the scorer plugs
// into; swapping the scorer changes the plan and nothing else about the shape.
func TestScoreOrderPluggable(t *testing.T) {
	txns := []*Txn{
		{Name: "a", Priority: 1},
		{Name: "b", Priority: 2},
		{Name: "c", Priority: 3},
	}
	// Score by priority ascending (opposite of PriorityOrder).
	asc := ScoreOrder{Score: func(t *Txn) float64 { return -float64(t.Priority) }}
	if got := asc.Order(txns); !reflect.DeepEqual(got, []int{0, 1, 2}) {
		t.Fatalf("ascending score plan = %v, want [0 1 2]", got)
	}
	desc := ScoreOrder{Score: func(t *Txn) float64 { return float64(t.Priority) }}
	if got := desc.Order(txns); !reflect.DeepEqual(got, []int{2, 1, 0}) {
		t.Fatalf("descending score plan = %v, want [2 1 0]", got)
	}
}

// TestInputOrderIsIdentity keeps the neutral default honest.
func TestInputOrderIsIdentity(t *testing.T) {
	txns := []*Txn{{Name: "x"}, {Name: "y"}, {Name: "z"}}
	if got := (InputOrder{}).Order(txns); !reflect.DeepEqual(got, []int{0, 1, 2}) {
		t.Fatalf("InputOrder = %v, want [0 1 2]", got)
	}
}

// TestPriorityOrderTiebreak: equal priorities break by Name, then input index.
func TestPriorityOrderTiebreak(t *testing.T) {
	txns := []*Txn{
		{Name: "zeta", Priority: 5},
		{Name: "alpha", Priority: 5},
		{Name: "mid", Priority: 9},
	}
	// mid(9) first; then alpha before zeta by name.
	if got := (PriorityOrder{}).Order(txns); !reflect.DeepEqual(got, []int{2, 1, 0}) {
		t.Fatalf("tiebreak plan = %v, want [2 1 0]", got)
	}
}
