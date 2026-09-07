package edge

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/graphnode"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/reserved"
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
// collectable, computed from the node and not settable by the writer.
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
	// And a durable one stays durable.
	if d, err := New(importedSpec(t)); err != nil {
		t.Fatal(err)
	} else if d.Retention() != graphnode.Durable {
		t.Error("an edge between durable endpoints must be durable")
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

// ephemeralEdge builds a collectable edge attached to a speculation state.
func ephemeralEdge(t *testing.T, r *repo.Repo, state multihash.Multihash, typ string) StoredEdge {
	t.Helper()
	eph, err := graphnode.Ephemeral(state)
	if err != nil {
		t.Fatal(err)
	}
	far, err := graphnode.External("agent", "belief/"+typ)
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(Spec{
		Source: eph, Target: far, Type: typ, ObservedUnder: state,
		Provenance: Provenance{
			Class: Asserted, Principal: "planner", Strength: object.StrengthDelegated,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// TestACollectableEdgeIsNeverANote is the structural half of §11.5: a note pins
// its target and its ref is a GC root and its ref move is reflogged, so an
// ephemeral-anchored note would keep alive the very state it should die with.
// It must not become a note at all.
func TestACollectableEdgeIsNeverANote(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := h(t, "attempt-1")
	e := ephemeralEdge(t, r, state, "agent:couples-with")
	if _, err := Put(r, e, "planner", 100); err != nil {
		t.Fatalf("a collectable edge must be storable: %v", err)
	}

	// Nothing in the note namespace, and therefore no ref and no reflog entry
	// that could pin the state.
	refs, err := r.Refs.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range refs {
		if strings.Contains(name, reserved.NoteEdge) {
			t.Errorf("a collectable edge created an edge note ref: %s", name)
		}
	}
	// It is readable all the same: the fork is a retention decision, not a
	// query distinction.
	got, err := List(r, state)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Edge.Type() != "agent:couples-with" {
		t.Fatalf("the collectable edge is not readable back: %+v", got)
	}
}

// TestEdgeCountReturnsToBaselineExactly is §11.5's standing invariant: create N
// speculation states with edges, discard them, and the count must return to
// baseline exactly — not approximately, and without a sweep having to remember
// anything.
func TestEdgeCountReturnsToBaselineExactly(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := CountAllEphemeral(r)
	if err != nil {
		t.Fatal(err)
	}
	if baseline != 0 {
		t.Fatalf("baseline is %d, want 0", baseline)
	}

	// A durable edge, which must survive the discard untouched.
	durable, err := New(importedSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Put(r, durable, "conn", 50); err != nil {
		t.Fatal(err)
	}
	durableAnchor, err := Anchor(durable)
	if err != nil {
		t.Fatal(err)
	}

	const states, perState = 20, 5
	var made []multihash.Multihash
	for i := 0; i < states; i++ {
		state := h(t, "attempt-"+string(rune('a'+i)))
		made = append(made, state)
		for j := 0; j < perState; j++ {
			e := ephemeralEdge(t, r, state, "agent:belief-"+string(rune('a'+j)))
			if _, err := Put(r, e, "planner", int64(100+i)); err != nil {
				t.Fatal(err)
			}
		}
	}
	total, err := CountAllEphemeral(r)
	if err != nil {
		t.Fatal(err)
	}
	if total != states*perState {
		t.Fatalf("stored %d collectable edges, want %d", total, states*perState)
	}

	// Discard every state, as a prune would.
	for _, state := range made {
		if err := ForgetState(r, state); err != nil {
			t.Fatal(err)
		}
	}
	after, err := CountAllEphemeral(r)
	if err != nil {
		t.Fatal(err)
	}
	if after != baseline {
		t.Errorf("after discarding every state the count is %d, want the baseline %d", after, baseline)
	}

	// The durable edge is untouched: retention is per endpoint class, so
	// discarding attempts cannot collect real knowledge.
	stillThere, err := List(r, durableAnchor)
	if err != nil {
		t.Fatal(err)
	}
	if len(stillThere) != 1 {
		t.Errorf("the durable edge did not survive the discard: %d found", len(stillThere))
	}

	// Discarding twice is not an error.
	if err := ForgetState(r, made[0]); err != nil {
		t.Errorf("ForgetState must be idempotent: %v", err)
	}
}

// TestPerStateEdgeBudgetFailsLoudlyAndNamesTheCount: §1.5 expects thousands of
// speculation states, and §8 names edge retention as how that buries the store.
// Excess must fail at the moment it happens, naming the count, rather than
// degrading the store invisibly.
func TestPerStateEdgeBudgetFailsLoudlyAndNamesTheCount(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old := MaxPerState
	MaxPerState = 3
	defer func() { MaxPerState = old }()

	state := h(t, "busy-attempt")
	for i := 0; i < MaxPerState; i++ {
		e := ephemeralEdge(t, r, state, "agent:belief-"+string(rune('a'+i)))
		if _, err := Put(r, e, "planner", 100); err != nil {
			t.Fatalf("edge %d within budget was refused: %v", i, err)
		}
	}
	over := ephemeralEdge(t, r, state, "agent:belief-over")
	_, err = Put(r, over, "planner", 100)
	if err == nil {
		t.Fatal("exceeding the per-state budget must fail")
	}
	if !errors.Is(err, ErrStateEdgeBudget) {
		t.Errorf("error = %v, want ErrStateEdgeBudget", err)
	}
	if !strings.Contains(err.Error(), "3") {
		t.Errorf("the error must name the count: %v", err)
	}
	// Re-writing an edge already recorded is not a new edge and must not trip
	// the budget — otherwise a retry would fail once the budget is reached.
	first := ephemeralEdge(t, r, state, "agent:belief-a")
	if _, written, err := PutOnce(r, first, "planner", 100); err != nil {
		t.Errorf("re-recording an existing edge must not trip the budget: %v", err)
	} else if written {
		t.Error("re-recording an existing collectable edge reported a write")
	}
}
