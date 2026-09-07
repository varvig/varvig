package assertion

import (
	"crypto/ed25519"
	"crypto/rand"
	"reflect"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/core"

	"github.com/dividebyzero/claude-experiments/varvig/internal/deps"
	"github.com/dividebyzero/claude-experiments/varvig/internal/edge"
	"github.com/dividebyzero/claude-experiments/varvig/internal/graphnode"
	"github.com/dividebyzero/claude-experiments/varvig/internal/graphquery"
	"github.com/dividebyzero/claude-experiments/varvig/internal/identity"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

type keySigner ed25519.PrivateKey

func (k keySigner) Public() ed25519.PublicKey {
	return ed25519.PrivateKey(k).Public().(ed25519.PublicKey)
}
func (k keySigner) Sign(b []byte) ([]byte, error) {
	return ed25519.Sign(ed25519.PrivateKey(k), b), nil
}

func newSigner(t *testing.T) identity.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return keySigner(priv)
}

func newRepo(t *testing.T) *repo.Repo {
	t.Helper()
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func blob(t *testing.T, r *repo.Repo, s string) multihash.Multihash {
	t.Helper()
	id, err := r.Objects.Put(object.NewBlob([]byte(s)))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func nodes(t *testing.T, r *repo.Repo, a, b string) (graphnode.Node, graphnode.Node) {
	t.Helper()
	x, err := graphnode.Object(blob(t, r, a))
	if err != nil {
		t.Fatal(err)
	}
	y, err := graphnode.Object(blob(t, r, b))
	if err != nil {
		t.Fatal(err)
	}
	return x, y
}

func claim(t *testing.T, r *repo.Repo) Claim {
	t.Helper()
	src, tgt := nodes(t, r, "module a", "module b")
	return Claim{
		Source: src, Target: tgt,
		Type:          "agent:couples-with",
		ObservedUnder: blob(t, r, "tree"),
		Principal:     "planner-agent",
		Strength:      object.StrengthDelegated,
	}
}

// TestInferenceProvenanceIsRequired: an assertion a model produced must name the
// model, its version, and its sampling parameters. "Some agent said so" is not
// provenance, and it is the only thing standing in for a confidence score.
func TestInferenceProvenanceIsRequired(t *testing.T) {
	r := newRepo(t)
	c := claim(t, r)
	for _, inf := range []Inference{
		{},
		{Model: "m"},
		{Model: "m", ModelVersion: "v"},
		{ModelVersion: "v", Sampling: "s"},
	} {
		if _, err := FromInference(r, c, inf, 100); err == nil {
			t.Errorf("FromInference accepted incomplete provenance %+v", inf)
		}
	}
	if _, err := FromInference(r, c, Inference{Model: "m", ModelVersion: "v", Sampling: "temp=0"}, 100); err != nil {
		t.Fatal(err)
	}
	// Read it back off the anchor its source node resolves to, rather than
	// rebuilding it: what matters is what actually landed in the store.
	anchor := c.Source.(graphnode.ObjectNode).ID()
	stored, err := edge.List(r, anchor)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 {
		t.Fatalf("got %d stored edges on the anchor, want 1", len(stored))
	}
	p := stored[0].Edge.Provenance()
	if p.Class != edge.Asserted {
		t.Errorf("class = %s, want asserted", p.Class)
	}
	if p.Model != "m" || p.ModelVersion != "v" || p.Sampling != "temp=0" {
		t.Errorf("model provenance not stored: %+v", p)
	}
}

// TestNoConfidenceScores: the claim and provenance types must offer nowhere to
// put a float. A score is unfalsifiable and immediately Goodharted, and the
// checkable thing — what produced this — is already recorded.
func TestNoConfidenceScores(t *testing.T) {
	// Compile-time by construction: neither Claim nor edge.Provenance has a
	// numeric field. This asserts it at runtime too, so a field added later
	// without reading the design fails here.
	forbidden := []string{"Confidence", "Score", "Probability", "Weight", "Certainty"}
	for _, name := range forbidden {
		if hasField(Claim{}, name) {
			t.Errorf("Claim has a %s field; provenance answers this, a number does not", name)
		}
		if hasField(edge.Provenance{}, name) {
			t.Errorf("edge.Provenance has a %s field; provenance answers this, a number does not", name)
		}
	}
}

// TestPromotionIsSignedAndNamesTheEdgeHash is G7's acceptance: a promoted
// assertion carries an attestation naming the edge's hash.
func TestPromotionIsSignedAndNamesTheEdgeHash(t *testing.T) {
	r := newRepo(t)
	signer := newSigner(t)
	noteID, err := FromPrincipal(r, claim(t, r), 100)
	if err != nil {
		t.Fatal(err)
	}

	// Unpromoted to begin with.
	promoted, _, err := Promoted(r, noteID)
	if err != nil {
		t.Fatal(err)
	}
	if promoted {
		t.Fatal("a fresh assertion must not be promoted; nothing promotes implicitly")
	}

	if _, err := Promote(r, noteID, signer, object.StrengthStrong, "reviewed the coupling", 200); err != nil {
		t.Fatal(err)
	}
	promoted, approvals, err := Promoted(r, noteID)
	if err != nil {
		t.Fatal(err)
	}
	if !promoted || len(approvals) != 1 {
		t.Fatalf("promotion did not take: promoted=%v approvals=%d", promoted, len(approvals))
	}
	if !approvals[0].Target.Equal(noteID) {
		t.Errorf("the attestation names %s, want the edge note %s",
			approvals[0].Target.Hex(), noteID.Hex())
	}
	if approvals[0].Strength != object.StrengthStrong {
		t.Errorf("strength = %s, want strong", approvals[0].Strength)
	}

	// A different assertion between the same endpoints is a different edge and
	// does not inherit the approval — A4, the same reason a ticket approval does
	// not carry across a revision.
	other := claim(t, r)
	other.Type = "agent:duplicates"
	otherID, err := FromPrincipal(r, other, 300)
	if err != nil {
		t.Fatal(err)
	}
	if otherID.Equal(noteID) {
		t.Fatal("the two assertions collided; the test would prove nothing")
	}
	promoted, _, err = Promoted(r, otherID)
	if err != nil {
		t.Fatal(err)
	}
	if promoted {
		t.Error("an approval carried forward to a different edge between the same endpoints")
	}
}

// TestAnAssertionCannotChangeAConflictOutcome is the acceptance the design cares
// most about, constructed so that it *would* change the answer if the boundary
// leaked.
//
// Two tickets hold disjoint scopes, so they do not block each other. An agent
// then asserts, at the strongest authority it can, that the two areas are
// coupled — exactly the claim that would make them conflict if anything in
// scheduling consulted assertions. The answer must not move.
func TestAnAssertionCannotChangeAConflictOutcome(t *testing.T) {
	r := newRepo(t)
	a := deps.Scope{Reads: []string{"src/auth"}, Writes: []string{"src/auth"}}
	b := deps.Scope{Reads: []string{"src/web"}, Writes: []string{"src/web"}}

	if deps.Blocks(a, b) {
		t.Fatal("the fixture scopes already conflict; the test would prove nothing")
	}

	src, tgt := nodes(t, r, "src/auth contents", "src/web contents")
	if _, err := FromInference(r, Claim{
		Source: src, Target: tgt,
		Type:          "agent:couples-with",
		ObservedUnder: blob(t, r, "tree"),
		Principal:     "planner-agent",
		Strength:      object.StrengthStrong, // the strongest a claim can carry
	}, Inference{Model: "m", ModelVersion: "v", Sampling: "temp=0"}, 100); err != nil {
		t.Fatal(err)
	}

	// The scheduler's answer is unchanged: scope conflict is computed from
	// declared scope, and an assertion is not an input to it.
	if deps.Blocks(a, b) {
		t.Fatal("an asserted coupling changed a conflict-detection outcome")
	}

	// Promoting it does not change that either. Promotion makes an assertion
	// durable knowledge; it does not make it reproducible, and only a
	// reproducible edge may gate (§11.3).
	if !graphquery.ClassDerived.Reproducible() {
		t.Fatal("a derived edge must be reproducible")
	}
	if graphquery.ClassAsserted.Reproducible() {
		t.Fatal("an asserted edge must never be reproducible, promoted or not")
	}
}

// TestGatingConsumersSeeOnlyDerivedEdges: the query surface hands a gating
// consumer derived edges and nothing else, so an assertion cannot reach one even
// by accident. The type is the refusal — a consumer taking []edge.DerivedEdge
// cannot be handed a StoredEdge at all.
func TestGatingConsumersSeeOnlyDerivedEdges(t *testing.T) {
	r := newRepo(t)
	tree := treeWithImport(t, r)

	// An assertion anchored on the same file the derived edge starts from.
	files, err := treeFilesOf(t, r, tree)
	if err != nil {
		t.Fatal(err)
	}
	src, err := graphnode.Object(files["a.js"])
	if err != nil {
		t.Fatal(err)
	}
	far, err := graphnode.External("agent", "belief/1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FromPrincipal(r, Claim{
		Source: src, Target: far, Type: "agent:couples-with",
		ObservedUnder: files["a.js"], Principal: "planner", Strength: object.StrengthStrong,
	}, 100); err != nil {
		t.Fatal(err)
	}

	q, err := graphquery.New(r, tree)
	if err != nil {
		t.Fatal(err)
	}
	res, err := q.Dependencies("a.js")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Asserted()) != 1 {
		t.Fatalf("the assertion is not visible to a planner: %d asserted", len(res.Asserted()))
	}
	// It informs planning — and it is absent from the gating set.
	gating := res.GatingEdges()
	if len(gating) != 1 {
		t.Fatalf("gating set has %d edges, want just the derived one", len(gating))
	}
	for _, e := range gating {
		if e.Type() != "varvig:imports-file" {
			t.Errorf("a non-derived edge reached the gating set: %s", e.Type())
		}
	}
}

// hasField reports whether a struct type has a field with the given name.
func hasField(v any, name string) bool {
	rt := reflect.TypeOf(v)
	if rt.Kind() != reflect.Struct {
		return false
	}
	_, ok := rt.FieldByName(name)
	return ok
}

// treeWithImport stores a two-file tree where a.js imports b.js, so a derived
// edge exists to compare an assertion against.
func treeWithImport(t *testing.T, r *repo.Repo) multihash.Multihash {
	t.Helper()
	entries := []object.Entry{
		{Name: "a.js", Mode: 0o100644, Kind: object.TypeBlob, ID: blob(t, r, "import './b.js'\n")},
		{Name: "b.js", Mode: 0o100644, Kind: object.TypeBlob, ID: blob(t, r, "export const b = 1\n")},
	}
	tree, err := r.Objects.Put(object.NewTree(entries))
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func treeFilesOf(t *testing.T, r *repo.Repo, tree multihash.Multihash) (map[string]multihash.Multihash, error) {
	t.Helper()
	return core.TreeFiles(r, tree)
}
