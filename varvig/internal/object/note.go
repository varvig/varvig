package object

import (
	"fmt"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
)

// Note attaches metadata to another object without changing that object's
// bytes or identity (design §2, "attach data to an immutable object without
// changing its hash"). Test results, review verdicts, deploy outcomes, and
// incident links accrete onto a change over time.
//
// A note is itself an immutable, content-addressed object. Accretion is a
// chain: each new note in a (namespace, target) pair points at the previous
// one via Parent, so the full history is preserved and syncs like any object.
type Note struct {
	Target    multihash.Multihash // the object this note is about
	Namespace string              // e.g. "test-results", "review", "deploy"
	Payload   []byte              // opaque metadata
	Parent    multihash.Multihash // previous note in this (namespace, target) chain, or nil
	Timestamp int64
	Author    string
}

// NewNote builds a note object.
func NewNote(n Note) *Object {
	fields := []field{
		{tag: tagNoteTarget, val: append([]byte(nil), n.Target...)},
		{tag: tagNoteNamespace, val: []byte(n.Namespace)},
		{tag: tagNotePayload, val: append([]byte(nil), n.Payload...)},
		{tag: tagNoteTimestamp, val: appendUvarint(nil, uint64(n.Timestamp))},
		{tag: tagNoteAuthor, val: []byte(n.Author)},
	}
	if n.Parent != nil {
		fields = append(fields, field{tag: tagNoteParent, val: append([]byte(nil), n.Parent...)})
	}
	return newObject(TypeNote, fields)
}

// AsNote decodes the typed view of a note object.
func (o *Object) AsNote() (Note, error) {
	if o.typ != TypeNote {
		return Note{}, fmt.Errorf("object: not a note (%s)", o.typ)
	}
	var n Note
	if v, ok := o.Field(tagNoteTarget); ok {
		n.Target = multihash.Multihash(append([]byte(nil), v...))
	}
	if v, ok := o.Field(tagNoteNamespace); ok {
		n.Namespace = string(v)
	}
	if v, ok := o.Field(tagNotePayload); ok {
		n.Payload = append([]byte(nil), v...)
	}
	if v, ok := o.Field(tagNoteParent); ok {
		n.Parent = multihash.Multihash(append([]byte(nil), v...))
	}
	if v, ok := o.Field(tagNoteAuthor); ok {
		n.Author = string(v)
	}
	if v, ok := o.Field(tagNoteTimestamp); ok {
		ts, k, err := readUvarint(v)
		if err != nil || k != len(v) {
			return Note{}, fmt.Errorf("%w: bad note timestamp", ErrMalformed)
		}
		n.Timestamp = int64(ts)
	}
	return n, nil
}
