package object

import (
	"bytes"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
)

func revHash(t *testing.T, s string) multihash.Multihash {
	t.Helper()
	h, err := multihash.Sum(multihash.SHA2_256, []byte(s))
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	return h
}

// TestChangeFulfillsRoundTrip: the ticket→commit link survives encode/decode.
func TestChangeFulfillsRoundTrip(t *testing.T) {
	rev := revHash(t, "intent-revision")
	o := NewChange(Change{Tree: revHash(t, "tree"), Message: "impl", Timestamp: 1, Author: "a", Fulfills: rev})
	got, err := Decode(o.Encode())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	c, err := got.AsChange()
	if err != nil {
		t.Fatalf("AsChange: %v", err)
	}
	if !c.Fulfills.Equal(rev) {
		t.Fatalf("Fulfills = %s, want %s", c.Fulfills.Hex(), rev.Hex())
	}
}

// TestChangeWithoutFulfillsIsAdditive: a change fulfilling nothing carries no
// Fulfills field — so it encodes exactly as a pre-field build would.
func TestChangeWithoutFulfillsIsAdditive(t *testing.T) {
	o := NewChange(Change{Tree: revHash(t, "tree"), Message: "impl", Timestamp: 1, Author: "a"})
	if _, ok := o.Field(tagChangeFulfills); ok {
		t.Fatal("a change with no Fulfills must not emit the field")
	}
	c, _ := o.AsChange()
	if c.Fulfills != nil {
		t.Fatalf("decoded Fulfills = %s, want nil", c.Fulfills.Hex())
	}
}

// TestFulfillsSurvivesUnknownFieldRoundTrip: a build that does not know tag 9
// reads and re-writes the object through the generic codec; the canonical
// encoding preserves every field, so Fulfills is not dropped (§8, C-format).
func TestFulfillsSurvivesUnknownFieldRoundTrip(t *testing.T) {
	rev := revHash(t, "intent-revision")
	orig := NewChange(Change{Tree: revHash(t, "tree"), Message: "impl", Timestamp: 1, Author: "a", Fulfills: rev})
	encoded := orig.Encode()

	// Generic round-trip: decode to an opaque object and re-encode, as a codec
	// that has no Change.Fulfills field would. The bytes must be identical.
	generic, err := Decode(encoded)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !bytes.Equal(generic.Encode(), encoded) {
		t.Fatal("generic re-encode changed the bytes (unknown field not preserved)")
	}
	c, _ := generic.AsChange()
	if !c.Fulfills.Equal(rev) {
		t.Fatalf("Fulfills lost across generic round-trip: %v", c.Fulfills)
	}
}
