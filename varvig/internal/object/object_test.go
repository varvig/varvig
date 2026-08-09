package object

import (
	"bytes"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
)

func mustID(t *testing.T, o *Object) multihash.Multihash {
	t.Helper()
	id, err := o.ID(multihash.Default)
	if err != nil {
		t.Fatalf("ID: %v", err)
	}
	return id
}

func TestBlobRoundTrip(t *testing.T) {
	b := NewBlob([]byte("hello, agents"))
	enc := b.Encode()
	got, err := Decode(enc)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Type() != TypeBlob {
		t.Fatalf("type = %v, want blob", got.Type())
	}
	content, ok := got.BlobContent()
	if !ok || string(content) != "hello, agents" {
		t.Fatalf("content = %q ok=%v", content, ok)
	}
	// decode∘encode must reproduce the exact bytes.
	if !bytes.Equal(got.Encode(), enc) {
		t.Fatal("re-encode not byte-identical")
	}
}

func TestEncodingIsDeterministic(t *testing.T) {
	a := NewBlob([]byte("x"))
	b := NewBlob([]byte("x"))
	if !bytes.Equal(a.Encode(), b.Encode()) {
		t.Fatal("identical blobs encode differently")
	}
	if !mustID(t, a).Equal(mustID(t, b)) {
		t.Fatal("identical blobs have different ids")
	}
}

func TestTreeRoundTripAndSorting(t *testing.T) {
	child := NewBlob([]byte("data"))
	childID := mustID(t, child)
	// Provide entries out of order; NewTree must canonicalize.
	tr := NewTree([]Entry{
		{Name: "z.txt", Mode: 0o100644, Kind: TypeBlob, ID: childID},
		{Name: "a.txt", Mode: 0o100644, Kind: TypeBlob, ID: childID},
	})
	enc := tr.Encode()
	got, err := Decode(enc)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	entries, err := got.TreeEntries()
	if err != nil {
		t.Fatalf("TreeEntries: %v", err)
	}
	if len(entries) != 2 || entries[0].Name != "a.txt" || entries[1].Name != "z.txt" {
		t.Fatalf("entries not sorted: %+v", entries)
	}
	if !bytes.Equal(got.Encode(), enc) {
		t.Fatal("tree re-encode not byte-identical")
	}
	// Order of construction must not affect identity.
	tr2 := NewTree([]Entry{
		{Name: "a.txt", Mode: 0o100644, Kind: TypeBlob, ID: childID},
		{Name: "z.txt", Mode: 0o100644, Kind: TypeBlob, ID: childID},
	})
	if !mustID(t, tr).Equal(mustID(t, tr2)) {
		t.Fatal("tree identity depends on input order")
	}
}

func TestChangeRoundTrip(t *testing.T) {
	treeID := mustID(t, NewBlob([]byte("tree-stand-in")))
	p1 := mustID(t, NewBlob([]byte("p1")))
	p2 := mustID(t, NewBlob([]byte("p2")))
	ch := NewChange(Change{
		Tree:      treeID,
		Parents:   []multihash.Multihash{p2, p1}, // out of order on purpose
		Message:   "did the thing",
		Timestamp: 1723100000,
		Author:    "agent-7",
	})
	got, err := Decode(ch.Encode())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	c, err := got.AsChange()
	if err != nil {
		t.Fatalf("AsChange: %v", err)
	}
	if !c.Tree.Equal(treeID) {
		t.Fatalf("tree mismatch")
	}
	if c.Message != "did the thing" || c.Author != "agent-7" || c.Timestamp != 1723100000 {
		t.Fatalf("metadata mismatch: %+v", c)
	}
	if len(c.Parents) != 2 {
		t.Fatalf("parents = %d, want 2", len(c.Parents))
	}
	// Parents must come back sorted (canonical).
	if bytes.Compare(c.Parents[0], c.Parents[1]) >= 0 {
		t.Fatal("parents not sorted")
	}
}

// TestUnmaterializedChangeRoundTrip covers decision D1: a change with no tree
// (a ticket, tickets §1.1) round-trips with its unmaterialized state intact,
// and the tree tag is absent from the encoding rather than present-but-empty.
func TestUnmaterializedChangeRoundTrip(t *testing.T) {
	ch := NewChange(Change{
		Message:   "add rate limiting to the auth module",
		Timestamp: 1723100000,
		Author:    "director",
	})
	// The tree tag must be absent, not present with an empty value.
	if _, ok := ch.Field(tagChangeTree); ok {
		t.Fatal("unmaterialized change emitted a tree field")
	}
	got, err := Decode(ch.Encode())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	c, err := got.AsChange()
	if err != nil {
		t.Fatalf("AsChange: %v", err)
	}
	if c.Materialized() {
		t.Fatal("decoded change reports materialized, want unmaterialized")
	}
	if c.Tree != nil {
		t.Fatalf("unmaterialized change has a tree: %x", c.Tree)
	}
	if !bytes.Equal(got.Encode(), ch.Encode()) {
		t.Fatal("unmaterialized change not byte-identical on round-trip")
	}
}

