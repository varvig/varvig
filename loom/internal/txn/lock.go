// Package txn is Loom's transaction scheduler (design §1.4). With hundreds of
// parallel agents, conflicts are the common case, so branching-and-merging is
// replaced by a database-like model: each transaction declares a read set and a
// write set ahead of time, the scheduler serializes genuinely conflicting work
// and runs the rest concurrently, and a transaction that loses a ref
// compare-and-swap retries automatically against the new base rather than
// surfacing to a human.
//
// The declared read set doubles as the transaction's capability boundary
// (design §1.4, §2 partial clone): a transaction may read only within its
// read+write sets and write only within its write set.
package txn

import (
	"context"
	"sort"
	"strings"
	"sync"
)

// claim is the set of path prefixes a running transaction reads and writes.
type claim struct {
	reads  []string
	writes []string
}

// pathCovers reports whether prefix a covers path b: the whole repo (""),
// an exact match, or an ancestor directory of b.
func pathCovers(a, b string) bool {
	if a == "" {
		return true
	}
	return a == b || strings.HasPrefix(b, a+"/")
}

// overlap reports whether two declared path prefixes intersect.
func overlap(a, b string) bool { return pathCovers(a, b) || pathCovers(b, a) }

func setsOverlap(as, bs []string) bool {
	for _, a := range as {
		for _, b := range bs {
			if overlap(a, b) {
				return true
			}
		}
	}
	return false
}

// conflicts reports whether two claims cannot run concurrently: write/write,
// write/read, or read/write overlap. Read/read never conflicts.
func conflicts(x, y claim) bool {
	return setsOverlap(x.writes, y.writes) ||
		setsOverlap(x.writes, y.reads) ||
		setsOverlap(x.reads, y.writes)
}

// lockManager admits claims that do not conflict with any currently-held claim,
// blocking a conflicting claim until the conflict clears. It is the concurrency
// gate that serializes conflicting transactions and parallelizes the rest.
type lockManager struct {
	mu   sync.Mutex
	cond *sync.Cond
	held []claim
}

func newLockManager() *lockManager {
	lm := &lockManager{}
	lm.cond = sync.NewCond(&lm.mu)
	return lm
}

// acquire blocks until c conflicts with nothing held, then records it. It
// returns ctx.Err() if the context is already done.
func (lm *lockManager) acquire(ctx context.Context, c claim) error {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !lm.anyConflict(c) {
			lm.held = append(lm.held, c)
			return nil
		}
		lm.cond.Wait()
	}
}

func (lm *lockManager) anyConflict(c claim) bool {
	for _, h := range lm.held {
		if conflicts(h, c) {
			return true
		}
	}
	return false
}

// release drops a held claim and wakes waiters.
func (lm *lockManager) release(c claim) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	for i := range lm.held {
		if sameClaim(lm.held[i], c) {
			lm.held = append(lm.held[:i], lm.held[i+1:]...)
			break
		}
	}
	lm.cond.Broadcast()
}

func sameClaim(a, b claim) bool {
	return strings.Join(a.reads, "\x00") == strings.Join(b.reads, "\x00") &&
		strings.Join(a.writes, "\x00") == strings.Join(b.writes, "\x00")
}

// normalize trims and de-duplicates a path-prefix set for stable comparison.
func normalize(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		p = strings.Trim(strings.TrimSpace(p), "/")
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
