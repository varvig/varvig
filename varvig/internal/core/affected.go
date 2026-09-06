package core

// "What does this change actually affect" (design §1.3) belongs to the shared
// core, not to a shell. Package affected holds the analysis; this file holds the
// wiring every caller needs to get the same answer out of it — where the index
// lives, which analyzers are in force, and how a revision becomes a tree — so
// the CLI and the read-gate cannot drift into computing it two ways.
//
// That drift is not hypothetical: this wiring lived in cmd/varvig for its whole
// life, which is why `varvig affected` had no gate counterpart at all. It is the
// v0.2.0 `diff` failure in its quieter form — not two shells disagreeing, but one
// shell holding a capability the other could not reach (design addendum, U1).

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/affected"
	"github.com/dividebyzero/claude-experiments/varvig/internal/hook"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// analyzerEventPrefix is the hook-manifest event that registers a language
// analyzer: "analyze:<ext>" (design §3.3). New languages arrive as
// content-addressed wasm modules, so learning one recompiles nothing.
const analyzerEventPrefix = "analyze:"

// AnalyzerRef names one analyzer that took part in building a graph: the file
// extension it handles and the content id of its wasm module. The module id is
// the honest identity — two analyzers for the same extension are the same
// analyzer only if their bytes agree — which is what lets a later index key
// itself by the analyzer set and self-invalidate when one changes (GRAPH.md
// §11.1). It is recorded, not just used, so a caller can say which analysis
// produced an answer.
type AnalyzerRef struct {
	Ext    string
	Module multihash.Multihash
}

// Coverage says which of the analyzed tree's files an analyzer actually
// understood. It is not optional decoration: an affected set computed over a
// language no analyzer covers is indistinguishable, in shape, from one computed
// over a fully analyzed tree, and a caller that cannot tell them apart will read
// a coverage gap as an absence of dependency (design §5, GRAPH.md §11.4).
//
// It is derived from the analyzer set that actually ran against the tree, never
// declared, so it cannot drift from what happened.
type Coverage struct {
	// Analyzed is the number of files a built-in or registered wasm analyzer
	// understood.
	Analyzed int
	// Unanalyzed is the number of files no analyzer understood. Such a file
	// contributes only itself when it changes: never a false claim that nothing
	// depends on it.
	Unanalyzed int
	// UnanalyzedExts is the distinct extensions among those files, sorted, so a
	// caller can name the gap rather than only count it. Files with no extension
	// are reported as "".
	UnanalyzedExts []string
}

// Complete reports whether every file in the tree was understood by an analyzer.
// A false here is the signal that an absent edge may be a gap rather than a fact.
func (c Coverage) Complete() bool { return c.Unanalyzed == 0 }

// AffectedResult is the answer to "what does this change affect".
//
// Affected is a superset of Changed: it is the transitive closure of the changed
// files plus everything that depends on them. The analysis degrades gracefully
// (design §5) — a file whose language no analyzer covers contributes only itself
// — so Affected under-reports coverage rather than over-claiming safety.
type AffectedResult struct {
	// Changed is the paths that differ between the two trees.
	Changed []string
	// Affected is Changed plus their transitive dependents, sorted.
	Affected []string
	// Analyzers is the analyzer set that produced the dependency graph, sorted
	// by extension. Empty means no wasm analyzer was registered and only the
	// built-in extractors ran.
	Analyzers []AnalyzerRef
	// Coverage says how much of the tree the analysis actually understood. It
	// travels with every result because an answer without it cannot be read
	// safely.
	Coverage Coverage
}

// Pulled reports whether p is in the affected set only by dependency — it was
// not itself changed. It is what distinguishes "you edited this" from "this is
// downstream of what you edited", which every renderer of an affected set needs
// and none should recompute.
func (a AffectedResult) Pulled(p string) bool {
	if !contains(a.Affected, p) {
		return false
	}
	return !contains(a.Changed, p)
}

// IndexDir is where derived indices live. They are caches: disposable, keyed by
// content, and safe to delete at any time, because no identity depends on them
// (design §4.3). Core owns the location so both shells share one index rather
// than each choosing its own.
func IndexDir(r *repo.Repo) string { return filepath.Join(r.GitDir(), "index") }

