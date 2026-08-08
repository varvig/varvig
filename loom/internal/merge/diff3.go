// Package merge implements Loom's merge driver (design §1.2). Its distinctive
// path is merge-by-regeneration: when two changes conflict, rather than forcing
// a textual diff3, the losing change's original intent (its provenance — task,
// context, model) is handed to a regenerator together with the new base and
// re-run, producing a change that is correct against the new base rather than
// merely textually plausible. Textual three-way merge remains the fast path and
// the fallback when no regenerator is available or it declines.
package merge

// Conflict markers match the widely understood diff3 style so a human or tool
// reading a fallback result sees something familiar.
const (
	markerOurs = "<<<<<<< ours\n"
	markerBase = "|||||||  base\n"
	markerMid  = "=======\n"
	markerThrs = ">>>>>>> theirs\n"
)

// lcsPairs returns the index pairs of a longest common subsequence of a and b,
// in increasing order — the alignment of unchanged elements.
func lcsPairs(a, b []string) [][2]int {
	n, m := len(a), len(b)
	// dp[i][j] = LCS length of a[i:], b[j:]
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var pairs [][2]int
	i, j := 0, 0
	for i < n && j < m {
		if a[i] == b[j] {
			pairs = append(pairs, [2]int{i, j})
			i++
			j++
		} else if dp[i+1][j] >= dp[i][j+1] {
			i++
		} else {
			j++
		}
	}
	return pairs
}

// merge3 performs a line-level three-way merge of ancestor o against ours a and
// theirs b. It returns the merged lines and whether any region conflicted;
// conflicting regions are emitted with diff3-style markers.
func merge3(o, a, b []string) (merged []string, conflict bool) {
	matchA := map[int]int{} // ancestor index -> ours index (unchanged lines)
	for _, p := range lcsPairs(o, a) {
		matchA[p[0]] = p[1]
	}
	matchB := map[int]int{}
	for _, p := range lcsPairs(o, b) {
		matchB[p[0]] = p[1]
	}
	// Stable anchors: ancestor lines unchanged by BOTH sides, in order.
	type anchor struct{ o, a, b int }
	var anchors []anchor
	for oi := 0; oi < len(o); oi++ {
		ai, okA := matchA[oi]
		bi, okB := matchB[oi]
		if okA && okB {
			anchors = append(anchors, anchor{oi, ai, bi})
		}
	}
	anchors = append(anchors, anchor{len(o), len(a), len(b)}) // end sentinel

	prevO, prevA, prevB := -1, -1, -1
	for _, an := range anchors {
		oReg := o[prevO+1 : an.o]
		aReg := a[prevA+1 : an.a]
		bReg := b[prevB+1 : an.b]
		switch {
		case equalLines(aReg, bReg):
			merged = append(merged, aReg...) // both sides identical (incl. unchanged)
		case equalLines(oReg, aReg):
			merged = append(merged, bReg...) // ours untouched here; take theirs
		case equalLines(oReg, bReg):
			merged = append(merged, aReg...) // theirs untouched here; take ours
		default:
			conflict = true
			merged = append(merged, markerOurs)
			merged = append(merged, aReg...)
			merged = append(merged, markerMid)
			merged = append(merged, bReg...)
			merged = append(merged, markerThrs)
		}
		if an.o < len(o) {
			merged = append(merged, a[an.a]) // the anchor line itself
		}
		prevO, prevA, prevB = an.o, an.a, an.b
	}
	return merged, conflict
}

func equalLines(x, y []string) bool {
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
