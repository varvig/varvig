package object

import (
	"fmt"
	"sort"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
)

// Entry is one child of a tree: a named reference to a blob or subtree. The
// referenced ID is a multihash, so a tree's own identity depends on its
// children's content — this is the Merkle DAG (design §2).
type Entry struct {
	Name string
	Mode uint32              // filesystem mode bits of the entry
	Kind Type                // TypeBlob or TypeTree
	ID   multihash.Multihash // identity of the referenced object
}

// The tree's entry list is serialized into a single field value with its own
// canonical framing:
//
//	count      uvarint
//	entries    count records, sorted by name ascending, names unique:
//	              nameLen uvarint
//	              name    nameLen bytes
//	              mode    uvarint
//	              kind    uvarint
//	              idLen   uvarint
//	              id      idLen bytes

// NewTree builds a tree from entries. Entries are sorted by name; duplicate
// names panic (a programming error).
func NewTree(entries []Entry) *Object {
	es := append([]Entry(nil), entries...)
	sort.Slice(es, func(a, b int) bool { return es[a].Name < es[b].Name })
	for i := 1; i < len(es); i++ {
		if es[i].Name == es[i-1].Name {
			panic(fmt.Sprintf("object: duplicate tree entry name %q", es[i].Name))
		}
	}
	var val []byte
	val = appendUvarint(val, uint64(len(es)))
	for _, e := range es {
		val = appendUvarint(val, uint64(len(e.Name)))
		val = append(val, e.Name...)
		val = appendUvarint(val, uint64(e.Mode))
		val = appendUvarint(val, uint64(e.Kind))
		val = appendUvarint(val, uint64(len(e.ID)))
		val = append(val, e.ID...)
	}
	return newObject(TypeTree, []field{{tag: tagTreeEntries, val: val}})
}

// TreeEntries decodes and validates the entries of a tree object. It refuses
// any non-canonical entry list (unsorted or duplicate names, bad framing).
func (o *Object) TreeEntries() ([]Entry, error) {
	if o.typ != TypeTree {
		return nil, fmt.Errorf("object: not a tree (%s)", o.typ)
	}
	val, ok := o.Field(tagTreeEntries)
	if !ok {
		return nil, fmt.Errorf("%w: tree missing entries field", ErrMalformed)
	}
	c := &cursor{b: val}
	n, err := c.uvarint()
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, n)
	var prev string
	for i := uint64(0); i < n; i++ {
		nameLen, err := c.uvarint()
		if err != nil {
			return nil, err
		}
		name, err := c.take(nameLen)
		if err != nil {
			return nil, err
		}
		mode, err := c.uvarint()
		if err != nil {
			return nil, err
		}
		kind, err := c.uvarint()
		if err != nil {
			return nil, err
		}
		idLen, err := c.uvarint()
		if err != nil {
			return nil, err
		}
		id, err := c.take(idLen)
		if err != nil {
			return nil, err
		}
		nm := string(name)
		if i > 0 && nm <= prev {
			return nil, fmt.Errorf("%w: tree entries not sorted/unique by name", ErrMalformed)
		}
		entries = append(entries, Entry{
			Name: nm,
			Mode: uint32(mode),
			Kind: Type(kind),
			ID:   multihash.Multihash(append([]byte(nil), id...)),
		})
		prev = nm
	}
	if !c.empty() {
		return nil, fmt.Errorf("%w: trailing bytes in tree entries", ErrMalformed)
	}
	return entries, nil
}
