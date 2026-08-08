package object

import "github.com/dividebyzero/claude-experiments/loom/internal/multihash"

// Links returns the identities this object references directly: a blob links to
// nothing, a tree to its entries, a change to its tree and parents. Walking
// Links transitively yields an object's reachable closure — the basis for
// sync, garbage collection, and bisect.
//
// An object of an unknown type reports no links. That is safe for transferring
// a closure a peer already understands, but a future garbage collector must
// treat unknown-type objects conservatively (never assume their closure is
// empty) rather than relying on this.
func (o *Object) Links() ([]multihash.Multihash, error) {
	switch o.typ {
	case TypeBlob:
		return nil, nil
	case TypeTree:
		entries, err := o.TreeEntries()
		if err != nil {
			return nil, err
		}
		ids := make([]multihash.Multihash, 0, len(entries))
		for _, e := range entries {
			ids = append(ids, e.ID)
		}
		return ids, nil
	case TypeChange:
		c, err := o.AsChange()
		if err != nil {
			return nil, err
		}
		ids := make([]multihash.Multihash, 0, len(c.Parents)+1)
		if c.Tree != nil {
			ids = append(ids, c.Tree)
		}
		ids = append(ids, c.Parents...)
		return ids, nil
	default:
		return nil, nil
	}
}
