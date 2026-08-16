// Package readapi is the one query layer behind every read of a Varvig
// repository (auth design §7.1). There must be exactly one implementation of
// "read a tree": two code paths drift, and the UI starts showing something the
// CLI does not. Both transports — HTTP/JSON over a Unix socket and the CLI
// plumbing commands — are thin wrappers over the methods here.
//
// Nothing in this package reads the on-disk layout directly (§7.2). All access
// goes through the object store and ref store, so the layout stays the
// disposable cache the storage design promises (design §4.3).
//
// Every view names an immutable, hash-addressed state (§7.3): a change or tree
// hash identifies exactly one snapshot, so results are permalinks and
// infinitely cacheable. Branch names are resolved to a hash first.
package readapi

import (
	"errors"
	"fmt"
	"sort"

	"github.com/dividebyzero/claude-experiments/varvig/internal/affected"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/refs"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/spec"
	"github.com/dividebyzero/claude-experiments/varvig/internal/store"
)

// Query answers read requests against a repository. It is safe for concurrent
// use: it only reads.
type Query struct{ r *repo.Repo }

// New returns a query layer over r.
func New(r *repo.Repo) *Query { return &Query{r: r} }

// ErrNotFound is returned when a requested object or path does not exist.
var ErrNotFound = errors.New("readapi: not found")

// ObjectInfo is the metadata view for /o/{hash}.
type ObjectInfo struct {
	Hash  string   `json:"hash"`
	Type  string   `json:"type"`
	Size  int      `json:"size"` // canonical encoded byte length
	Links []string `json:"links,omitempty"`
}

// Object returns metadata for any object by identity.
func (q *Query) Object(id multihash.Multihash) (ObjectInfo, error) {
	raw, err := q.r.Objects.GetRaw(id)
	if err != nil {
		return ObjectInfo{}, wrap(err)
	}
	o, err := object.Decode(raw)
	if err != nil {
		return ObjectInfo{}, err
	}
	links, err := o.Links()
	if err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{Hash: id.Hex(), Type: o.Type().String(), Size: len(raw), Links: hexes(links)}, nil
}

// TreeEntryView is one child in a directory listing.
type TreeEntryView struct {
	Name string `json:"name"`
	Mode uint32 `json:"mode"`
	Kind string `json:"kind"` // "tree" or "blob"
	Hash string `json:"hash"`
}

// TreeListing is the view for /tree/{hash}/{path…}.
type TreeListing struct {
	Root    string          `json:"root"` // the resolved tree hash at path ""
	Path    string          `json:"path"`
	Hash    string          `json:"hash"` // the tree hash at this path
	Entries []TreeEntryView `json:"entries"`
}

// Tree lists a directory. id may name a change (its tree is used) or a tree.
// path descends into subtrees, slash-separated; "" is the root.
func (q *Query) Tree(id multihash.Multihash, path string) (TreeListing, error) {
	rootTree, err := q.treeOf(id)
	if err != nil {
		return TreeListing{}, err
	}
	cur := rootTree
	for _, comp := range splitPath(path) {
		entries, err := q.treeEntries(cur)
		if err != nil {
			return TreeListing{}, err
		}
		next, ok := findEntry(entries, comp)
		if !ok || next.Kind != object.TypeTree {
			return TreeListing{}, fmt.Errorf("%w: %q in tree", ErrNotFound, comp)
		}
		cur = next.ID
	}
	entries, err := q.treeEntries(cur)
	if err != nil {
		return TreeListing{}, err
	}
	views := make([]TreeEntryView, 0, len(entries))
	for _, e := range entries {
		views = append(views, TreeEntryView{Name: e.Name, Mode: e.Mode, Kind: e.Kind.String(), Hash: e.ID.Hex()})
	}
	return TreeListing{Root: rootTree.Hex(), Path: path, Hash: cur.Hex(), Entries: views}, nil
}

// Blob returns raw file content by identity. The caller decides content type.
func (q *Query) Blob(id multihash.Multihash) ([]byte, error) {
	o, err := q.r.Objects.Get(id)
	if err != nil {
		return nil, wrap(err)
	}
	content, ok := o.BlobContent()
	if !ok {
		return nil, fmt.Errorf("%w: %s is not a blob", ErrNotFound, id.Hex())
	}
	return content, nil
}

