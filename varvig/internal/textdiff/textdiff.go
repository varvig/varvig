// Package textdiff renders a unified textual diff of two byte slices. It is the
// local, no-network read that gives an agent an independent view of its own
// change (build spec P0.2) — textual only, per §5's graceful-degradation rule; a
// binary path is reported as changed, never dumped.
package textdiff

import (
	"bytes"
	"fmt"
	"strings"
)

// DefaultContext is the number of unchanged lines shown around each hunk.
const DefaultContext = 3

// IsBinary reports whether content should be treated as binary (a NUL byte in
// the first 8000 bytes, git's heuristic).
func IsBinary(b []byte) bool {
	n := len(b)
	if n > 8000 {
		n = 8000
	}
	return bytes.IndexByte(b[:n], 0) >= 0
}

// Unified returns a unified diff of a→b labelled labelA/labelB with `context`
// lines of surrounding context. The second result is true when the diff is
// empty (a and b are identical). Binary inputs must be handled by the caller via
// IsBinary; Unified assumes text.
func Unified(a, b []byte, labelA, labelB string, context int) (string, bool) {
	if bytes.Equal(a, b) {
		return "", true
	}
	if context < 0 {
		context = DefaultContext
	}
	al, aNL := splitLines(a)
	bl, bNL := splitLines(b)
	ops := diffOps(al, bl)

	// A difference that is only the trailing newline leaves every line as context;
	// surface it by showing the last line as changed, so the "\ No newline" marker
	// appears (the bytes differ, or bytes.Equal above would have returned).
	if aNL != bNL && !anyChanged(ops) && len(ops) > 0 {
		last := ops[len(ops)-1]
		ops = append(ops[:len(ops)-1], op{kind: '-', ai: last.ai}, op{kind: '+', bi: last.bi})
	}

	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", labelA, labelB)
	for _, h := range hunks(ops, context) {
		writeHunk(&out, h, al, bl, aNL, bNL)
	}
	return out.String(), false
}

// Stat returns the number of added and removed lines between a and b.
func Stat(a, b []byte) (added, removed int) {
	al, _ := splitLines(a)
	bl, _ := splitLines(b)
	for _, o := range diffOps(al, bl) {
		switch o.kind {
		case '+':
			added++
		case '-':
			removed++
		}
	}
	return added, removed
}

// splitLines splits content into lines without their trailing newline, and
// reports whether the content ended with a newline (so "\ No newline at end of
// file" can be emitted faithfully). Empty content is zero lines.
func splitLines(b []byte) ([]string, bool) {
	if len(b) == 0 {
		return nil, true
	}
	s := string(b)
	endsNL := strings.HasSuffix(s, "\n")
	if endsNL {
		s = s[:len(s)-1]
	}
	return strings.Split(s, "\n"), endsNL
}

// op is one edit-script entry: a context (keep), delete, or insert line, with the
// 0-based source index on the relevant side.
type op struct {
	kind byte // ' ' keep, '-' delete (index into a), '+' insert (index into b)
	ai   int
	bi   int
}

// diffOps produces a linear edit script aligning a to b via their LCS.
func diffOps(a, b []string) []op {
	pairs := lcs(a, b)
	var ops []op
	i, j := 0, 0
	for _, p := range pairs {
		for i < p[0] {
			ops = append(ops, op{kind: '-', ai: i})
			i++
		}
		for j < p[1] {
			ops = append(ops, op{kind: '+', bi: j})
			j++
		}
		ops = append(ops, op{kind: ' ', ai: i, bi: j})
		i++
		j++
	}
	for i < len(a) {
		ops = append(ops, op{kind: '-', ai: i})
		i++
	}
	for j < len(b) {
		ops = append(ops, op{kind: '+', bi: j})
		j++
	}
	return ops
}

func anyChanged(ops []op) bool {
	for _, o := range ops {
		if o.kind != ' ' {
			return true
		}
	}
	return false
}

// lcs returns the index pairs of a longest common subsequence of a and b, in
// increasing order (the alignment of unchanged lines).
func lcs(a, b []string) [][2]int {
	n, m := len(a), len(b)
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

// hunk is a contiguous run of ops plus the 1-based start lines and lengths.
type hunk struct {
	ops          []op
	aStart, aLen int
	bStart, bLen int
}

// hunks groups the edit script into unified-diff hunks with `context` lines of
// context, merging runs that are closer together than 2*context.
func hunks(ops []op, context int) []hunk {
	// Indices of changed ops.
	var changed []int
	for i, o := range ops {
		if o.kind != ' ' {
			changed = append(changed, i)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	var groups [][2]int // [lo,hi] op-index ranges to emit
	lo := max0(changed[0] - context)
	hi := min(len(ops), changed[0]+context+1)
	for _, c := range changed[1:] {
		nlo := max0(c - context)
		if nlo <= hi {
			hi = min(len(ops), c+context+1)
			continue
		}
		groups = append(groups, [2]int{lo, hi})
		lo, hi = nlo, min(len(ops), c+context+1)
	}
	groups = append(groups, [2]int{lo, hi})

	var hs []hunk
	for _, g := range groups {
		seg := ops[g[0]:g[1]]
		h := hunk{ops: seg}
		h.aStart, h.bStart = -1, -1
		for _, o := range seg {
			if o.kind == ' ' || o.kind == '-' {
				if h.aStart < 0 {
					h.aStart = o.ai
				}
				h.aLen++
			}
			if o.kind == ' ' || o.kind == '+' {
				if h.bStart < 0 {
					h.bStart = o.bi
				}
				h.bLen++
			}
		}
		if h.aStart < 0 {
			h.aStart = 0
		}
		if h.bStart < 0 {
			h.bStart = 0
		}
		hs = append(hs, h)
	}
	return hs
}

func writeHunk(out *strings.Builder, h hunk, a, b []string, aNL, bNL bool) {
	fmt.Fprintf(out, "@@ -%s +%s @@\n", rangeStr(h.aStart, h.aLen), rangeStr(h.bStart, h.bLen))
	for _, o := range h.ops {
		switch o.kind {
		case ' ':
			writeLine(out, ' ', a[o.ai], o.ai == len(a)-1 && !aNL)
		case '-':
			writeLine(out, '-', a[o.ai], o.ai == len(a)-1 && !aNL)
		case '+':
			writeLine(out, '+', b[o.bi], o.bi == len(b)-1 && !bNL)
		}
	}
}

func writeLine(out *strings.Builder, sign byte, line string, noNewline bool) {
	out.WriteByte(sign)
	out.WriteString(line)
	out.WriteByte('\n')
	if noNewline {
		out.WriteString("\\ No newline at end of file\n")
	}
}

// rangeStr renders a unified hunk range: 1-based start,length, with the git
// convention that a zero-length range starts at the line it follows.
func rangeStr(start0, length int) string {
	if length == 0 {
		return fmt.Sprintf("%d,0", start0)
	}
	return fmt.Sprintf("%d,%d", start0+1, length)
}

func max0(x int) int {
	if x < 0 {
		return 0
	}
	return x
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
