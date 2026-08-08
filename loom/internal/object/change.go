package object

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/dividebyzero/claude-experiments/loom/internal/multihash"
)

// Change is a history node in the DAG: a snapshot (a tree) plus its parents
// and minimal metadata. It is the analogue of a Git commit, but deliberately
// thin for step 1 — intent, provenance, and signing are layered on later as
// new field tags (design §1.1, §2.1), which older builds preserve untouched.
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
		{tag: tagChangeTree, val: append([]byte(nil), c.Tree...)},
		{tag: tagChangeParents, val: pv},
		{tag: tagChangeMessage, val: []byte(c.Message)},
		{tag: tagChangeTimestamp, val: appendUvarint(nil, uint64(c.Timestamp))},
		{tag: tagChangeAuthor, val: []byte(c.Author)},
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
		c.Tree = multihash.Multihash(append([]byte(nil), v...))
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
