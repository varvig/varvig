package agentrules

import (
	"fmt"
	"strings"
)

// unifiedDiff renders a compact, deterministic line diff of old→new labeled with
// path. It is not a full git-grade diff — it exists so `--diff` and the
// "here is what was replaced" notice show a human what changed. Empty string
// means the inputs are identical.
//
// The algorithm is a standard longest-common-subsequence over lines, then a
// single hunk walk emitting context, deletions, and insertions. Output is
// deterministic (no timestamps), which `--check`/golden tests depend on.
func unifiedDiff(path, old, new string) string {
	if old == new {
		return ""
	}
	a := splitLines(old)
	b := splitLines(new)
	ops := lcsDiff(a, b)

	var out strings.Builder
	fmt.Fprintf(&out, "--- %s (on disk)\n", path)
	fmt.Fprintf(&out, "+++ %s (generated)\n", path)
	for _, op := range ops {
		switch op.kind {
		case opEqual:
			fmt.Fprintf(&out, " %s\n", op.line)
		case opDel:
			fmt.Fprintf(&out, "-%s\n", op.line)
		case opAdd:
			fmt.Fprintf(&out, "+%s\n", op.line)
		}
	}
	return out.String()
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

type opKind int

const (
	opEqual opKind = iota
	opDel
	opAdd
)

type diffOp struct {
	kind opKind
	line string
}

// lcsDiff computes a line-level diff via an LCS-length DP table and a backtrace.
// Inputs here are small (a rules file), so the O(n·m) table is fine.
func lcsDiff(a, b []string) []diffOp {
	n, m := len(a), len(b)
	// dp[i][j] = LCS length of a[i:] and b[j:].
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
	var ops []diffOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{opEqual, a[i]})
			i, j = i+1, j+1
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, diffOp{opDel, a[i]})
			i++
		default:
			ops = append(ops, diffOp{opAdd, b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{opDel, a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{opAdd, b[j]})
	}
	return ops
}