// ProvenanceView is the audit evidence attached to a change (design §2.1).
type ProvenanceView struct {
	Authority    string   `json:"authority,omitempty"`
	Model        string   `json:"model,omitempty"`
	ModelVersion string   `json:"model_version,omitempty"`
	Sampling     string   `json:"sampling,omitempty"`
	ToolPerms    []string `json:"tool_permissions,omitempty"`
	ToolHash     string   `json:"tool_hash,omitempty"`
	TaskSpec     string   `json:"task_spec,omitempty"`
	ContextRead  string   `json:"context_read,omitempty"`
	Reasoning    string   `json:"reasoning,omitempty"`
}

// ChangeView is the view for /change/{hash}. It leads with intent, then
// evidence, then the diff — deliberately, not cosmetically (auth design §7.3):
// a diff-first view quietly rebuilds the diff-centric forge it replaces and the
// premise of the system leaks away.
type ChangeView struct {
	Hash       string          `json:"hash"`
	Intent     string          `json:"intent"` // the change message: what this change is for
	Evidence   *ProvenanceView `json:"evidence,omitempty"`
	Author     string          `json:"author,omitempty"`
	Timestamp  int64           `json:"timestamp,omitempty"`
	Tree       string          `json:"tree"`
	Parents    []string        `json:"parents,omitempty"`
	Signed     bool            `json:"signed"`
	ChangedAdd []string        `json:"changed_added,omitempty"`
	ChangedMod []string        `json:"changed_modified,omitempty"`
	ChangedDel []string        `json:"changed_removed,omitempty"`
}

// Change returns the intent-first view of a change object.
func (q *Query) Change(id multihash.Multihash) (ChangeView, error) {
	o, err := q.r.Objects.Get(id)
	if err != nil {
		return ChangeView{}, wrap(err)
	}
	c, err := o.AsChange()
	if err != nil {
		return ChangeView{}, err
	}
	_, signed := o.RawSignature()
	view := ChangeView{
		Hash:      id.Hex(),
		Intent:    c.Message,
		Author:    c.Author,
		Timestamp: c.Timestamp,
		Tree:      c.Tree.Hex(),
		Parents:   hexes(c.Parents),
		Signed:    signed,
	}
	if c.Provenance != nil {
		if pv, err := q.provenance(c.Provenance); err == nil {
			view.Evidence = pv
		}
	}
	// Diff against the first parent. A root change (no parent) has no base
	// tree, so every file it introduces is an addition.
	if len(c.Parents) > 0 {
		if baseTree, err := q.treeOf(c.Parents[0]); err == nil {
			if d, err := affected.DiffTrees(q.r.Objects, baseTree, c.Tree); err == nil {
				view.ChangedAdd, view.ChangedMod, view.ChangedDel = d.Added, d.Modified, d.Removed
			}
		}
	} else if files, err := affected.FlattenTree(q.r.Objects, c.Tree); err == nil {
		for path := range files {
			view.ChangedAdd = append(view.ChangedAdd, path)
		}
		sortStrings(view.ChangedAdd)
	}
	return view, nil
}

// LogEntryView is one row of /log.
type LogEntryView struct {
	Hash      string   `json:"hash"`
	Intent    string   `json:"intent"`
	Author    string   `json:"author,omitempty"`
	Timestamp int64    `json:"timestamp,omitempty"`
	Parents   []string `json:"parents,omitempty"`
}

// Log walks the change DAG from start (a ref or hash), breadth-first over
// parents, returning at most limit entries (0 means no limit).
func (q *Query) Log(start multihash.Multihash, limit int) ([]LogEntryView, error) {
	var out []LogEntryView
	seen := map[string]bool{}
	queue := []multihash.Multihash{start}
	for len(queue) > 0 {
		if limit > 0 && len(out) >= limit {
			break
		}
		id := queue[0]
		queue = queue[1:]
		key := id.Hex()
		if seen[key] {
			continue
		}
		seen[key] = true
		o, err := q.r.Objects.Get(id)
		if err != nil {
			return nil, wrap(err)
		}
		c, err := o.AsChange()
		if err != nil {
			return nil, err
		}
		out = append(out, LogEntryView{
			Hash: key, Intent: c.Message, Author: c.Author,
			Timestamp: c.Timestamp, Parents: hexes(c.Parents),
		})
		queue = append(queue, c.Parents...)
	}
	return out, nil
}

