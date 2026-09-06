package core

// The read-gate may not decode core objects (design addendum, U1) — it is a
// surface over this package, not a second implementation of it. These are the
// object questions a shell legitimately needs answered, expressed so that asking
// them requires no knowledge of the object format at all.
//
// They exist because the alternative kept happening: a shell that cannot import
// internal/object can still call a method on a value it got back from the store,
// so "the gate holds no object-store vocabulary" was true of its import list and
// false of its code. Every helper here replaced exactly one such call site.

import (
	"fmt"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// ErrNotAChange reports that an id names something other than a change. It is a
// distinct error because a caller that asked for a change usually wants to say
// so precisely rather than fail generically.
var ErrNotAChange = fmt.Errorf("core: not a change")

// ChangeTrees resolves a change to the two trees a comparison needs: its own,
// and its first parent's. A root change yields a nil base tree, which is the
// empty tree — so every path in it reads as an addition.
//
// It refuses an id that is not a change. That refusal is the point: "this change
// against what came before" is meaningless for a bare tree, and silently
// treating a tree as one side of it would compare against nothing while looking
// like it worked.
func ChangeTrees(r *repo.Repo, id multihash.Multihash) (tree, baseTree multihash.Multihash, err error) {
	if id == nil {
		return nil, nil, ErrNotAChange
	}
	o, err := r.Objects.Get(id)
	if err != nil {
		return nil, nil, err
	}
	if !IsChange(o) {
		return nil, nil, ErrNotAChange
	}
	c, err := o.AsChange()
	if err != nil {
		return nil, nil, err
	}
	if len(c.Parents) > 0 {
		if baseTree, err = TreeOf(r, c.Parents[0]); err != nil {
			return nil, nil, err
		}
	}
	return c.Tree, baseTree, nil
}

// BlobBytes reads a blob's content by id. ok is false when the id names
// something that is not a blob, which a caller reading a path's content should
// treat as absent rather than as an error — a tree at that id is a directory,
// not a failure.
func BlobBytes(r *repo.Repo, id multihash.Multihash) (content []byte, ok bool, err error) {
	if id == nil {
		return nil, false, nil
	}
	o, err := r.Objects.Get(id)
	if err != nil {
		return nil, false, err
	}
	c, ok := o.BlobContent()
	return c, ok, nil
}

// ObjectKind names an object's type for display ("blob", "tree", "change", …).
// It is the one piece of object vocabulary a shell genuinely has to render, so
// it is served here rather than by handing the shell an object to inspect.
func ObjectKind(r *repo.Repo, id multihash.Multihash) (string, error) {
	o, err := r.Objects.Get(id)
	if err != nil {
		return "", err
	}
	return o.Type().String(), nil
}

// KindBlob and KindTree are the two kinds a shell compares against — the
// content-bearing objects whose reachability scope must confine (§9.4).
const (
	KindBlob = "blob"
	KindTree = "tree"
)
