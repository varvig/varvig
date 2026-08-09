package object

import (
	"bytes"
	"errors"
	"fmt"
	"sort"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
)

// ErrUnmaterialized reports that a change carries intent but no tree: it has
// not been materialized (tickets §1.1, decision D1). This is a legitimate,
// distinct state — the object model of an intent whose code does not yet
// exist — and it is deliberately *not* the same as a change materialized to
// the empty tree (a real, meaningful repository state with no files). The two
// encode to different bytes and hash differently; the distinction is preserved
// so it can never be silently conflated after first run.
var ErrUnmaterialized = errors.New("object: change is unmaterialized (no tree)")

// Change is a history node in the DAG: a snapshot (a tree) plus its parents
// and minimal metadata. It is the analogue of a Git commit, but deliberately
// thin for step 1 — intent, provenance, and signing are layered on later as
// new field tags (design §1.1, §2.1), which older builds preserve untouched.
//
// Tree is nullable. A change with no tree is *unmaterialized*: intent without
// a materialization, the object underlying a ticket (tickets §1.1). The tree
// field is therefore encoded as an explicit absence — the tag is omitted
// entirely — rather than as the empty-tree hash, so that "not materialized"
// and "materialized to an empty tree" remain two distinct, non-conflatable
// states (decision D1). Use Materialized to tell them apart.
type Change struct {
	Tree      multihash.Multihash
	Parents   []multihash.Multihash
	Message   string
	Timestamp int64
	Author    string
	// Provenance is the id of a TypeProvenance object describing who or what
	// produced this change (design §1.1, §2.1). Optional at the codec level so
	// that git-imported and legacy changes remain representable; the commit and
	// verify layers require it for native changes.
	Provenance multihash.Multihash
}

// Materialized reports whether the change carries a tree. A change with no
// tree is an unmaterialized intent (a ticket, tickets §1.1); this is a normal
// state, not an error, and is distinct from a change materialized to the empty
// tree, for which Materialized reports true.
func (c Change) Materialized() bool { return c.Tree != nil }

// The parents list is serialized into one field value:
//
//	count    uvarint
//	parents  count records, sorted by id bytes ascending, unique:
//	            idLen uvarint
//	            id    idLen bytes

// NewChange builds a change object. Parents are sorted by identity and
// de-duplicated so the encoding is canonical regardless of input order.
func NewChange(c Change) *Object {
	parents := append([]multihash.Multihash(nil), c.Parents...)
	sort.Slice(parents, func(a, b int) bool { return bytes.Compare(parents[a], parents[b]) < 0 })
	// de-dup identical parents
	uniq := parents[:0]
	for i, p := range parents {
		if i > 0 && bytes.Equal(p, parents[i-1]) {
			continue
		}
		uniq = append(uniq, p)
	}
	parents = uniq

	var pv []byte
	pv = appendUvarint(pv, uint64(len(parents)))
	for _, p := range parents {
		pv = appendUvarint(pv, uint64(len(p)))
		pv = append(pv, p...)
	}

	fields := []field{
		{tag: tagChangeParents, val: pv},
		{tag: tagChangeMessage, val: []byte(c.Message)},
		{tag: tagChangeTimestamp, val: appendUvarint(nil, uint64(c.Timestamp))},
		{tag: tagChangeAuthor, val: []byte(c.Author)},
	}
	// The tree is nullable (decision D1): emit the field only when the change
	// is materialized. An unmaterialized change (a ticket) omits the tag
	// entirely, encoding absence explicitly rather than as an empty-valued
	// field or the empty-tree hash.
	if c.Tree != nil {
		fields = append(fields, field{tag: tagChangeTree, val: append([]byte(nil), c.Tree...)})
	}
	// Provenance is optional at the codec level: emit the field only when set,
	// so changes without it (git-imported, legacy) encode exactly as before.
	if c.Provenance != nil {
		fields = append(fields, field{tag: tagChangeProvenance, val: append([]byte(nil), c.Provenance...)})
	}
	return newObject(TypeChange, fields)
}

// AsChange decodes the typed view of a change object.
func (o *Object) AsChange() (Change, error) {
	if o.typ != TypeChange {
		return Change{}, fmt.Errorf("object: not a change (%s)", o.typ)
	}
	var c Change
	if v, ok := o.Field(tagChangeTree); ok {
		// Presence of the tag means materialized (decision D1). Copy into a
		// guaranteed non-nil slice so Materialized reflects presence, not the
		// value's length — absence of the tag is the only unmaterialized state.
		tree := make([]byte, len(v))
		copy(tree, v)
		c.Tree = multihash.Multihash(tree)
	}
	if v, ok := o.Field(tagChangeMessage); ok {
		c.Message = string(v)
	}
	if v, ok := o.Field(tagChangeAuthor); ok {
		c.Author = string(v)
	}
	if v, ok := o.Field(tagChangeProvenance); ok {
		c.Provenance = multihash.Multihash(append([]byte(nil), v...))
	}
	if v, ok := o.Field(tagChangeTimestamp); ok {
		ts, n, err := readUvarint(v)
		if err != nil || n != len(v) {
			return Change{}, fmt.Errorf("%w: bad change timestamp", ErrMalformed)
		}
		c.Timestamp = int64(ts)
	}
	if v, ok := o.Field(tagChangeParents); ok {
		cur := &cursor{b: v}
		n, err := cur.uvarint()
		if err != nil {
			return Change{}, err
		}
		var prev []byte
		for i := uint64(0); i < n; i++ {
			idLen, err := cur.uvarint()
			if err != nil {
				return Change{}, err
			}
			id, err := cur.take(idLen)
			if err != nil {
				return Change{}, err
			}
			if i > 0 && bytes.Compare(id, prev) <= 0 {
				return Change{}, fmt.Errorf("%w: change parents not sorted/unique", ErrMalformed)
			}
			c.Parents = append(c.Parents, multihash.Multihash(append([]byte(nil), id...)))
			prev = id
		}
		if !cur.empty() {
			return Change{}, fmt.Errorf("%w: trailing bytes in change parents", ErrMalformed)
		}
	}
	return c, nil
}