// RefInfo maps a ref name to the hash it points at.
type RefInfo struct {
	Name string `json:"name"`
	Hash string `json:"hash"`
}

// Refs lists every ref and the hash it resolves to.
func (q *Query) Refs() ([]RefInfo, error) {
	names, err := q.r.Refs.List()
	if err != nil {
		return nil, err
	}
	out := make([]RefInfo, 0, len(names))
	for _, n := range names {
		id, err := q.r.Refs.Resolve(n)
		if err != nil {
			continue
		}
		out = append(out, RefInfo{Name: n, Hash: id.Hex()})
	}
	return out, nil
}

// Proposal is one unpromoted speculative state (auth design §7.3).
type Proposal struct {
	Task    string  `json:"task"`
	Change  string  `json:"change"`
	Score   float64 `json:"score"`
	Scored  bool    `json:"scored"`
	Created int64   `json:"created"`
}

// Proposals lists unpromoted speculation candidates. A non-empty scope filters
// to a single task (the speculation pool is keyed by task).
func (q *Query) Proposals(scope string) ([]Proposal, error) {
	pool := spec.Open(q.r.GitDir())
	tasks, err := pool.Tasks()
	if err != nil {
		return nil, err
	}
	var out []Proposal
	for _, task := range tasks {
		if scope != "" && task != scope {
			continue
		}
		entries, err := pool.List(task)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			out = append(out, Proposal{
				Task: task, Change: e.Change.Hex(),
				Score: e.Score, Scored: e.Scored, Created: e.Created,
			})
		}
	}
	return out, nil
}

// Resolve turns a ref name or hex hash into an identity, so a hash-addressed
// route can redirect a branch name to its current hash (§7.3).
func (q *Query) Resolve(refOrHash string) (multihash.Multihash, error) {
	if id, err := q.r.Refs.Resolve(refOrHash); err == nil {
		return id, nil
	}
	if id, err := q.r.Refs.Resolve("refs/heads/" + refOrHash); err == nil {
		return id, nil
	}
	id, err := multihash.ParseHex(refOrHash)
	if err != nil {
		return nil, fmt.Errorf("%w: %q is neither a ref nor a hash", ErrNotFound, refOrHash)
	}
	return id, nil
}

// --- internal helpers ---

func (q *Query) treeOf(id multihash.Multihash) (multihash.Multihash, error) {
	o, err := q.r.Objects.Get(id)
	if err != nil {
		return nil, wrap(err)
	}
	switch o.Type() {
	case object.TypeTree:
		return id, nil
	case object.TypeChange:
		c, err := o.AsChange()
		if err != nil {
			return nil, err
		}
		return c.Tree, nil
	default:
		return nil, fmt.Errorf("%w: %s is a %s, not a tree or change", ErrNotFound, id.Hex(), o.Type())
	}
}

func (q *Query) treeEntries(id multihash.Multihash) ([]object.Entry, error) {
	o, err := q.r.Objects.Get(id)
	if err != nil {
		return nil, wrap(err)
	}
	return o.TreeEntries()
}

func (q *Query) provenance(id multihash.Multihash) (*ProvenanceView, error) {
	o, err := q.r.Objects.Get(id)
	if err != nil {
		return nil, err
	}
	p, err := o.AsProvenance()
	if err != nil {
		return nil, err
	}
	pv := &ProvenanceView{
		Authority: p.Authority, Model: p.Model, ModelVersion: p.ModelVersion,
		Sampling: p.Sampling, ToolPerms: p.ToolPermissions, TaskSpec: p.TaskSpec,
		ContextRead: p.ContextRead, Reasoning: p.Reasoning,
	}
	if p.ToolHash != nil {
		pv.ToolHash = p.ToolHash.Hex()
	}
	return pv, nil
}

func findEntry(entries []object.Entry, name string) (object.Entry, bool) {
	for _, e := range entries {
		if e.Name == name {
			return e, true
		}
	}
	return object.Entry{}, false
}

func splitPath(p string) []string {
	var out []string
	cur := ""
	for _, r := range p {
		if r == '/' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func hexes(ids []multihash.Multihash) []string {
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.Hex()
	}
	return out
}

// wrap maps a store/ref "not found" to the package's ErrNotFound so the HTTP
// layer can return a 404 without depending on lower-level error types.
func wrap(err error) error {
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, refs.ErrNotExist) {
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	return err
}

func sortStrings(s []string) { sort.Strings(s) }
