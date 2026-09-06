package edge

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/graphnode"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

func h(t *testing.T, s string) multihash.Multihash {
	t.Helper()
	x, err := multihash.Sum(multihash.BLAKE3, []byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func objNode(t *testing.T, s string) graphnode.ObjectNode {
	t.Helper()
	n, err := graphnode.Object(h(t, s))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func importedSpec(t *testing.T) Spec {
	t.Helper()
	ext, err := graphnode.External("tracker", "PROJ-123")
	if err != nil {
		t.Fatal(err)
	}
	return Spec{
		Source:        objNode(t, "commit"),
		Target:        ext,
		Type:          "tracker:relates-to",
		ObservedUnder: h(t, "tree"),
		Provenance: Provenance{
			Class: Imported, Principal: "bridge-prod", Strength: object.StrengthWeak,
		},
		Validity: Validity{From: h(t, "from")},
	}
}

// TestRoundTripIsByteIdentical: encode, decode, re-encode must produce the same
// bytes, which is what makes a read-and-rewrite by another binary safe.
func TestRoundTripIsByteIdentical(t *testing.T) {
	e, err := New(importedSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	first, err := Encode(e)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Decode(first)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Encode(back)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("round trip changed the bytes:\n  %s\n  %s", first, second)
	}
	if back.Type() != e.Type() || back.Source().Key() != e.Source().Key() ||
		back.Target().Key() != e.Target().Key() {
		t.Error("round trip changed the edge's endpoints or type")
	}
}

// TestUnknownEdgeTypeRoundTrips is the §11.2 acceptance: an edge type no binary
// has ever seen survives write and read untouched, because the core never looks
// at the value. There is no registry to add it to, which is the mitigation.
func TestUnknownEdgeTypeRoundTrips(t *testing.T) {
	s := importedSpec(t)
	s.Type = "somefutureproducer:some-verb-invented-in-2031"
	e, err := New(s)
	if err != nil {
		t.Fatalf("an unseen edge type must be accepted on shape alone: %v", err)
	}
	payload, err := Encode(e)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Decode(payload)
	if err != nil {
		t.Fatalf("an unseen edge type must decode: %v", err)
	}
	if back.Type() != s.Type {
		t.Errorf("edge type changed: %q -> %q", s.Type, back.Type())
	}
	again, err := Encode(back)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, again) {
		t.Error("rewriting an unseen edge type changed the bytes")
	}
}

// TestUnknownFieldsRoundTrip is §4.4 / A3: a field a newer producer wrote and
// this binary does not model must survive a read-and-rewrite, at the top level
// and inside provenance. Dropping it silently is the failure A3 names.
func TestUnknownFieldsRoundTrip(t *testing.T) {
	e, err := New(importedSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := Encode(e)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a newer producer: add fields this binary knows nothing about.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(payload, &top); err != nil {
		t.Fatal(err)
	}
	top["future_field"] = json.RawMessage(`{"nested":[1,2,3]}`)
	var prov map[string]json.RawMessage
	if err := json.Unmarshal(top["provenance"], &prov); err != nil {
		t.Fatal(err)
	}
	prov["future_provenance_field"] = json.RawMessage(`"a value from 2031"`)
	provBytes, err := json.Marshal(prov)
	if err != nil {
		t.Fatal(err)
	}
	top["provenance"] = provBytes
	newer, err := json.Marshal(top)
	if err != nil {
		t.Fatal(err)
	}

	// This binary reads it and writes it back.
	back, err := Decode(newer)
	if err != nil {
		t.Fatalf("a record with unknown fields must decode: %v", err)
	}
	rewritten, err := Encode(back)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(newer, rewritten) {
		t.Fatalf("an older binary lost or reordered a newer producer's fields:\n  in:  %s\n  out: %s",
			newer, rewritten)
	}
}

// TestImportedEdgeIsAlwaysWeak makes G3's rule structural: a connector signs for
// a principal who holds no key, so no input can produce a stronger imported edge.
func TestImportedEdgeIsAlwaysWeak(t *testing.T) {
	for _, s := range []object.Strength{object.StrengthDelegated, object.StrengthStrong} {
		spec := importedSpec(t)
		spec.Provenance.Strength = s
		if _, err := New(spec); err == nil {
			t.Errorf("an imported edge was accepted at strength %s", s)
		}
	}
	spec := importedSpec(t)
	spec.Provenance.Model = "some-model"
	if _, err := New(spec); err == nil {
		t.Error("an imported edge carrying model provenance was accepted; it ran no model")
	}
}

// TestConstructionRefusesMalformedEdges: every check happens at the one door in.
func TestConstructionRefusesMalformedEdges(t *testing.T) {
	base := importedSpec(t)

	bad := base
	bad.Type = "unqualified"
	if _, err := New(bad); err == nil {
		t.Error("an unqualified edge type must be refused")
	}

	bad = base
	bad.ObservedUnder = nil
	if _, err := New(bad); err == nil {
		t.Error("an edge with no observed-under must be refused")
	}

	bad = base
	bad.Provenance.Class = 0
	if _, err := New(bad); err == nil {
		t.Error("an edge with no provenance class must be refused")
	}

	bad = base
	bad.Provenance.Principal = ""
	if _, err := New(bad); err == nil {
		t.Error("an edge naming no producing principal must be refused")
	}

	bad = base
	bad.Source = nil
	if _, err := New(bad); err == nil {
		t.Error("an edge with a missing endpoint must be refused")
	}
}

// TestRetentionFollowsEndpointClass: an ephemeral endpoint makes the edge
// collectable, computed from the node and not settable by the writer. Storing
// one is refused loudly rather than leaking the state it should die with.
func TestRetentionFollowsEndpointClass(t *testing.T) {
	eph, err := graphnode.Ephemeral(h(t, "speculation"))
	if err != nil {
		t.Fatal(err)
	}
	s := importedSpec(t)
	s.Target = eph
	e, err := New(s)
	if err != nil {
		t.Fatal(err)
	}
	if e.Retention() != graphnode.Collectable {
		t.Fatal("an edge to an ephemeral endpoint must be collectable")
	}

	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Put(r, e, "jan", 100); err == nil {
		t.Error("storing a collectable edge must fail loudly, not leak the endpoint it pins")
	}
}

// TestPutAndList: an edge attaches to its varvig endpoint and reads back.
func TestPutAndList(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	spec := importedSpec(t)
	e, err := New(spec)
	if err != nil {
		t.Fatal(err)
	}
	noteID, err := Put(r, e, "jan", 100)
	if err != nil {
		t.Fatal(err)
	}
	if noteID == nil {
		t.Fatal("Put returned no note id; a promotion attestation binds to it")
	}

	anchor, err := Anchor(e)
	if err != nil {
		t.Fatal(err)
	}
	got, err := List(r, anchor)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("List returned %d edges, want 1", len(got))
	}
	if got[0].Edge.Type() != spec.Type {
		t.Errorf("stored edge type = %q, want %q", got[0].Edge.Type(), spec.Type)
	}
	if !got[0].Note.Equal(noteID) {
		t.Error("the listed note id is not the one Put returned")
	}
}

// TestAnchorPrefersTheVarvigEndpoint, and refuses an edge with none.
func TestAnchorRequiresAVarvigEndpoint(t *testing.T) {
	a, err := graphnode.External("tracker", "A-1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := graphnode.External("ci", "run/1")
	if err != nil {
		t.Fatal(err)
	}
	s := importedSpec(t)
	s.Source, s.Target = a, b
	e, err := New(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Anchor(e); err == nil {
		t.Error("an edge between two foreign systems has nothing here to attach to")
	}
}
