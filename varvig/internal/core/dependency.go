package core

import (
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// Dependency validation is optimistic concurrency control as §1.4 intended
// (design addendum, F3): the declared dependency set is only a scheduling hint,
// and what actually gates admission is the *observed* dependency set — the reads
// the change recorded in provenance. If something a change read has changed since
// the base it was built on, the change is stale and must be re-derived; if a path
// changed but the change never read it, there is no conflict and no retry.
//
// The check is hash-based, so it needs no path mapping: a read is recorded as the
// content id it resolved to. That id was reachable in the base the change built
// on; if it is no longer reachable in the current base, the content the change
// depended on has changed underneath it. A read whose id is still reachable is
// unchanged (identical content, even if moved); a read that was never part of the
// base tree (a change or log read, not a file dependency) is ignored.

// DependencyStale reports whether any of a change's observed reads changed
// between oldBase (the base it was built on) and newBase (the current base). It
// returns the stale read ids, so a refusal can name what moved. Reads not present
// in the old base tree are not file dependencies and are ignored.
func DependencyStale(r *repo.Repo, contextRead []string, oldBase, newBase multihash.Multihash) (bool, []string, error) {
	oldTree, err := TreeOf(r, oldBase)
	if err != nil {
		return false, nil, err
	}
	newTree, err := TreeOf(r, newBase)
	if err != nil {
		return false, nil, err
	}
	oldSet := map[string]bool{}
	if err := CollectReach(r, oldTree, oldSet); err != nil {
		return false, nil, err
	}
	newSet := map[string]bool{}
	if err := CollectReach(r, newTree, newSet); err != nil {
		return false, nil, err
	}
	var changed []string
	seen := map[string]bool{}
	for _, h := range contextRead {
		h = strings.TrimSpace(h)
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		// A read is a file dependency only if it was part of the base tree the
		// change built on; if that content is gone from the current base, the
		// dependency changed underneath the change.
		if oldSet[h] && !newSet[h] {
			changed = append(changed, h)
		}
	}
	return len(changed) > 0, changed, nil
}

// CheckReadStaleness validates a change's observed reads against the current base
// (design addendum, F3). It reads the change's parent (the base it was built on)
// and its provenance read set, and reports whether any read has since changed —
// the disagreement a shell surfaces as a refusal or the scheduler acts on as a
// retry. A change with no parent, or no recorded reads, is never stale.
func CheckReadStaleness(r *repo.Repo, changeID, currentBase multihash.Multihash) (bool, []string, error) {
	obj, err := r.Objects.Get(changeID)
	if err != nil {
		return false, nil, err
	}
	c, err := obj.AsChange()
	if err != nil {
		return false, nil, nil // not a change: no reads to validate
	}
	if len(c.Parents) == 0 || c.Provenance == nil {
		return false, nil, nil
	}
	oldBase := c.Parents[0]
	if oldBase.Equal(currentBase) {
		return false, nil, nil // base has not moved
	}
	provObj, err := r.Objects.Get(c.Provenance)
	if err != nil {
		return false, nil, err
	}
	pv, err := provObj.AsProvenance()
	if err != nil {
		return false, nil, err
	}
	reads := strings.Fields(pv.ContextRead)
	return DependencyStale(r, reads, oldBase, currentBase)
}
