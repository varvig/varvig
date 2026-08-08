// Package object defines Loom's content-addressed object model and the
// canonical, frozen wire encoding used to compute object identities.
//
// # Format LOM1 (frozen — see FORMAT.md)
//
// An object is a type tag followed by a set of typed fields:
//
//	magic       4 bytes  "LOM1"
//	objectType  uvarint
//	fieldCount  uvarint
//	fields      fieldCount records, sorted ascending by tag, tags unique:
//	              tag     uvarint
//	              length  uvarint
//	              value   length bytes
//
// All varints are minimal (no non-minimal or overlong encodings), fields are
// sorted by tag with no duplicates, and there are no trailing bytes. These
// rules give every logical object exactly one byte representation, which is
// what makes the identity multihash(bytes) stable.
//
// # Unknown fields round-trip (design §4.4)
//
// An object is stored internally as its raw canonical field list. Typed
// accessors are views over that list. A field whose tag a given build does
// not recognize is retained verbatim and re-emitted in order, so decoding and
// re-encoding an object without changes reproduces its exact bytes — and thus
// its exact identity. This is how provenance and signatures written by a newer
// build survive being handled by an older one: preserve-or-refuse, never
// silently degrade.
package object

import (
	"bytes"
	"errors"
	"fmt"
	"sort"

	"github.com/dividebyzero/claude-experiments/loom/internal/multihash"
)

// Type identifies an object kind. The set is registry-extensible: an unknown
// type decodes to an Opaque object that still round-trips exactly.
type Type uint64

const (
	TypeBlob       Type = 1
	TypeTree       Type = 2
	TypeChange     Type = 3
	TypeProvenance Type = 4
	TypeNote       Type = 5
	TypeHookConfig Type = 6
)

// Magic frames every object. It names the frozen format family LOM1.
var Magic = [4]byte{'L', 'O', 'M', '1'}

// Field tags per object type. Tags are append-only: new capabilities take new
// tags; existing tags never change meaning.
const (
	tagBlobContent = 1

	tagTreeEntries = 1

	tagChangeTree       = 1
	tagChangeParents    = 2
	tagChangeMessage    = 3
	tagChangeTimestamp  = 4
	tagChangeAuthor     = 5
	tagChangeProvenance = 6 // id of a TypeProvenance object (design §1.1, §2.1)
	tagChangeSignature  = 7 // opaque signature blob over the change sans this tag

	tagProvAuthority    = 1
	tagProvModel        = 2
	tagProvModelVersion = 3
	tagProvSampling     = 4
	tagProvToolPerms    = 5
	tagProvToolHash     = 6
	tagProvTaskSpec     = 7
	tagProvContextRead  = 8
	tagProvReasoning    = 9

	tagNoteTarget    = 1
	tagNoteNamespace = 2
	tagNotePayload   = 3
	tagNoteParent    = 4
	tagNoteTimestamp = 5
	tagNoteAuthor    = 6

	tagHookEntries = 1
)

// ErrMalformed marks any input that violates the canonical LOM1 framing.
var ErrMalformed = errors.New("object: malformed encoding")

func (t Type) String() string {
	switch t {
	case TypeBlob:
		return "blob"
	case TypeTree:
		return "tree"
	case TypeChange:
		return "change"
	case TypeProvenance:
		return "provenance"
	case TypeNote:
		return "note"
	case TypeHookConfig:
		return "hookconfig"
	default:
		return fmt.Sprintf("type-%d", uint64(t))
	}
}

type field struct {
	tag uint64
	val []byte
}

// Object is a decoded LOM1 object: a type and a canonical field list. It is
// the single in-memory representation for all object kinds; typed
// constructors and accessors build on it.
type Object struct {
	typ    Type
	fields []field // invariant: sorted ascending by tag, tags unique
}

// Type returns the object's type tag.
func (o *Object) Type() Type { return o.typ }

// Field returns the raw value stored under tag, if present.
func (o *Object) Field(tag uint64) ([]byte, bool) {
	i := o.index(tag)
	if i < 0 {
		return nil, false
	}
	return o.fields[i].val, true
}

// SetField inserts or replaces the value stored under tag, preserving all
// other fields and the canonical ordering. Passing nil is distinct from
// absence: it stores a present, empty field.
func (o *Object) SetField(tag uint64, val []byte) {
	cp := append([]byte(nil), val...)
	i := o.index(tag)
	if i >= 0 {
		o.fields[i].val = cp
		return
	}
	o.fields = append(o.fields, field{tag: tag, val: cp})
	sort.Slice(o.fields, func(a, b int) bool { return o.fields[a].tag < o.fields[b].tag })
}

// DeleteField removes a field by tag, if present.
func (o *Object) DeleteField(tag uint64) {
	i := o.index(tag)
	if i < 0 {
		return
	}
	o.fields = append(o.fields[:i], o.fields[i+1:]...)
}

func (o *Object) index(tag uint64) int {
	for i := range o.fields {
		if o.fields[i].tag == tag {
			return i
		}
	}
	return -1
}

// Encode serializes the object to its canonical LOM1 bytes.
func (o *Object) Encode() []byte {
	var buf []byte
	buf = append(buf, Magic[:]...)
	buf = appendUvarint(buf, uint64(o.typ))
	buf = appendUvarint(buf, uint64(len(o.fields)))
	for _, f := range o.fields {
		buf = appendUvarint(buf, f.tag)
		buf = appendUvarint(buf, uint64(len(f.val)))
		buf = append(buf, f.val...)
	}
	return buf
}

