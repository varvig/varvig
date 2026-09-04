package core

import (
	"fmt"
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/textdiff"
	"github.com/dividebyzero/claude-experiments/varvig/internal/worktree"
)

// This is the read side of the shared core: the diff and status computations both
// shells present. The CLI prints them to a terminal; the gate returns them as
// JSON. The rendering must be identical either way — an agent reading a diff
// through the gate and a human reading one at the CLI are looking at the same
// change — so it lives here once (design addendum, U1: the read verbs move into
// the core so the gate can expose them, which is what makes diff/status reachable
// from a task at all).

// BytesFn returns a path's content on one side of a diff. ok is false when the
// path is absent on that side (an add has no old side; a delete has no new side).
type BytesFn func(path string) (content []byte, ok bool, err error)

// StatLine is one path's per-file line delta, or a note for a change with no line
// delta (a mode change or a rename).
type StatLine struct {
	Path    string
	Added   int
	Removed int
	Note    string
}

// DiffResult is a computed diff rendered to the forms both shells need: the raw
// TreeDiff, the changed-path set (what a proposal observes), the unified text,
// and per-path stats.
type DiffResult struct {
	Diff    worktree.TreeDiff
	Changed []string
	Unified string
	Stat    []StatLine
}

// Diff compares two flattened states and renders the result. oldBytes reads a
// path's content on the base side; newBytes on the new side (a stored tree, or a
// live working tree the caller reads from disk). Read errors are treated as empty
// content rather than failing the whole diff, matching a diff's best-effort
// nature over content it can always re-derive.
func Diff(base, work map[string]worktree.FileState, oldBytes, newBytes BytesFn) DiffResult {
	d := worktree.Compare(base, work)
	res := DiffResult{Diff: d, Changed: ChangedPaths(d)}
	res.Stat = diffStat(d, oldBytes, newBytes)
	res.Unified = diffUnified(d, base, work, oldBytes, newBytes)
	return res
}

// ChangedPaths lists every path a diff touched, one class after another — exactly
// the set a proposal observes (build spec P0.2 / P1.1).
func ChangedPaths(d worktree.TreeDiff) []string {
	var ps []string
	ps = append(ps, d.Added...)
	ps = append(ps, d.Modified...)
	ps = append(ps, d.ModeChanged...)
	ps = append(ps, d.Removed...)
	for _, rn := range d.Renamed {
		ps = append(ps, rn.To)
	}
	return ps
}

func read(fn BytesFn, p string) []byte {
	b, _, _ := fn(p)
	return b
}

func diffStat(d worktree.TreeDiff, oldBytes, newBytes BytesFn) []StatLine {
	var out []StatLine
	for _, p := range d.Added {
		a, r := textdiff.Stat(nil, read(newBytes, p))
		out = append(out, StatLine{Path: p, Added: a, Removed: r})
	}
	for _, p := range d.Modified {
		a, r := textdiff.Stat(read(oldBytes, p), read(newBytes, p))
		out = append(out, StatLine{Path: p, Added: a, Removed: r})
	}
	for _, p := range d.Removed {
		a, r := textdiff.Stat(read(oldBytes, p), nil)
		out = append(out, StatLine{Path: p, Added: a, Removed: r})
	}
	for _, p := range d.ModeChanged {
		out = append(out, StatLine{Path: p, Note: "mode"})
	}
	for _, rn := range d.Renamed {
		out = append(out, StatLine{Path: rn.From + " => " + rn.To, Note: "rename"})
	}
	return out
}

func diffUnified(d worktree.TreeDiff, base, work map[string]worktree.FileState, oldBytes, newBytes BytesFn) string {
	var b strings.Builder
	// git-style side labels: a real path gets an a//b/ prefix; /dev/null does not.
	label := func(side, p string) string {
		if p == "/dev/null" {
			return p
		}
		return side + p
	}
	emit := func(pathA, pathB string, a, bb []byte) {
		la, lb := label("a/", pathA), label("b/", pathB)
		fmt.Fprintf(&b, "diff --varvig %s %s\n", la, lb)
		if textdiff.IsBinary(a) || textdiff.IsBinary(bb) {
			fmt.Fprintf(&b, "Binary files %s and %s differ\n", la, lb)
			return
		}
		body, empty := textdiff.Unified(a, bb, la, lb, textdiff.DefaultContext)
		if !empty {
			b.WriteString(body)
		}
	}
	for _, p := range d.Added {
		emit("/dev/null", p, nil, read(newBytes, p))
	}
	for _, p := range d.Modified {
		emit(p, p, read(oldBytes, p), read(newBytes, p))
	}
	for _, p := range d.ModeChanged {
		fmt.Fprintf(&b, "diff --varvig a/%s b/%s\nold mode %o\nnew mode %o\n", p, p, base[p].Mode, work[p].Mode)
	}
	for _, p := range d.Removed {
		emit(p, "/dev/null", read(oldBytes, p), nil)
	}
	for _, rn := range d.Renamed {
		fmt.Fprintf(&b, "diff --varvig a/%s b/%s\nrename from %s\nrename to %s\n", rn.From, rn.To, rn.From, rn.To)
	}
	return b.String()
}