// Analyzers resolves the repository's registered wasm language analyzers from
// the hook manifest, ready to run in the same sandbox as any other hook. The
// result is sorted by extension so an analyzer set has one spelling.
func Analyzers(r *repo.Repo) ([]affected.WasmAnalyzer, error) {
	cfg, err := hook.LoadManifest(r)
	if err != nil {
		return nil, err
	}
	runner := func(ctx context.Context, module, input []byte) ([]byte, error) {
		res, err := hook.Run(ctx, module, input)
		if err != nil {
			return nil, err
		}
		if !res.Allowed() {
			return nil, fmt.Errorf("core: analyzer exited %d: %s",
				res.ExitCode, strings.TrimSpace(string(res.Stderr)))
		}
		return res.Stdout, nil
	}
	var out []affected.WasmAnalyzer
	for _, e := range cfg.Entries {
		ext, ok := strings.CutPrefix(e.Event, analyzerEventPrefix)
		if !ok {
			continue
		}
		obj, err := r.Objects.Get(e.Module)
		if err != nil {
			return nil, fmt.Errorf("core: analyzer module for %q: %w", e.Event, err)
		}
		mod, ok := obj.BlobContent()
		if !ok {
			return nil, fmt.Errorf("core: analyzer module for %q is not a blob", e.Event)
		}
		out = append(out, affected.WasmAnalyzer{Ext: ext, Module: mod, ID: e.Module, Run: runner})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ext < out[j].Ext })
	return out, nil
}

// Affected answers "what does this change affect" between two revisions. base
// and new are each a change id or a tree id (TreeOf's contract), so a caller
// need not know which it holds; a nil base is the empty tree, which makes every
// file in new an addition.
//
// The specifier cache under IndexDir makes this incremental across runs: a
// file's dependencies are a function of its content, so an unchanged blob is
// never re-analyzed, and a wasm-analyzed file is keyed by its analyzer module
// too (package affected). Cache misses cost analysis, never correctness.
func Affected(r *repo.Repo, base, new multihash.Multihash) (AffectedResult, error) {
	baseTree, err := TreeOf(r, base)
	if err != nil {
		return AffectedResult{}, err
	}
	newTree, err := TreeOf(r, new)
	if err != nil {
		return AffectedResult{}, err
	}

	diff, err := affected.DiffTrees(r.Objects, baseTree, newTree)
	if err != nil {
		return AffectedResult{}, err
	}
	changed := diff.Changed()

	cache, err := affected.NewDiskCache(filepath.Join(IndexDir(r), "deps"))
	if err != nil {
		return AffectedResult{}, err
	}
	wasm, err := Analyzers(r)
	if err != nil {
		return AffectedResult{}, err
	}
	graph, err := affected.BuildGraph(r.Objects, newTree, affected.Options{Cache: cache, Wasm: wasm})
	if err != nil {
		return AffectedResult{}, err
	}

	refs := make([]AnalyzerRef, 0, len(wasm))
	covered := make(map[string]bool, len(wasm))
	for _, wa := range wasm {
		refs = append(refs, AnalyzerRef{Ext: wa.Ext, Module: wa.ID})
		covered[strings.ToLower(wa.Ext)] = true
	}
	return AffectedResult{
		Changed:   changed,
		Affected:  graph.Affected(changed),
		Analyzers: refs,
		Coverage:  coverageOf(graph.Files, covered),
	}, nil
}

// coverageOf counts what the analysis understood. A file is covered if a
// built-in analyzer handles it or a wasm analyzer is registered for its
// extension — the two sources package affected consults, so this cannot claim
// coverage the graph did not have.
func coverageOf(files map[string]multihash.Multihash, wasmExts map[string]bool) Coverage {
	var c Coverage
	gaps := map[string]bool{}
	for p := range files {
		if affected.BuiltinCovers(p) || wasmExts[strings.ToLower(extOf(p))] {
			c.Analyzed++
			continue
		}
		c.Unanalyzed++
		gaps[strings.ToLower(extOf(p))] = true
	}
	c.UnanalyzedExts = make([]string, 0, len(gaps))
	for e := range gaps {
		c.UnanalyzedExts = append(c.UnanalyzedExts, e)
	}
	sort.Strings(c.UnanalyzedExts)
	return c
}

// extOf is the file extension of a repo path, including the dot, or "" when the
// final path segment has none.
func extOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		p = p[i+1:]
	}
	if i := strings.LastIndexByte(p, '.'); i > 0 {
		return p[i:]
	}
	return ""
}

// FirstParent returns a change's first parent, or nil if it has none (a root
// change) or if id is not a change at all. It is what "this change against what
// came before" resolves to, and it lives here so a shell forbidden from decoding
// objects can still ask the question.
func FirstParent(r *repo.Repo, id multihash.Multihash) (multihash.Multihash, error) {
	if id == nil {
		return nil, nil
	}
	o, err := r.Objects.Get(id)
	if err != nil {
		return nil, err
	}
	if o.Type() != object.TypeChange {
		return nil, nil
	}
	c, err := o.AsChange()
	if err != nil {
		return nil, err
	}
	if len(c.Parents) == 0 {
		return nil, nil
	}
	return c.Parents[0], nil
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
