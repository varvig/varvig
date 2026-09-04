package txn

import "sort"

// This file is the write-set overlap surface (build spec P2.1). The scheduler
// already serializes conflicting work and parallelizes the rest; §1.4 makes that
// its responsibility. What was missing is *visibility*: two proposals colliding
// on a path is a discharge failure the scheduler should be able to see and
// report — before execution from the declared sets, and after execution from the
// observed ones — so a disagreement between what a unit of work declared it would
// write and what it actually wrote does not pass silently.
//
// Everything here is metadata: path sets, never content. No proposal's bytes
// cross a task boundary through these reports — the isolation the read set buys
// is not weakened to gain the visibility.

// Overlap is a write-set intersection between two named units of work: the two
// names and the overlapping paths (declared prefixes before execution, concrete
// paths after). It carries no content.
type Overlap struct {
	A     string
	B     string
	Paths []string
}

// DeclaredOverlaps reports every pair of transactions whose declared write sets
// intersect, with the overlapping path prefixes. It is a pure function of the
// declared sets — no workspace runs — so a caller can see collisions ahead of
// execution, which is exactly what the scheduler serializes on.
func DeclaredOverlaps(txns []*Txn) []Overlap {
	writes := make([][]string, len(txns))
	for i, t := range txns {
		writes[i] = normalize(t.Writes)
	}
	var out []Overlap
	for i := 0; i < len(txns); i++ {
		for j := i + 1; j < len(txns); j++ {
			if paths := prefixIntersection(writes[i], writes[j]); len(paths) > 0 {
				out = append(out, Overlap{A: txns[i].Name, B: txns[j].Name, Paths: paths})
			}
		}
	}
	return out
}

// ObservedOverlaps reports every pair of results whose observed write sets share
// a concrete path — the collisions that actually happened. Two proposals landing
// on the same file is the §1.4 failure P2.1 makes visible; disjoint results
// produce nothing.
func ObservedOverlaps(results []Result) []Overlap {
	var out []Overlap
	for i := 0; i < len(results); i++ {
		for j := i + 1; j < len(results); j++ {
			if paths := exactIntersection(results[i].Writes, results[j].Writes); len(paths) > 0 {
				out = append(out, Overlap{A: results[i].Name, B: results[j].Name, Paths: paths})
			}
		}
	}
	return out
}

// ScopeDrift is the disagreement between a unit of work's declared write set and
// what it actually wrote: paths it wrote that no declared prefix covers
// (under-declaration), and declared prefixes that matched nothing it wrote
// (over-declaration). Both are path names only.
type ScopeDrift struct {
	Name            string
	WroteUndeclared []string
	DeclaredUnused  []string
}

// Empty reports whether declared and observed agreed exactly.
func (d ScopeDrift) Empty() bool {
	return len(d.WroteUndeclared) == 0 && len(d.DeclaredUnused) == 0
}

// Drift compares a transaction's declared write set against an observed write set
// (e.g. Result.Writes) and reports any disagreement between them.
func Drift(name string, declaredWrites, observed []string) ScopeDrift {
	declared := normalize(declaredWrites)
	obs := normalize(observed)

	d := ScopeDrift{Name: name}
	for _, p := range obs {
		if !inScope(p, declared) {
			d.WroteUndeclared = append(d.WroteUndeclared, p)
		}
	}
	for _, pref := range declared {
		used := false
		for _, p := range obs {
			if pathCovers(pref, p) {
				used = true
				break
			}
		}
		if !used {
			d.DeclaredUnused = append(d.DeclaredUnused, pref)
		}
	}
	return d
}

// Reconcile reports the declared/observed drift for each transaction whose result
// disagreed with its declaration; agreeing transactions are omitted. txns and
// results are matched by position, so pass the results Run returned for txns.
func Reconcile(txns []*Txn, results []Result) []ScopeDrift {
	var out []ScopeDrift
	n := len(txns)
	if len(results) < n {
		n = len(results)
	}
	for i := 0; i < n; i++ {
		if results[i].Err != nil {
			continue // a failed transaction has no meaningful observed set
		}
		if d := Drift(txns[i].Name, txns[i].Writes, results[i].Writes); !d.Empty() {
			out = append(out, d)
		}
	}
	return out
}

// prefixIntersection returns the overlapping regions of two declared prefix sets:
// for each overlapping pair, the more specific prefix (the region actually
// shared), deduplicated and sorted.
func prefixIntersection(as, bs []string) []string {
	set := map[string]bool{}
	for _, a := range as {
		for _, b := range bs {
			if overlap(a, b) {
				set[moreSpecific(a, b)] = true
			}
		}
	}
	return sortedKeys(set)
}

// moreSpecific returns the narrower of two overlapping prefixes: if a covers b, b
// is the shared region; otherwise a is.
func moreSpecific(a, b string) string {
	if pathCovers(a, b) {
		return b
	}
	return a
}

func exactIntersection(as, bs []string) []string {
	inB := make(map[string]bool, len(bs))
	for _, b := range bs {
		inB[b] = true
	}
	set := map[string]bool{}
	for _, a := range as {
		if inB[a] {
			set[a] = true
		}
	}
	return sortedKeys(set)
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
