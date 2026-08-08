package txn

import (
	"strings"

	"github.com/dividebyzero/claude-experiments/loom/internal/multihash"
	"github.com/dividebyzero/claude-experiments/loom/internal/object"
)

// ObjectStore is the read/write surface transactions need.
type ObjectStore interface {
	Get(multihash.Multihash) (*object.Object, error)
	Put(*object.Object) (multihash.Multihash, error)
}

// fileEnt is a file in a flattened tree: its blob id and mode.
type fileEnt struct {
	ID   multihash.Multihash
	Mode uint32
}

const (
	modeFile = 0o100644
	modeTree = 0o40000
)

// flatten reads a tree into path -> fileEnt (slash paths), or empty if nil.
func flatten(objs ObjectStore, treeID multihash.Multihash) (map[string]fileEnt, error) {
	out := map[string]fileEnt{}
	if treeID == nil {
		return out, nil
	}
	if err := flattenInto(objs, treeID, "", out); err != nil {
		return nil, err
	}
	return out, nil
}

func flattenInto(objs ObjectStore, treeID multihash.Multihash, prefix string, out map[string]fileEnt) error {
	obj, err := objs.Get(treeID)
	if err != nil {
		return err
	}
	entries, err := obj.TreeEntries()
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := e.Name
		if prefix != "" {
			p = prefix + "/" + e.Name
		}
		if e.Kind == object.TypeTree {
			if err := flattenInto(objs, e.ID, p, out); err != nil {
				return err
			}
		} else {
			out[p] = fileEnt{ID: e.ID, Mode: e.Mode}
		}
	}
	return nil
}

// buildTree constructs a tree object from a flat path -> fileEnt map, creating
// nested subtrees bottom-up, and returns the root tree id.
func buildTree(objs ObjectStore, files map[string]fileEnt) (multihash.Multihash, error) {
	blobs := map[string]fileEnt{}
	subdirs := map[string]map[string]fileEnt{}
	for p, fe := range files {
		if i := strings.IndexByte(p, '/'); i >= 0 {
			seg, rest := p[:i], p[i+1:]
			if subdirs[seg] == nil {
				subdirs[seg] = map[string]fileEnt{}
			}
			subdirs[seg][rest] = fe
		} else {
			blobs[p] = fe
		}
	}
	var entries []object.Entry
	for name, fe := range blobs {
		entries = append(entries, object.Entry{Name: name, Mode: fe.Mode, Kind: object.TypeBlob, ID: fe.ID})
	}
	for seg, sub := range subdirs {
		subID, err := buildTree(objs, sub)
		if err != nil {
			return nil, err
		}
		entries = append(entries, object.Entry{Name: seg, Mode: modeTree, Kind: object.TypeTree, ID: subID})
	}
	return objs.Put(object.NewTree(entries))
}
