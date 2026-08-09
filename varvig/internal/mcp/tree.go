package mcp

import (
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/store"
)

// The propose path builds a new tree by overlaying proposed file contents onto a
// base tree. It flattens the base to path→blob, applies the overlay, and rebuilds
// nested trees bottom-up. This mirrors the flatten/build pattern the merge and
// txn packages use internally; it is kept local so the gate stays independent of
// their unexported helpers.

const (
	modeFile = 0o100644
	modeTree = 0o40000
)

type fileEnt struct {
	id   multihash.Multihash
	mode uint32
}

// flattenTree reads treeID into a path→file map. A nil base is an empty tree, so
// a proposal against an empty repository still works.
func flattenTree(s *store.Store, treeID multihash.Multihash) (map[string]fileEnt, error) {
	out := map[string]fileEnt{}
	if treeID == nil {
		return out, nil
	}
	if err := flattenInto(s, treeID, "", out); err != nil {
		return nil, err
	}
	return out, nil
}

func flattenInto(s *store.Store, treeID multihash.Multihash, prefix string, out map[string]fileEnt) error {
	o, err := s.Get(treeID)
	if err != nil {
		return err
	}
	entries, err := o.TreeEntries()
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := e.Name
		if prefix != "" {
			p = prefix + "/" + e.Name
		}
		if e.Kind == object.TypeTree {
			if err := flattenInto(s, e.ID, p, out); err != nil {
				return err
			}
			continue
		}
		out[p] = fileEnt{id: e.ID, mode: e.Mode}
	}
	return nil
}

// buildTree writes a path→file map back into nested tree objects and returns the
// root tree id.
func buildTree(s *store.Store, files map[string]fileEnt) (multihash.Multihash, error) {
	blobs := map[string]fileEnt{}
	subdirs := map[string]map[string]fileEnt{}
	for p, fe := range files {
		if i := strings.IndexByte(p, '/'); i >= 0 {
			seg, rest := p[:i], p[i+1:]
			if subdirs[seg] == nil {
				subdirs[seg] = map[string]fileEnt{}
			}
			subdirs[seg][rest] = fe
			continue
		}
		blobs[p] = fe
	}
	var entries []object.Entry
	for name, fe := range blobs {
		entries = append(entries, object.Entry{Name: name, Mode: fe.mode, Kind: object.TypeBlob, ID: fe.id})
	}
	for seg, sub := range subdirs {
		subID, err := buildTree(s, sub)
		if err != nil {
			return nil, err
		}
		entries = append(entries, object.Entry{Name: seg, Mode: modeTree, Kind: object.TypeTree, ID: subID})
	}
	return s.Put(object.NewTree(entries))
}
