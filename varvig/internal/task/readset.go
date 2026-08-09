package task

import "sync"

// ReadSet accumulates, in order and without duplicates, every object hash a task
// resolves through the gate. It is the raw material for provenance: "log every
// resolved hash a task touches and feed it directly into the change record"
// (auth design §8.2), so audit and provenance become one mechanism rather than
// two. It is safe for concurrent use — a gate may serve overlapping tool calls.
type ReadSet struct {
	mu    sync.Mutex
	order []string
	seen  map[string]bool
}

// NewReadSet returns an empty read set.
func NewReadSet() *ReadSet { return &ReadSet{seen: map[string]bool{}} }

// Record notes that the task resolved hash. Repeats are collapsed; first-seen
// order is preserved so the provenance record reads as a plausible trace.
func (rs *ReadSet) Record(hash string) {
	if hash == "" {
		return
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.seen[hash] {
		return
	}
	rs.seen[hash] = true
	rs.order = append(rs.order, hash)
}

// Hashes returns a copy of the resolved hashes in first-seen order.
func (rs *ReadSet) Hashes() []string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return append([]string(nil), rs.order...)
}

// Len reports how many distinct hashes have been recorded.
func (rs *ReadSet) Len() int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return len(rs.order)
}
