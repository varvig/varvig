package txn

import "sort"

// Ordering decides the admission order of a batch of transactions (tickets M2,
// §3.3). It is a module boundary, not hardcoded policy: the scheduler asks the
// ordering for a permutation of the input and admits conflicting transactions
// in that order, so swapping the ordering changes the sequence and nothing
// else. The order is a pure function of the input, which is what makes
// deterministic replay possible (§7.4): the same transactions and the same
// ordering always yield the same admission order.
//
// An ordering ranks; it does not decide *whether* a transaction may run — that
// is the promotion policy's job (tickets §2, §3.3: constraints carry the
// safety, score is only throughput). Keeping the two separate keeps this
// obvious.
type Ordering interface {
	// Order returns the indices of txns in admission order: a permutation of
	// 0..len(txns)-1. It must be deterministic and total.
	Order(txns []*Txn) []int
}

// InputOrder admits transactions in the order given — the neutral default,
// equivalent to Stage 1 "then age" once callers pass work oldest-first.
type InputOrder struct{}

// Order returns the identity permutation.
func (InputOrder) Order(txns []*Txn) []int {
	order := make([]int, len(txns))
	for i := range order {
		order[i] = i
	}
	return order
}

// PriorityOrder admits higher Priority first (Stage 1 P0–P3, §3.3), breaking
// ties by Name and then by input position so the order is total and stable.
type PriorityOrder struct{}

// Order ranks by Priority descending, then Name, then input index.
func (PriorityOrder) Order(txns []*Txn) []int {
	return sortedIndices(txns, func(a, b *Txn) bool {
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		return a.Name < b.Name
	})
}

// ScoreOrder admits the highest-scored transaction first. It is the module
// boundary the Stage 2.5 model-judged and Stage 3 learned scorers plug into
// (§3.3): the scorer is a pure function of a transaction, and swapping it
// changes only the admission order. Ties break by Name then input index.
type ScoreOrder struct {
	Score func(*Txn) float64
}

// Order ranks by Score descending, then Name, then input index.
func (s ScoreOrder) Order(txns []*Txn) []int {
	return sortedIndices(txns, func(a, b *Txn) bool {
		sa, sb := s.Score(a), s.Score(b)
		if sa != sb {
			return sa > sb
		}
		return a.Name < b.Name
	})
}

// sortedIndices returns the indices of txns sorted by less, with input position
// as the final, stable tiebreak so the permutation is always total.
func sortedIndices(txns []*Txn, less func(a, b *Txn) bool) []int {
	order := make([]int, len(txns))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(x, y int) bool {
		a, b := txns[order[x]], txns[order[y]]
		if less(a, b) {
			return true
		}
		if less(b, a) {
			return false
		}
		return order[x] < order[y]
	})
	return order
}
