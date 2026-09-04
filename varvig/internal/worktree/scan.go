package worktree

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/store"
)

// FileState is a working-tree or tree path reduced to what a diff needs: the
// content hash and the git-vocabulary mode. It carries no content — a diff reads
// that lazily, only for the paths that changed.
type FileState struct {
	Hash multihash.Multihash
	Mode uint32
}

// --- the staleness index (build spec P0.1) -------------------------------------
//
// varvig's view of a file goes stale the moment it is edited. The index is a pure
// *cache* of (path → stat tuple → content hash): a path whose stat tuple still
// matches is clean and its cached hash is reused; anything else is rehashed. It
// is never authoritative — delete it and every result is byte-identical, only
// slower. The one subtlety is the racy-clean window (git's term): an edit landing
// in the same clock granularity as the last index write cannot be told apart by
// stat alone, so any entry whose mtime is at or after the index's own write time
// is rehashed regardless.

type indexEntry struct {
	Size    int64  `json:"size"`
	MtimeNs int64  `json:"mtime_ns"`
	Inode   uint64 `json:"inode"`
	Mode    uint32 `json:"mode"`
	Hash    string `json:"hash"`
}

// Index is the working-tree staleness cache, persisted as one JSON file.
type Index struct {
	path      string
	WrittenNs int64                 `json:"written_ns"`
	Entries   map[string]indexEntry `json:"entries"`
}

// indexFileName is where the cache lives under the metadata dir.
const indexFileName = "worktree-index.json"

// OpenIndex loads the index under gitDir, or an empty one if none exists yet. A
// corrupt or unreadable index is treated as empty — it is only a cache.
func OpenIndex(gitDir string) *Index {
	idx := &Index{path: filepath.Join(gitDir, indexFileName), Entries: map[string]indexEntry{}}
	b, err := os.ReadFile(idx.path)
	if err != nil {
		return idx
	}
	var loaded Index
	if json.Unmarshal(b, &loaded) == nil && loaded.Entries != nil {
		idx.WrittenNs = loaded.WrittenNs
		idx.Entries = loaded.Entries
	}
	return idx
}

// Save persists the index, stamping the write time so the next scan can detect
// racy-clean entries. Best-effort: a failure to persist only forfeits the cache.
func (idx *Index) Save() error {
	if idx.path == "" {
		return nil
	}
	idx.WrittenNs = time.Now().UnixNano()
	b, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(idx.path, b, 0o600)
}

// clean reports whether the cached entry for a path can be trusted for the given
// stat, i.e. the tuple matches and the entry is not in the racy window.
func (idx *Index) clean(rel string, size, mtimeNs int64, inode uint64, mode uint32) (multihash.Multihash, bool) {
	e, ok := idx.Entries[rel]
	if !ok || e.Size != size || e.MtimeNs != mtimeNs || e.Inode != inode || e.Mode != mode {
		return nil, false
	}
	// Racy-clean: an mtime in the same second as (or after) the last index write
	// cannot be trusted by stat alone — rehash.
	if idx.WrittenNs != 0 && mtimeNs/1e9 >= idx.WrittenNs/1e9 {
		return nil, false
	}
	h, err := multihash.ParseHex(e.Hash)
	if err != nil {
		return nil, false
	}
	return h, true
}

func (idx *Index) put(rel string, size, mtimeNs int64, inode uint64, mode uint32, h multihash.Multihash) {
	idx.Entries[rel] = indexEntry{Size: size, MtimeNs: mtimeNs, Inode: inode, Mode: mode, Hash: h.Hex()}
}

// Scan hashes the working tree at dir into a path→state map, reusing the index
// for unchanged paths and rehashing (and storing the blob) for the rest, then
// pruning index entries for paths that no longer exist. The caller Saves the
// index. It is a read of the tree; the only writes are content-addressed blobs.
func Scan(s *store.Store, dir string, idx *Index) (map[string]FileState, error) {
	out := map[string]FileState{}
	if err := scanDir(s, dir, "", idx, out); err != nil {
		return nil, err
	}
	// Prune stale cache entries so the index does not grow without bound.
	for rel := range idx.Entries {
		if _, ok := out[rel]; !ok {
			delete(idx.Entries, rel)
		}
	}
	return out, nil
}

