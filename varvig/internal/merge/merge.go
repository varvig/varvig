package merge

import (
	"bytes"
	"context"
	"sort"
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
)

// ObjectStore is the read/write surface the merge driver needs.
type ObjectStore interface {
	Get(multihash.Multihash) (*object.Object, error)
	Put(*object.Object) (multihash.Multihash, error)
}

// RegenRequest is handed to a Regenerator for a conflicted file. It carries the
// incoming change's intent (its provenance) plus the three file versions, so a
// model can re-derive the change against the new base (design §1.2).
type RegenRequest struct {
	Path     string
	Intent   object.Provenance // the incoming (theirs) change's recorded intent
	Ancestor []byte            // the merge-base version
	Ours     []byte            // the current base going forward
	Theirs   []byte            // the incoming version being re-applied
}

// Regenerator re-runs an intent against a new base to resolve a conflict. It is
// intentionally out-of-process behind this interface (design §3.3): the driver
// never embeds a model. It returns the resolved content, or an error to decline
// (the driver then falls back to a textual conflict).
type Regenerator interface {
	Regenerate(ctx context.Context, req RegenRequest) ([]byte, error)
}

// Conflict is a path the driver could not resolve automatically.
type Conflict struct {
	Path   string
	Reason string
}

// Result is the outcome of a merge.
type Result struct {
	Tree        multihash.Multihash // merged tree (with conflict markers for unresolved paths)
	Change      multihash.Multihash // merge change (parents ours,theirs) — set only if fully resolved
	Base        multihash.Multihash // merge base change used, or nil
	Clean       []string            // paths taken from a single side
	TextMerged  []string            // paths resolved by clean textual three-way merge
	Regenerated []string            // paths resolved by regeneration
	Conflicts   []Conflict          // unresolved paths (markers left in the tree)
}

// Resolved reports whether the merge completed with no unresolved conflicts.
func (r Result) Resolved() bool { return len(r.Conflicts) == 0 }

// Merge computes a three-way merge of theirs into ours and returns the merged
// tree. Conflicted files are resolved by regeneration when a Regenerator is
// supplied and the incoming change carries intent; otherwise they fall back to
// a textual three-way merge, and finally to conflict markers. When nothing is
// left unresolved a merge change (parents ours, theirs) is created.
func Merge(ctx context.Context, objs ObjectStore, ours, theirs multihash.Multihash, reg Regenerator) (Result, error) {
	bases := MergeBases(objs, ours, theirs)
	baseTree, err := virtualBaseTree(ctx, objs, bases, reg)
	if err != nil {
		return Result{}, err
	}
	oursTree, err := treeOf(objs, ours)
	if err != nil {
		return Result{}, err
	}
	theirsTree, err := treeOf(objs, theirs)
	if err != nil {
		return Result{}, err
	}

	// The incoming change's intent, if any, drives regeneration.
	intent, hasIntent := changeIntent(objs, theirs)

	mt, err := mergeTrees(ctx, objs, baseTree, oursTree, theirsTree, intent, hasIntent, reg)
	if err != nil {
		return Result{}, err
	}

	res := Result{
		Tree:        mt.tree,
		Clean:       mt.clean,
		TextMerged:  mt.textMerged,
		Regenerated: mt.regenerated,
		Conflicts:   mt.conflicts,
	}
	if len(bases) == 1 {
		res.Base = bases[0]
	} // multiple (or zero) bases => virtual/empty base, Base left nil

	if res.Resolved() {
		change := object.NewChange(object.Change{
			Tree:    mt.tree,
			Parents: []multihash.Multihash{ours, theirs},
			Message: "merge",
		})
		id, err := objs.Put(change)
		if err != nil {
			return Result{}, err
		}
		res.Change = id
	}
	return res, nil
}

// treeMerge is the outcome of merging three trees into one.
type treeMerge struct {
	tree        multihash.Multihash
	clean       []string
	textMerged  []string
	regenerated []string
	conflicts   []Conflict
}

// mergeTrees performs the per-file three-way merge of ours and theirs against a
// base tree, returning the merged tree and per-path outcomes. It is the shared
// core used both for the top-level merge and for synthesizing a virtual base.
func mergeTrees(ctx context.Context, objs ObjectStore, baseTree, oursTree, theirsTree multihash.Multihash,
	intent object.Provenance, hasIntent bool, reg Regenerator) (treeMerge, error) {

	bf, err := flatten(objs, baseTree)
	if err != nil {
		return treeMerge{}, err
	}
	of, err := flatten(objs, oursTree)
	if err != nil {
		return treeMerge{}, err
	}
	tf, err := flatten(objs, theirsTree)
	if err != nil {
		return treeMerge{}, err
	}

	var mt treeMerge
	merged := map[string]fileEnt{}
	for _, path := range unionPaths(bf, of, tf) {
		b, hasB := bf[path]
		o, hasO := of[path]
		tv, hasT := tf[path]

		switch {
		case hasO && hasT && eqID(o.ID, tv.ID):
			merged[path] = o
			mt.clean = append(mt.clean, path)
		case !hasO && !hasT:
			// deleted on both sides: nothing to emit
		case sameAsBase(o, hasO, b, hasB):
			if hasT {
				merged[path] = tv
			}
			mt.clean = append(mt.clean, path)
		case sameAsBase(tv, hasT, b, hasB):
			if hasO {
				merged[path] = o
			}
			mt.clean = append(mt.clean, path)
		default:
			fe, method, conflict, err := resolveConflict(ctx, objs, path, b, hasB, o, hasO, tv, hasT, intent, hasIntent, reg)
			if err != nil {
				return treeMerge{}, err
			}
			if fe != nil {
				merged[path] = *fe
			}
			switch method {
			case "text":
				mt.textMerged = append(mt.textMerged, path)
			case "regen":
				mt.regenerated = append(mt.regenerated, path)
			}
			if conflict {
				mt.conflicts = append(mt.conflicts, Conflict{Path: path, Reason: "diverged edits"})
			}
		}
	}
	tree, err := buildTree(objs, merged)
	if err != nil {
		return treeMerge{}, err
	}
	mt.tree = tree
	return mt, nil
}

