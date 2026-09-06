package mcp

import (
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/core"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/worktree"
)

// The checkout-scope regression suite asserts that `diff` and `status` called
// from inside a task — by the gate and by the CLI — give *identical* results.
// Both shells present `core.Diff` / `worktree.Compare` verbatim (that is the
// point of the one-core-two-shells unification, Tier U), so this pins the gate's
// output against the same core the CLI renders: a future change that made the
// gate post-process a diff differently from the CLI would fail here rather than
// drift unnoticed. (Test files may reach into the store; the shell-purity guard
// covers only non-test source.)

func blobReaderFor(g *gateFixture, states map[string]worktree.FileState) core.BytesFn {
	return func(p string) ([]byte, bool, error) {
		s, ok := states[p]
		if !ok || s.Hash == nil {
			return nil, false, nil
		}
		o, err := g.repo.Objects.Get(s.Hash)
		if err != nil {
			return nil, false, err
		}
		c, ok := o.BlobContent()
		return c, ok, nil
	}
}

// coreDiffOf computes the diff the CLI would render for a stored change: its
// first parent's tree against its own tree, through the shared core.
func coreDiffOf(t *testing.T, f *gateFixture, parent, change multihash.Multihash) core.DiffResult {
	t.Helper()
	parentTree, err := core.TreeOf(f.repo, parent)
	if err != nil {
		t.Fatal(err)
	}
	changeTree, err := core.TreeOf(f.repo, change)
	if err != nil {
		t.Fatal(err)
	}
	base, err := worktree.FlattenStates(f.repo.Objects, parentTree)
	if err != nil {
		t.Fatal(err)
	}
	work, err := worktree.FlattenStates(f.repo.Objects, changeTree)
	if err != nil {
		t.Fatal(err)
	}
	return core.Diff(base, work, blobReaderFor(f, base), blobReaderFor(f, work))
}

// TestGateDiffEqualsCoreDiff: the gate's varvig_diff is byte-identical to the
// core diff the CLI prints, for the same change.
func TestGateDiffEqualsCoreDiff(t *testing.T) {
	f := newGateFixture(t)
	gate, _ := newGate(f, "src/auth", time.Hour)
	changeHex := proposeChange(t, gate, "src/auth/login.go", "package auth // patched\n")

	resps := drive(t, gate, call(1, "varvig_diff", `{"change":"`+changeHex+`"}`))
	var gate1 struct {
		Changed []string `json:"changed"`
		Unified string   `json:"unified"`
	}
	if err := json.Unmarshal(decodeTool(t, resps[0]).StructuredContent, &gate1); err != nil {
		t.Fatal(err)
	}

	change, err := multihash.ParseHex(changeHex)
	if err != nil {
		t.Fatal(err)
	}
	cli := coreDiffOf(t, f, f.base, change)

	if gate1.Unified != cli.Unified {
		t.Fatalf("gate and CLI diffs differ:\n--- gate ---\n%s\n--- cli ---\n%s", gate1.Unified, cli.Unified)
	}
	if !equalStrings(gate1.Changed, cli.Changed) {
		t.Fatalf("changed sets differ: gate=%v cli=%v", gate1.Changed, cli.Changed)
	}
}

// TestGateStatusEqualsCoreCompare: the gate's varvig_status sets match
// worktree.Compare — the same summary the CLI's status renders.
func TestGateStatusEqualsCoreCompare(t *testing.T) {
	f := newGateFixture(t)
	gate, _ := newGate(f, "src/auth", time.Hour)
	changeHex := proposeChange(t, gate, "src/auth/new.go", "package auth\n")

	resps := drive(t, gate, call(1, "varvig_status", `{"change":"`+changeHex+`"}`))
	var st struct {
		Clean    bool     `json:"clean"`
		Added    []string `json:"added"`
		Modified []string `json:"modified"`
		Deleted  []string `json:"deleted"`
	}
	if err := json.Unmarshal(decodeTool(t, resps[0]).StructuredContent, &st); err != nil {
		t.Fatal(err)
	}

	change, err := multihash.ParseHex(changeHex)
	if err != nil {
		t.Fatal(err)
	}
	parentTree, _ := core.TreeOf(f.repo, f.base)
	changeTree, _ := core.TreeOf(f.repo, change)
	base, _ := worktree.FlattenStates(f.repo.Objects, parentTree)
	work, _ := worktree.FlattenStates(f.repo.Objects, changeTree)
	d := worktree.Compare(base, work)

	if st.Clean != d.Empty() {
		t.Fatalf("clean = %v, core Empty = %v", st.Clean, d.Empty())
	}
	if !equalStrings(st.Added, d.Added) || !equalStrings(st.Modified, d.Modified) || !equalStrings(st.Deleted, d.Removed) {
		t.Fatalf("status sets differ from core: gate(add=%v mod=%v del=%v) core(add=%v mod=%v del=%v)",
			st.Added, st.Modified, st.Deleted, d.Added, d.Modified, d.Removed)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as, bs := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}