func scanDir(s *store.Store, dir, prefix string, idx *Index, out map[string]FileState) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, de := range ents {
		name := de.Name()
		if name == skipName || name == ".git" {
			continue
		}
		full := filepath.Join(dir, name)
		rel := name
		if prefix != "" {
			rel = prefix + "/" + name
		}
		info, err := de.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(full)
			if err != nil {
				return err
			}
			id, err := s.Put(object.NewBlob([]byte(target)))
			if err != nil {
				return err
			}
			out[rel] = FileState{Hash: id, Mode: modeSymlink}
		case de.IsDir():
			if err := scanDir(s, full, rel, idx, out); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			mode := uint32(modeFile)
			if info.Mode()&0o111 != 0 {
				mode = modeExec
			}
			size, mtimeNs, inode := statTuple(info)
			if h, ok := idx.clean(rel, size, mtimeNs, inode, mode); ok {
				out[rel] = FileState{Hash: h, Mode: mode}
				continue
			}
			content, err := os.ReadFile(full)
			if err != nil {
				return err
			}
			id, err := s.Put(object.NewBlob(content))
			if err != nil {
				return err
			}
			idx.put(rel, size, mtimeNs, inode, mode, id)
			out[rel] = FileState{Hash: id, Mode: mode}
		default:
			// Skip unsupported node types (sockets, devices) rather than fail a read.
		}
	}
	return nil
}

func statTuple(info os.FileInfo) (size, mtimeNs int64, inode uint64) {
	size = info.Size()
	mtimeNs = info.ModTime().UnixNano()
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		inode = st.Ino
	}
	return size, mtimeNs, inode
}

// FlattenStates walks a stored tree into a path→state map (hash + mode), the
// base side of a working-tree diff. A nil tree is the empty set.
func FlattenStates(s *store.Store, treeID multihash.Multihash) (map[string]FileState, error) {
	out := map[string]FileState{}
	if treeID == nil {
		return out, nil
	}
	if err := flattenStates(s, treeID, "", out); err != nil {
		return nil, err
	}
	return out, nil
}

func flattenStates(s *store.Store, treeID multihash.Multihash, prefix string, out map[string]FileState) error {
	obj, err := s.Get(treeID)
	if err != nil {
		return err
	}
	entries, err := obj.TreeEntries()
	if err != nil {
		return err
	}
	for _, e := range entries {
		rel := e.Name
		if prefix != "" {
			rel = prefix + "/" + e.Name
		}
		if e.Kind == object.TypeTree {
			if err := flattenStates(s, e.ID, rel, out); err != nil {
				return err
			}
			continue
		}
		out[rel] = FileState{Hash: e.ID, Mode: e.Mode}
	}
	return nil
}

// Edit is one path a proposal will write or delete. Renames are split into a
// delete of the old path and a write of the new.
type Edit struct {
	Path string
	Del  bool
}

// SelectEdits turns a working-tree diff into the edits to propose, applying the
// build spec's §A2 reconciliation, shared by the CLI and the gate: a change
// outside what `covers` allows is a refusal (not a truncated proposal); explicit
// `paths` narrow the observed set but can never name a path that was not
// observed; an empty result is a distinct error, never a silent empty success.
// scopeLabel is used only in messages.
func SelectEdits(d TreeDiff, covers func(string) bool, scopeLabel string, paths []string) ([]Edit, error) {
	var edits []Edit
	add := func(ps []string, del bool) {
		for _, p := range ps {
			edits = append(edits, Edit{Path: p, Del: del})
		}
	}
	add(d.Added, false)
	add(d.Modified, false)
	add(d.ModeChanged, false)
	add(d.Removed, true)
	for _, rn := range d.Renamed {
		edits = append(edits, Edit{Path: rn.From, Del: true}, Edit{Path: rn.To})
	}

	var outside []string
	for _, e := range edits {
		if !covers(e.Path) {
			outside = append(outside, e.Path)
		}
	}
	if len(outside) > 0 {
		sort.Strings(outside)
		return nil, fmt.Errorf("%d path(s) changed outside the declared scope %s: %s",
			len(outside), scopeLabel, strings.Join(outside, ", "))
	}

	if len(paths) > 0 {
		observed := map[string]bool{}
		for _, e := range edits {
			observed[e.Path] = true
		}
		want := map[string]bool{}
		for _, p := range paths {
			if !observed[p] {
				return nil, fmt.Errorf("%q was named but is not among the changed paths", p)
			}
			want[p] = true
		}
		kept := edits[:0]
		for _, e := range edits {
			if want[e.Path] {
				kept = append(kept, e)
			}
		}
		edits = kept
	}

	if len(edits) == 0 {
		return nil, fmt.Errorf("nothing to propose — the working tree matches the base within scope %s", scopeLabel)
	}
	return edits, nil
}

