package object

import "github.com/dividebyzero/claude-experiments/varvig/internal/multihash"

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
		ids := make([]multihash.Multihash, 0, len(c.Parents)+2)
		if c.Tree != nil {
			ids = append(ids, c.Tree)
		}
		ids = append(ids, c.Parents...)
		if c.Provenance != nil {
			ids = append(ids, c.Provenance)
		}
		return ids, nil
	case TypeProvenance:
		return nil, nil
	case TypeNote:
		n, err := o.AsNote()
		if err != nil {
			return nil, err
		}
		var ids []multihash.Multihash
		if n.Target != nil {
			ids = append(ids, n.Target)
		}
		if n.Parent != nil {
			ids = append(ids, n.Parent)
		}
		return ids, nil
	case TypeHookConfig:
		c, err := o.AsHookConfig()
		if err != nil {
			return nil, err
		}
		ids := make([]multihash.Multihash, 0, len(c.Entries))
		for _, e := range c.Entries {
			ids = append(ids, e.Module)
		}
		return ids, nil
	default:
		return nil, nil
	}
}