// TestUnmaterializedNotEqualEmptyTree is the core D1 guarantee: an
// unmaterialized change must not encode as, or hash as, a change materialized
// to the empty tree. Conflating them is unrecoverable once written.
func TestUnmaterializedNotEqualEmptyTree(t *testing.T) {
	base := Change{Message: "same intent", Timestamp: 1, Author: "director"}

	unmaterialized := NewChange(base)

	withEmptyTree := base
	withEmptyTree.Tree = mustID(t, NewTree(nil)) // materialized to the empty tree
	materialized := NewChange(withEmptyTree)

	if bytes.Equal(unmaterialized.Encode(), materialized.Encode()) {
		t.Fatal("unmaterialized change encodes identically to an empty-tree change")
	}
	if mustID(t, unmaterialized).Equal(mustID(t, materialized)) {
		t.Fatal("unmaterialized change hashes identically to an empty-tree change")
	}
	// And the empty-tree change is genuinely materialized.
	mc, err := materialized.AsChange()
	if err != nil {
		t.Fatalf("AsChange: %v", err)
	}
	if !mc.Materialized() {
		t.Fatal("empty-tree change reports unmaterialized")
	}
}

// TestUnknownFieldsRoundTrip is the load-bearing §4.4 guarantee: a build that
// does not understand a field must preserve it byte-for-byte on rewrite.
func TestUnknownFieldsRoundTrip(t *testing.T) {
	b := NewBlob([]byte("payload"))
	// Simulate a newer build attaching an unknown field (e.g. provenance).
	b.SetField(99, []byte("signed-by-agent"))
	enc := b.Encode()

	// A build reading it still sees the blob content...
	got, err := Decode(enc)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	content, _ := got.BlobContent()
	if string(content) != "payload" {
		t.Fatalf("content = %q", content)
	}
	// ...and re-encoding preserves the unknown field exactly.
	if !bytes.Equal(got.Encode(), enc) {
		t.Fatal("unknown field not preserved on round-trip")
	}
	if v, ok := got.Field(99); !ok || string(v) != "signed-by-agent" {
		t.Fatalf("unknown field lost: %q ok=%v", v, ok)
	}
}

func TestSetFieldMaintainsCanonicalOrder(t *testing.T) {
	b := NewBlob([]byte("x"))
	b.SetField(50, []byte("a"))
	b.SetField(5, []byte("b"))
	b.SetField(200, []byte("c"))
	// Encode then decode must succeed, proving fields are sorted/unique.
	if _, err := Decode(b.Encode()); err != nil {
		t.Fatalf("Decode after SetField: %v", err)
	}
}

func TestDecodeRejectsMalformed(t *testing.T) {
	good := NewBlob([]byte("ok")).Encode()

	cases := map[string][]byte{
		"empty":         {},
		"bad magic":     append([]byte("XXXX"), good[4:]...),
		"truncated":     good[:len(good)-1],
		"trailing byte": append(append([]byte(nil), good...), 0x00),
	}
	for name, in := range cases {
		if _, err := Decode(in); err == nil {
			t.Errorf("%s: Decode succeeded, want error", name)
		}
	}
}

func TestDecodeRejectsUnsortedFields(t *testing.T) {
	// Hand-craft an object with two fields in descending tag order.
	var buf []byte
	buf = append(buf, Magic[:]...)
	buf = appendUvarint(buf, uint64(TypeBlob))
	buf = appendUvarint(buf, 2) // two fields
	// tag 5 first...
	buf = appendUvarint(buf, 5)
	buf = appendUvarint(buf, 1)
	buf = append(buf, 'a')
	// ...then tag 1 (out of order)
	buf = appendUvarint(buf, 1)
	buf = appendUvarint(buf, 1)
	buf = append(buf, 'b')
	if _, err := Decode(buf); err == nil {
		t.Fatal("Decode accepted unsorted fields")
	}
}

func TestDecodeRejectsNonMinimalVarint(t *testing.T) {
	// A field count encoded non-minimally as 0x80 0x00 (== 0, overlong).
	var buf []byte
	buf = append(buf, Magic[:]...)
	buf = appendUvarint(buf, uint64(TypeBlob))
	buf = append(buf, 0x80, 0x00) // non-minimal zero
	if _, err := Decode(buf); err == nil {
		t.Fatal("Decode accepted non-minimal varint")
	}
}

func TestOpaqueUnknownType(t *testing.T) {
	// An object of an unknown type must still decode and round-trip.
	var buf []byte
	buf = append(buf, Magic[:]...)
	buf = appendUvarint(buf, 9999) // unknown type
	buf = appendUvarint(buf, 1)
	buf = appendUvarint(buf, 7)
	buf = appendUvarint(buf, 3)
	buf = append(buf, 'a', 'b', 'c')
	got, err := Decode(buf)
	if err != nil {
		t.Fatalf("Decode unknown type: %v", err)
	}
	if got.Type() != Type(9999) {
		t.Fatalf("type = %v", got.Type())
	}
	if !bytes.Equal(got.Encode(), buf) {
		t.Fatal("unknown-type object not preserved on round-trip")
	}
}