// virtualBaseTree returns the base tree for a merge given its best common
// ancestors (design §1.2 / recursive merge). With zero it is the empty tree;
// with one it is that ancestor's tree; with more (a criss-cross history) the
// ancestors are recursively merged into a synthesized virtual base — so the
// top-level merge measures "changed" against a base that already reconciles the
// ancestors, avoiding spurious conflicts. The two-ancestor case (by far the
// most common) is exact; with more than two, later folds use an empty base,
// which is conservative (may surface extra conflicts, never a wrong merge).
func virtualBaseTree(ctx context.Context, objs ObjectStore, bases []multihash.Multihash, reg Regenerator) (multihash.Multihash, error) {
	switch len(bases) {
	case 0:
		return nil, nil
	case 1:
		return treeOf(objs, bases[0])
	}
	accTree, err := treeOf(objs, bases[0])
	if err != nil {
		return nil, err
	}
	for i := 1; i < len(bases); i++ {
		var subBase multihash.Multihash
		if i == 1 {
			// Recurse on the two ancestors' own merge bases (strictly older
			// commits, so this terminates).
			subBase, err = virtualBaseTree(ctx, objs, MergeBases(objs, bases[0], bases[1]), reg)
			if err != nil {
				return nil, err
			}
		}
		biTree, err := treeOf(objs, bases[i])
		if err != nil {
			return nil, err
		}
		mt, err := mergeTrees(ctx, objs, subBase, accTree, biTree, object.Provenance{}, false, nil)
		if err != nil {
			return nil, err
		}
		accTree = mt.tree
	}
	return accTree, nil
}

// resolveConflict handles a path that diverged on both sides. It returns the
// resolved entry (nil to omit the file), the method used, and whether the
// result still contains an unresolved conflict.
func resolveConflict(ctx context.Context, objs ObjectStore, path string,
	b fileEnt, hasB bool, o fileEnt, hasO bool, tv fileEnt, hasT bool,
	intent object.Provenance, hasIntent bool, reg Regenerator) (*fileEnt, string, bool, error) {

	baseC, err := content(objs, b, hasB)
	if err != nil {
		return nil, "", false, err
	}
	oursC, err := content(objs, o, hasO)
	if err != nil {
		return nil, "", false, err
	}
	theirsC, err := content(objs, tv, hasT)
	if err != nil {
		return nil, "", false, err
	}

	mergedLines, conflict := merge3(lines(baseC), lines(oursC), lines(theirsC))
	if !conflict {
		fe, err := putBlob(objs, []byte(strings.Join(mergedLines, "")), pickMode(o, hasO, tv, hasT))
		if err != nil {
			return nil, "", false, err
		}
		return &fe, "text", false, nil
	}

	// Textual merge conflicts: re-run the incoming intent against ours (§1.2).
	if reg != nil && hasIntent {
		out, rerr := reg.Regenerate(ctx, RegenRequest{
			Path: path, Intent: intent, Ancestor: baseC, Ours: oursC, Theirs: theirsC,
		})
		if rerr == nil {
			fe, err := putBlob(objs, out, pickMode(o, hasO, tv, hasT))
			if err != nil {
				return nil, "", false, err
			}
			return &fe, "regen", false, nil
		}
		// Regenerator declined: fall through to markers.
	}

	// Fallback: keep the conflict-markered textual result (design §1.2).
	fe, err := putBlob(objs, []byte(strings.Join(mergedLines, "")), pickMode(o, hasO, tv, hasT))
	if err != nil {
		return nil, "", false, err
	}
	return &fe, "", true, nil
}