// Overlay applies edits onto a copy of base, taking the new state for a written
// path from work — the proposed path→state map to hand to BuildTree.
func Overlay(base, work map[string]FileState, edits []Edit) map[string]FileState {
	out := make(map[string]FileState, len(base))
	for p, s := range base {
		out[p] = s
	}
	for _, e := range edits {
		if e.Del {
			delete(out, e.Path)
		} else {
			out[e.Path] = work[e.Path]
		}
	}
	return out
}

// BuildTree writes a flat path→state map back into nested tree objects and
// returns the root tree id — the inverse of FlattenStates. It is how a proposal
// materializes a base tree overlaid with a selected set of working changes.
func BuildTree(s *store.Store, files map[string]FileState) (multihash.Multihash, error) {
	blobs := map[string]FileState{}
	subdirs := map[string]map[string]FileState{}
	for p, fs := range files {
		if i := strings.IndexByte(p, '/'); i >= 0 {
			seg, rest := p[:i], p[i+1:]
			if subdirs[seg] == nil {
				subdirs[seg] = map[string]FileState{}
			}
			subdirs[seg][rest] = fs
			continue
		}
		blobs[p] = fs
	}
	var entries []object.Entry
	for name, fs := range blobs {
		entries = append(entries, object.Entry{Name: name, Mode: fs.Mode, Kind: object.TypeBlob, ID: fs.Hash})
	}
	for seg, sub := range subdirs {
		subID, err := BuildTree(s, sub)
		if err != nil {
			return nil, err
		}
		entries = append(entries, object.Entry{Name: seg, Mode: modeTree, Kind: object.TypeTree, ID: subID})
	}
	return s.Put(object.NewTree(entries))
}

// --- comparison ----------------------------------------------------------------

// Rename is a path whose content moved unchanged from From to To.
type Rename struct{ From, To string }

// TreeDiff is a working-tree/tree comparison, split by kind. Renames are detected
// by content hash and removed from Added/Removed.
type TreeDiff struct {
	Added       []string
	Modified    []string
	Removed     []string
	ModeChanged []string
	Renamed     []Rename
}

// Empty reports whether nothing changed.
func (d TreeDiff) Empty() bool {
	return len(d.Added)+len(d.Modified)+len(d.Removed)+len(d.ModeChanged)+len(d.Renamed) == 0
}

// Compare diffs a base state map against a working (new) state map.
func Compare(base, work map[string]FileState) TreeDiff {
	var d TreeDiff
	var added, removed []string
	for p, ws := range work {
		bs, ok := base[p]
		if !ok {
			added = append(added, p)
			continue
		}
		switch {
		case !bs.Hash.Equal(ws.Hash):
			d.Modified = append(d.Modified, p)
		case bs.Mode != ws.Mode:
			d.ModeChanged = append(d.ModeChanged, p)
		}
	}
	for p := range base {
		if _, ok := work[p]; !ok {
			removed = append(removed, p)
		}
	}
	// Rename detection: a removed path whose content hash reappears at an added
	// path is a rename, not a delete+add.
	byHash := map[string]string{} // hash → added path
	for _, a := range added {
		byHash[work[a].Hash.Hex()] = a
	}
	usedAdd := map[string]bool{}
	for _, r := range removed {
		if to, ok := byHash[base[r].Hash.Hex()]; ok && !usedAdd[to] {
			d.Renamed = append(d.Renamed, Rename{From: r, To: to})
			usedAdd[to] = true
			continue
		}
		d.Removed = append(d.Removed, r)
	}
	for _, a := range added {
		if !usedAdd[a] {
			d.Added = append(d.Added, a)
		}
	}
	sort.Strings(d.Added)
	sort.Strings(d.Modified)
	sort.Strings(d.Removed)
	sort.Strings(d.ModeChanged)
	sort.Slice(d.Renamed, func(i, j int) bool { return d.Renamed[i].To < d.Renamed[j].To })
	return d
}
