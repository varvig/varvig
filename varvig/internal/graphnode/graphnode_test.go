package graphnode

import (
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
)

func hash(t *testing.T, s string) multihash.Multihash {
	t.Helper()
	h, err := multihash.Sum(multihash.BLAKE3, []byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func mustObject(t *testing.T, s string) ObjectNode {
	t.Helper()
	n, err := Object(hash(t, s))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestClassIsComputedNotStored: the class comes from the concrete type, so there
// is no field a writer could set to disagree with it (GRAPH.md §11.5).
func TestClassIsComputedNotStored(t *testing.T) {
	obj, _ := Object(hash(t, "a"))
	eph, _ := Ephemeral(hash(t, "a")) // same bytes, different class
	id, _ := Identity("refs/varvig/nodes/abc", hash(t, "r"))
	ext, _ := External("tracker", "PROJ-123")

	for _, tc := range []struct {
		n    Node
		want Class
	}{
		{obj, ClassObject},
		{eph, ClassEphemeral},
		{id, ClassIdentity},
		{ext, ClassExternal},
	} {
		if got := tc.n.Class(); got != tc.want {
			t.Errorf("%T.Class() = %v, want %v", tc.n, got, tc.want)
		}
	}

	// The same content id in two classes is two distinct nodes, and their
	// retention rules are opposite. If the class were not part of the key they
	// would collide.
	if obj.Key() == eph.Key() {
		t.Fatal("an object and an ephemeral node with identical bytes share a key")
	}
}

// TestRetentionIsDerivedFromClass: an ephemeral endpoint is collectable and
// everything else is durable, with no way for a writer to say otherwise
// (GRAPH.md §11.5).
func TestRetentionIsDerivedFromClass(t *testing.T) {
	eph, _ := Ephemeral(hash(t, "speculation"))
	if got := RetentionOf(eph); got != Collectable {
		t.Errorf("ephemeral retention = %v, want collectable", got)
	}
	obj, _ := Object(hash(t, "commit"))
	id, _ := Identity("refs/varvig/nodes/abc", hash(t, "r"))
	ext, _ := External("ci", "run/8891")
	for _, n := range []Node{obj, id, ext} {
		if got := RetentionOf(n); got != Durable {
			t.Errorf("%T retention = %v, want durable", n, got)
		}
	}
}

// TestKeyRoundTrip: every node this package can build survives Key -> ParseKey
// unchanged, which is what lets an edge record store a node as text and a binary
// that has never seen it hand it back untouched.
func TestKeyRoundTrip(t *testing.T) {
	obj, _ := Object(hash(t, "commit"))
	eph, _ := Ephemeral(hash(t, "attempt"))
	id, _ := Identity("refs/varvig/nodes/deadbeef", hash(t, "rev"))
	ext, _ := External("tracker", "PROJ-123")
	// A foreign id containing colons and slashes is ordinary, not exotic.
	extHard, _ := External("ci", "run/8891:step:3")

	for _, want := range []Node{obj, eph, id, ext, extHard} {
		got, err := ParseKey(want.Key())
		if err != nil {
			t.Fatalf("ParseKey(%q): %v", want.Key(), err)
		}
		if got.Key() != want.Key() {
			t.Errorf("round trip changed the key: %q -> %q", want.Key(), got.Key())
		}
		if got.Class() != want.Class() {
			t.Errorf("round trip changed the class of %q: %v -> %v",
				want.Key(), want.Class(), got.Class())
		}
	}
}

// TestParseKeyRefusesUnknownClass: a reader that cannot understand an endpoint
// says so. Treating it as absent is how a gap becomes a wrong answer (§5).
func TestParseKeyRefusesUnknownClass(t *testing.T) {
	for _, bad := range []string{
		"future:whatever",
		"nocolon",
		"id:refs/varvig/nodes/x", // identity with no revision
		"ext:tracker",            // external with no foreign id
		"obj:nothex",
	} {
		if n, err := ParseKey(bad); err == nil {
			t.Errorf("ParseKey(%q) = %v, want an error", bad, n)
		}
	}
}

// TestConstructorsRefuseMalformedNodes: the validating constructor is the only
// way in, so these can never reach an edge record.
func TestConstructorsRefuseMalformedNodes(t *testing.T) {
	if _, err := Object(nil); err == nil {
		t.Error("Object(nil) must fail")
	}
	if _, err := Ephemeral(nil); err == nil {
		t.Error("Ephemeral(nil) must fail")
	}
	if _, err := Identity("src/auth.ts", hash(t, "r")); err == nil {
		t.Error("a path is not a ref and must not make an identity node")
	}
	if _, err := Identity("refs/varvig/nodes/x", nil); err == nil {
		t.Error("an identity node without a revision must fail — that is the rename bug")
	}
	if _, err := Identity("refs/varvig/nodes/x@y", hash(t, "r")); err == nil {
		t.Error("a ref containing the key separator must fail; it would make the key ambiguous")
	}
	if _, err := External("", "PROJ-1"); err == nil {
		t.Error("External with no system must fail")
	}
	if _, err := External("a:b", "PROJ-1"); err == nil {
		t.Error("a system tag containing a colon must fail; it would make the key ambiguous")
	}
}

// TestZeroValueIsNotAUsableNode: the classes are structs, so a zero value is
// constructible without the constructor. It must not encode to something that
// parses back — otherwise the validating constructor is bypassable by accident.
func TestZeroValueIsNotAUsableNode(t *testing.T) {
	for _, n := range []Node{ObjectNode{}, EphemeralNode{}, IdentityNode{}, ExternalNode{}} {
		if _, err := ParseKey(n.Key()); err == nil {
			t.Errorf("the zero %T encoded to a parseable key %q", n, n.Key())
		}
	}
}