// MergeBases returns the best common ancestors of two changes: the common
// ancestors that are not themselves ancestors of another common ancestor (the
// maximal elements of the common set). A linear history yields one; a
// criss-cross history yields several. The result is sorted by id for
// determinism. Unrelated histories yield none.
func MergeBases(objs ObjectStore, a, b multihash.Multihash) []multihash.Multihash {
	if a == nil || b == nil {
		return nil
	}
	ancA := map[string]bool{}
	collectAncestors(objs, a, ancA)
	ancB := map[string]bool{}
	collectAncestors(objs, b, ancB)

	// Common ancestors, and the id -> change lookup for them.
	common := map[string]multihash.Multihash{}
	for hexID := range ancA {
		if ancB[hexID] {
			common[hexID] = nil
		}
	}
	// Resolve ids we can reach to real multihashes by re-walking from a.
	idOf := map[string]multihash.Multihash{}
	collectAncestorIDs(objs, a, idOf)

	// Mark every strict ancestor of every common commit; a common commit so
	// marked is dominated by another and is therefore not a best base.
	dominated := map[string]bool{}
	for hexID := range common {
		id := idOf[hexID]
		if id == nil {
			continue
		}
		for _, p := range parents(objs, id) {
			collectAncestors(objs, p, dominated)
		}
	}

	var out []multihash.Multihash
	for hexID := range common {
		if dominated[hexID] {
			continue
		}
		if id := idOf[hexID]; id != nil {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i], out[j]) < 0 })
	return out
}

// MergeBase returns a single best common ancestor (the lowest by id), or nil.
// Retained for callers that assume a unique base; MergeBases is the general
// form.
func MergeBase(objs ObjectStore, a, b multihash.Multihash) multihash.Multihash {
	bs := MergeBases(objs, a, b)
	if len(bs) == 0 {
		return nil
	}
	return bs[0]
}

// collectAncestorIDs records id -> multihash for a change and all its ancestors.
func collectAncestorIDs(objs ObjectStore, id multihash.Multihash, out map[string]multihash.Multihash) {
	if id == nil || out[id.Hex()] != nil {
		return
	}
	out[id.Hex()] = id
	for _, p := range parents(objs, id) {
		collectAncestorIDs(objs, p, out)
	}
}

func collectAncestors(objs ObjectStore, id multihash.Multihash, set map[string]bool) {
	if id == nil || set[id.Hex()] {
		return
	}
	set[id.Hex()] = true
	for _, p := range parents(objs, id) {
		collectAncestors(objs, p, set)
	}
}

func parents(objs ObjectStore, id multihash.Multihash) []multihash.Multihash {
	obj, err := objs.Get(id)
	if err != nil {
		return nil
	}
	if obj.Type() != object.TypeChange {
		return nil
	}
	c, err := obj.AsChange()
	if err != nil {
		return nil
	}
	return c.Parents
}

// changeIntent returns the provenance (intent) recorded by a change, if any.
func changeIntent(objs ObjectStore, change multihash.Multihash) (object.Provenance, bool) {
	if change == nil {
		return object.Provenance{}, false
	}
	obj, err := objs.Get(change)
	if err != nil {
		return object.Provenance{}, false
	}
	c, err := obj.AsChange()
	if err != nil || c.Provenance == nil {
		return object.Provenance{}, false
	}
	pobj, err := objs.Get(c.Provenance)
	if err != nil {
		return object.Provenance{}, false
	}
	p, err := pobj.AsProvenance()
	if err != nil {
		return object.Provenance{}, false
	}
	return p, true
}

// --- helpers ---

func treeOf(objs ObjectStore, change multihash.Multihash) (multihash.Multihash, error) {
	if change == nil {
		return nil, nil
	}
	obj, err := objs.Get(change)
	if err != nil {
		return nil, err
	}
	if obj.Type() != object.TypeChange {
		return change, nil
	}
	c, err := obj.AsChange()
	if err != nil {
		return nil, err
	}
	return c.Tree, nil
}

func eqID(a, b multihash.Multihash) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(b)
}

func sameAsBase(x fileEnt, hasX bool, b fileEnt, hasB bool) bool {
	if hasX != hasB {
		return false
	}
	if !hasX {
		return true // absent in both
	}
	return x.ID.Equal(b.ID)
}

func content(objs ObjectStore, fe fileEnt, has bool) ([]byte, error) {
	if !has {
		return nil, nil
	}
	obj, err := objs.Get(fe.ID)
	if err != nil {
		return nil, err
	}
	c, _ := obj.BlobContent()
	return c, nil
}

func putBlob(objs ObjectStore, content []byte, mode uint32) (fileEnt, error) {
	id, err := objs.Put(object.NewBlob(content))
	if err != nil {
		return fileEnt{}, err
	}
	return fileEnt{ID: id, Mode: mode}, nil
}

func pickMode(o fileEnt, hasO bool, tv fileEnt, hasT bool) uint32 {
	if hasO {
		return o.Mode
	}
	if hasT {
		return tv.Mode
	}
	return modeFile
}

// lines splits content into merge tokens that retain their trailing newline, so
// re-joining is exact. An empty file yields no lines.
func lines(s []byte) []string {
	if len(s) == 0 {
		return nil
	}
	return strings.SplitAfter(string(s), "\n")
}

func unionPaths(maps ...map[string]fileEnt) []string {
	set := map[string]bool{}
	for _, m := range maps {
		for p := range m {
			set[p] = true
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	// deterministic order
	sortStrings(out)
	return out
}
