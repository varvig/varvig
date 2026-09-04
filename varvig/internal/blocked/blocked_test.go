package blocked

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

type testSigner struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

func newSigner(t *testing.T) *testSigner {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &testSigner{priv: priv, pub: pub}
}

func (s *testSigner) Public() ed25519.PublicKey     { return s.pub }
func (s *testSigner) Sign(b []byte) ([]byte, error) { return ed25519.Sign(s.priv, b), nil }

// intentRev stores a stand-in intent revision object and returns its hash.
func intentRev(t *testing.T, r *repo.Repo) multihash.Multihash {
	t.Helper()
	id, err := r.Objects.Put(object.NewBlob([]byte("intent revision v1")))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestSixHitsOneReport is the spec's headline test: six boundary hits produce
// one blocked-on-scope outcome carrying all six, not six failures.
func TestSixHitsOneReport(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	intent := intentRev(t, r)
	s := newSigner(t)

	hits := []Hit{}
	for _, p := range []string{"src/db", "src/api", "src/web", "config", "infra", "scripts"} {
		hits = append(hits, Hit{Path: p, Reason: "needed to complete the change"})
	}
	rep := Report{
		Intent: intent.Hex(), Scope: "src/auth", Need: "src/db",
		Why: "the auth change needs a schema migration", Unmet: "write to src/db",
		Hits: hits, Author: "task:abc", Timestamp: 100,
	}
	if _, err := Attach(r, s, rep); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	got, err := List(r, intent)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d reports, want exactly one aggregated outcome", len(got))
	}
	if len(got[0].Hits) != 6 {
		t.Fatalf("the one report carries %d hits, want all six", len(got[0].Hits))
	}
	if got[0].Scope != "src/auth" || got[0].Need != "src/db" {
		t.Fatalf("report lost its declared scope or requested need: %+v", got[0])
	}
}

// TestSignatureIsAuthority: a tampered report does not verify and is skipped by
// List — the note payload is not authority, the signature is.
func TestSignatureIsAuthority(t *testing.T) {
	s := newSigner(t)
	rep := Report{Intent: "abcd", Scope: "a", Need: "b", Author: "t", Timestamp: 1}
	payload, err := SignReport(s, rep)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := VerifyReport(payload); err != nil {
		t.Fatalf("clean payload must verify: %v", err)
	}
	// Flip a byte in the signed body: verification must now fail.
	tampered := make([]byte, len(payload))
	copy(tampered, payload)
	for i := range tampered {
		if tampered[i] == 'a' { // the scope value
			tampered[i] = 'z'
			break
		}
	}
	if _, _, err := VerifyReport(tampered); err == nil {
		t.Fatal("a tampered report must not verify")
	}
}

// TestWideningShowsBothDeclarations: widening scope and resuming produces a trace
// whose provenance shows both the original declaration and the widening decision.
func TestWideningShowsBothDeclarations(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	intent := intentRev(t, r)
	task := newSigner(t)
	authority := newSigner(t)

	// The task reports it is blocked, declaring the scope it ran under.
	if _, err := Attach(r, task, Report{
		Intent: intent.Hex(), Scope: "src/auth", Need: "src/db",
		Why: "schema migration", Unmet: "write to src/db",
		Hits: []Hit{{Path: "src/db"}}, Author: "task:abc", Timestamp: 100,
	}); err != nil {
		t.Fatal(err)
	}
	// The authority widens it — a separate, authored decision.
	if _, err := Widen(r, authority, Widening{
		Intent: intent.Hex(), FromScope: "src/auth", ToScope: "src/auth,src/db",
		Decider: "director", Reason: "migration approved", Timestamp: 200,
	}); err != nil {
		t.Fatal(err)
	}

	tr, err := Provenance(r, intent)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Requests) != 1 || len(tr.Widenings) != 1 {
		t.Fatalf("trace = %d requests, %d widenings; want one of each", len(tr.Requests), len(tr.Widenings))
	}
	// Both declarations are visible: the original scope, and the scope widened to.
	if tr.Requests[0].Scope != "src/auth" {
		t.Errorf("original declaration missing from trace: %q", tr.Requests[0].Scope)
	}
	if tr.Widenings[0].FromScope != "src/auth" || tr.Widenings[0].ToScope != "src/auth,src/db" {
		t.Errorf("widening decision missing from trace: %+v", tr.Widenings[0])
	}
}

// TestKindsDoNotCross: a widening does not decode as a request, and vice versa,
// so List and Widenings each return only their own kind.
func TestKindsDoNotCross(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	intent := intentRev(t, r)
	s := newSigner(t)
	if _, err := Attach(r, s, Report{Intent: intent.Hex(), Scope: "a", Need: "b", Author: "t", Timestamp: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := Widen(r, s, Widening{Intent: intent.Hex(), FromScope: "a", ToScope: "a,b", Decider: "d", Timestamp: 2}); err != nil {
		t.Fatal(err)
	}
	reqs, _ := List(r, intent)
	wids, _ := Widenings(r, intent)
	if len(reqs) != 1 {
		t.Errorf("List returned %d, want only the request", len(reqs))
	}
	if len(wids) != 1 {
		t.Errorf("Widenings returned %d, want only the widening", len(wids))
	}
}
