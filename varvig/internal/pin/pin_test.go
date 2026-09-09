package pin

import (
	"testing"

	"github.com/varvig/varvig/varvig/internal/multihash"

	"github.com/varvig/varvig/varvig/internal/reserved"
)

// TestPrefixComesFromTheReservation is the guard for the thing that made this
// worth reserving: GC's root walk and the p2p pin handlers both act on this
// name, so a second spelling of it that drifted would silently stop pins being
// recognised — objects would either be collected while still pinned, or pinned
// forever. One constant, checked here.
func TestPrefixComesFromTheReservation(t *testing.T) {
	if Prefix != reserved.PinsPrefix {
		t.Fatalf("pin.Prefix = %q, reserved.PinsPrefix = %q; the two spellings have drifted", Prefix, reserved.PinsPrefix)
	}
	if !reserved.IsPinRef(Prefix + "aabb/0000000000000000/1e20ff") {
		t.Error("the reservation does not recognise a ref built from this prefix")
	}
}

// TestRefNameIsSelfDescribing pins the property the encoding exists for: a
// pin's expiry is recoverable from its name alone, with no dependence on reflog
// messages that retention may compact away.
func TestRefNameIsSelfDescribing(t *testing.T) {
	const peer = "peer/with/slashes"
	const notAfter int64 = 0x5f5e100
	name := RefName(peer, notAfter, testHash(t))

	if !IsPinRef(name) {
		t.Fatalf("RefName produced %q, which is not a pin ref", name)
	}
	gotPeer, gotNotAfter, gotHash, ok := Parse(name)
	if !ok {
		t.Fatalf("Parse(%q) failed", name)
	}
	if gotPeer != peer {
		// The hex encoding is what makes an identity containing '/' survive as
		// one path segment.
		t.Errorf("peer = %q, want %q", gotPeer, peer)
	}
	if gotNotAfter != notAfter {
		t.Errorf("not_after = %d, want %d", gotNotAfter, notAfter)
	}
	if gotHash.Hex() != testHash(t).Hex() {
		t.Errorf("hash = %s, want %s", gotHash.Hex(), testHash(t).Hex())
	}
}

func testHash(t *testing.T) multihash.Multihash {
	t.Helper()
	h, err := multihash.Sum(multihash.Default, []byte("pinned object"))
	if err != nil {
		t.Fatal(err)
	}
	return h
}