// Decode parses canonical LOM1 bytes into an Object, refusing any input that
// is not in canonical form (bad magic, non-minimal varints, unsorted or
// duplicate tags, or trailing bytes). Refusing non-canonical input keeps
// decode∘encode an exact identity.
func Decode(b []byte) (*Object, error) {
	c := &cursor{b: b}
	magic, err := c.take(4)
	if err != nil || !bytes.Equal(magic, Magic[:]) {
		return nil, fmt.Errorf("%w: bad magic", ErrMalformed)
	}
	typ, err := c.uvarint()
	if err != nil {
		return nil, err
	}
	n, err := c.uvarint()
	if err != nil {
		return nil, err
	}
	fields := make([]field, 0, n)
	var prev uint64
	for i := uint64(0); i < n; i++ {
		tag, err := c.uvarint()
		if err != nil {
			return nil, err
		}
		if i > 0 && tag <= prev {
			return nil, fmt.Errorf("%w: fields not sorted/unique by tag", ErrMalformed)
		}
		length, err := c.uvarint()
		if err != nil {
			return nil, err
		}
		val, err := c.take(length)
		if err != nil {
			return nil, err
		}
		fields = append(fields, field{tag: tag, val: append([]byte(nil), val...)})
		prev = tag
	}
	if !c.empty() {
		return nil, fmt.Errorf("%w: trailing bytes", ErrMalformed)
	}
	return &Object{typ: Type(typ), fields: fields}, nil
}

// ID computes the object's identity under algorithm c.
func (o *Object) ID(c multihash.Code) (multihash.Multihash, error) {
	return multihash.Sum(c, o.Encode())
}

// encodeWithout serializes the object omitting the field with the given tag.
// It is the basis for signing over everything but the signature itself.
func (o *Object) encodeWithout(skip uint64) []byte {
	var buf []byte
	buf = append(buf, Magic[:]...)
	buf = appendUvarint(buf, uint64(o.typ))
	n := 0
	for _, f := range o.fields {
		if f.tag != skip {
			n++
		}
	}
	buf = appendUvarint(buf, uint64(n))
	for _, f := range o.fields {
		if f.tag == skip {
			continue
		}
		buf = appendUvarint(buf, f.tag)
		buf = appendUvarint(buf, uint64(len(f.val)))
		buf = append(buf, f.val...)
	}
	return buf
}

// SignableBytes returns the canonical bytes a signature covers: the whole
// change except the signature field. Signing and verifying both use it, so the
// signed and verified byte strings are identical (the signer simply has not
// attached the signature field yet).
func (o *Object) SignableBytes() []byte {
	return o.encodeWithout(tagChangeSignature)
}

// SetSignature attaches an opaque signature blob. Its interpretation
// (algorithm, public key, signature bytes) belongs to a higher layer; the
// object model only knows there is a signature field and can exclude it from
// SignableBytes.
func (o *Object) SetSignature(blob []byte) { o.SetField(tagChangeSignature, blob) }

// RawSignature returns the opaque signature blob, if present.
func (o *Object) RawSignature() ([]byte, bool) { return o.Field(tagChangeSignature) }

// newObject builds an Object from an unsorted set of fields, sorting them and
// asserting tag uniqueness (a programming error if violated).
func newObject(typ Type, fields []field) *Object {
	sort.Slice(fields, func(a, b int) bool { return fields[a].tag < fields[b].tag })
	for i := 1; i < len(fields); i++ {
		if fields[i].tag == fields[i-1].tag {
			panic(fmt.Sprintf("object: duplicate field tag %d", fields[i].tag))
		}
	}
	return &Object{typ: typ, fields: fields}
}

// --- cursor over a byte slice with strict, minimal varint decoding ---

type cursor struct {
	b []byte
	i int
}

func (c *cursor) uvarint() (uint64, error) {
	v, n, err := readUvarint(c.b[c.i:])
	if err != nil {
		return 0, err
	}
	c.i += n
	return v, nil
}

func (c *cursor) take(n uint64) ([]byte, error) {
	if n > uint64(len(c.b)-c.i) {
		return nil, fmt.Errorf("%w: truncated", ErrMalformed)
	}
	s := c.b[c.i : c.i+int(n)]
	c.i += int(n)
	return s, nil
}

func (c *cursor) empty() bool { return c.i == len(c.b) }

func appendUvarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func readUvarint(b []byte) (val uint64, n int, err error) {
	var x uint64
	var s uint
	for i := 0; i < len(b); i++ {
		ch := b[i]
		if i == 10 {
			return 0, 0, fmt.Errorf("%w: varint overflow", ErrMalformed)
		}
		if ch < 0x80 {
			if i == 9 && ch > 1 {
				return 0, 0, fmt.Errorf("%w: varint overflow", ErrMalformed)
			}
			if ch == 0 && i != 0 {
				return 0, 0, fmt.Errorf("%w: non-minimal varint", ErrMalformed)
			}
			return x | uint64(ch)<<s, i + 1, nil
		}
		x |= uint64(ch&0x7f) << s
		s += 7
	}
	return 0, 0, fmt.Errorf("%w: truncated varint", ErrMalformed)
}
