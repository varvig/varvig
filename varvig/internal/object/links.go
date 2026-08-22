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
		ids := make([]multihash.Multihash, 0, len(c.Parents)+len(c.Artifacts)+2)
		if c.Tree != nil {
			ids = append(ids, c.Tree)
		}
		ids = append(ids, c.Parents...)
		if c.Provenance != nil {
			ids = append(ids, c.Provenance)
		}
		// A reachable change makes every artifact-ref it names reachable, which
		// pins the external bytes (federation §1.3).
		ids = append(ids, c.Artifacts...)
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
	case TypeAttestation:
		a, err := o.AsAttestation()
		if err != nil {
			return nil, err
		}
		var ids []multihash.Multihash
		if a.Target != nil {
			ids = append(ids, a.Target)
		}
		if a.Policy != nil {
			ids = append(ids, a.Policy)
		}
		return ids, nil
	case TypePrincipal:
		return nil, nil
	case TypeArtifactRef:
		// The content bytes are external and never enter the store, so they are
		// not a link. The producing change/attempt is a real object, so it is:
		// while the artifact-ref is reachable its producer is retained.
		a, err := o.AsArtifactRef()
		if err != nil {
			return nil, err
		}
		if a.ProducedBy != nil {
			return []multihash.Multihash{a.ProducedBy}, nil
		}
		return nil, nil
	default:
		return nil, nil
	}
}
