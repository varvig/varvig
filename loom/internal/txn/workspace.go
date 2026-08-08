package txn

import (
	"errors"
	"fmt"
	"sort"

	"github.com/dividebyzero/claude-experiments/loom/internal/multihash"
	"github.com/dividebyzero/claude-experiments/loom/internal/object"
)

// ErrOutOfScope is returned when a transaction reads or writes outside its
// declared sets — a capability-boundary violation (design §1.4).
var ErrOutOfScope = errors.New("txn: access outside declared scope")

// ErrNotExist is returned by Read for a path not present in the base tree.
var ErrNotExist = errors.New("txn: file does not exist")

// Workspace is the capability-scoped view a transaction operates on. Reads are
// permitted within the read+write sets; writes and removes only within the
// write set. Mutations are buffered and applied to the base tree at commit.
type Workspace struct {
	objs    ObjectStore
	base    map[string]fileEnt
	reads   []string
	writes  []string
	pending map[string]*pendingWrite
}

type pendingWrite struct {
	content []byte
	mode    uint32
	del     bool
}

func newWorkspace(objs ObjectStore, base map[string]fileEnt, reads, writes []string) *Workspace {
	return &Workspace{
		objs:    objs,
		base:    base,
		reads:   reads,
		writes:  writes,
		pending: map[string]*pendingWrite{},
	}
}

func inScope(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if pathCovers(p, path) {
			return true
		}
	}
	return false
}

// readable is the union of the read and write sets: writing a file implies
// being able to read it for a read-modify-write.
func (ws *Workspace) readable(path string) bool {
	return inScope(path, ws.reads) || inScope(path, ws.writes)
}

// Read returns a file's current content (including this transaction's own
// pending writes). It fails if the path is outside the readable scope.
func (ws *Workspace) Read(path string) ([]byte, error) {
	if !ws.readable(path) {
		return nil, fmt.Errorf("%w: read %q", ErrOutOfScope, path)
	}
	if w, ok := ws.pending[path]; ok {
		if w.del {
			return nil, ErrNotExist
		}
		return append([]byte(nil), w.content...), nil
	}
	fe, ok := ws.base[path]
	if !ok {
		return nil, ErrNotExist
	}
	obj, err := ws.objs.Get(fe.ID)
	if err != nil {
		return nil, err
	}
	content, _ := obj.BlobContent()
	return content, nil
}

// Exists reports whether a readable path currently exists.
func (ws *Workspace) Exists(path string) bool {
	_, err := ws.Read(path)
	return err == nil
}

// Write buffers new content for a path, which must be within the write set.
func (ws *Workspace) Write(path string, content []byte) error {
	if !inScope(path, ws.writes) {
		return fmt.Errorf("%w: write %q", ErrOutOfScope, path)
	}
	ws.pending[path] = &pendingWrite{content: append([]byte(nil), content...), mode: modeFile}
	return nil
}

// Remove buffers deletion of a path, which must be within the write set.
func (ws *Workspace) Remove(path string) error {
	if !inScope(path, ws.writes) {
		return fmt.Errorf("%w: remove %q", ErrOutOfScope, path)
	}
	ws.pending[path] = &pendingWrite{del: true}
	return nil
}

// List returns the readable paths currently present, sorted.
func (ws *Workspace) List() []string {
	set := map[string]bool{}
	for p := range ws.base {
		if ws.readable(p) {
			set[p] = true
		}
	}
	for p, w := range ws.pending {
		if !ws.readable(p) {
			continue
		}
		if w.del {
			delete(set, p)
		} else {
			set[p] = true
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// finalize applies the buffered mutations to the base tree and returns the new
// tree id. Written content is stored as blobs here.
func (ws *Workspace) finalize() (multihash.Multihash, error) {
	files := make(map[string]fileEnt, len(ws.base))
	for p, fe := range ws.base {
		files[p] = fe
	}
	for p, w := range ws.pending {
		if w.del {
			delete(files, p)
			continue
		}
		id, err := ws.objs.Put(object.NewBlob(w.content))
		if err != nil {
			return nil, err
		}
		files[p] = fileEnt{ID: id, Mode: w.mode}
	}
	tree, err := buildTree(ws.objs, files)
	if err != nil {
		return nil, err
	}
	return tree, nil
}

// mutated reports whether the transaction buffered any change.
func (ws *Workspace) mutated() bool { return len(ws.pending) > 0 }
