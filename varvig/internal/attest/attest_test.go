package attest

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/gc"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// testSigner is an in-process Ed25519 signer implementing identity.Signer.
type testSigner struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

func newSigner(t *testing.T) *testSigner {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return &testSigner{priv: priv, pub: pub}
}

func (s *testSigner) Public() ed25519.PublicKey     { return s.pub }
func (s *testSigner) Sign(b []byte) ([]byte, error) { return ed25519.Sign(s.priv, b), nil }

func approve(t *testing.T, s *testSigner, target []byte, strength object.Strength) *object.Object {
	t.Helper()
	obj, err := Sign(s, object.Attestation{
		Target:   target,
		Decision: object.DecisionApprove,
		Strength: strength,
	})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return obj
}

func TestSignVerifyRoundTrip(t *testing.T) {
	s := newSigner(t)
	target := object.NewBlob([]byte("intent")).Encode()[:8] // any bytes as a stand-in id
	obj := approve(t, s, target, object.StrengthStrong)

	pub, a, err := Verify(obj)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !pub.Equal(s.pub) {
		t.Fatal("verify returned a different key")
	}
	if a.Decision != object.DecisionApprove || a.Strength != object.StrengthStrong {
		t.Fatalf("decoded mismatch: %+v", a)
	}
}

// TestTamperedTargetFailsVerification is the tamper test (§7.2): the signature
// covers the target, so mutating it after signing must invalidate the signature.
func TestTamperedTargetFailsVerification(t *testing.T) {
	s := newSigner(t)
	obj := approve(t, s, []byte("original-target"), object.StrengthStrong)

	// Mutate the signed target field in place.
	obj.SetField(1 /* tagAttestTarget */, []byte("swapped-target"))

	if _, _, err := Verify(obj); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("Verify after tamper = %v, want ErrBadSignature", err)
	}
}

// TestWeakDoesNotSatisfyStrong is the no-upgrade guarantee (§7.2): a weak
// attestation cannot satisfy a policy requiring strong, and nothing raises it.
func TestWeakDoesNotSatisfyStrong(t *testing.T) {
	s := newSigner(t)
	weak := []object.Attestation{{Decision: object.DecisionApprove, Strength: object.StrengthWeak}}
	if got := Derive(weak, object.StrengthStrong); got != StatusPending {
		t.Fatalf("weak approval under strong policy = %v, want pending", got)
	}
	// The same attestation satisfies a weak policy.
	if got := Derive(weak, object.StrengthWeak); got != StatusApproved {
		t.Fatalf("weak approval under weak policy = %v, want approved", got)
	}
	// Verify decodes strength faithfully; there is no setter that upgrades it.
	obj := approve(t, s, []byte("t"), object.StrengthWeak)
	if _, a, _ := Verify(obj); a.Strength != object.StrengthWeak {
		t.Fatal("strength changed through the sign/verify path")
	}
}

// TestBridgeCannotMintStrong is the bridge-integrity guarantee (§7.2): a bridge
// key, under any payload it signs, cannot produce a strong or delegated
// attestation; and a keyholder cannot produce a weak one.
func TestBridgeCannotMintStrong(t *testing.T) {
	bridge := newSigner(t)
	human := newSigner(t)
	principals := PrincipalSet{}
	principals.Add(object.Principal{Key: bridge.pub, Kind: object.KindBridge})
	principals.Add(object.Principal{Key: human.pub, Kind: object.KindHuman})

	// A bridge signing a strong attestation directly (a compromised bridge) is
	// rejected at verification, not trusted because the payload said "strong".
	strongFromBridge := approve(t, bridge, []byte("t"), object.StrengthStrong)
	if _, _, err := VerifyWithPrincipal(strongFromBridge, principals); !errors.Is(err, ErrStrengthKind) {
		t.Fatalf("bridge strong = %v, want ErrStrengthKind", err)
	}
	// A bridge producing weak is fine.
	weakFromBridge := approve(t, bridge, []byte("t"), object.StrengthWeak)
	if _, _, err := VerifyWithPrincipal(weakFromBridge, principals); err != nil {
		t.Fatalf("bridge weak = %v, want ok", err)
	}
	// A keyholder producing weak is rejected (weak means "holds no key").
	weakFromHuman := approve(t, human, []byte("t"), object.StrengthWeak)
	if _, _, err := VerifyWithPrincipal(weakFromHuman, principals); !errors.Is(err, ErrStrengthKind) {
		t.Fatalf("human weak = %v, want ErrStrengthKind", err)
	}
	// A keyholder producing strong is fine.
	strongFromHuman := approve(t, human, []byte("t"), object.StrengthStrong)
	if _, _, err := VerifyWithPrincipal(strongFromHuman, principals); err != nil {
		t.Fatalf("human strong = %v, want ok", err)
	}
	// An unknown signer is not accepted at all.
	stranger := newSigner(t)
	strange := approve(t, stranger, []byte("t"), object.StrengthStrong)
	if _, _, err := VerifyWithPrincipal(strange, principals); !errors.Is(err, ErrUnknownPrincipal) {
		t.Fatalf("unknown signer = %v, want ErrUnknownPrincipal", err)
	}
}

// TestApprovalDoesNotSurviveSpecEdit is the single most important test in the
// governance suite (§7.2): an approval binds to the exact intent revision hash,
// so editing the spec — which produces a new hash — leaves the new revision
// with no approval. If this ever passes silently the audit chain is theatre.
func TestApprovalDoesNotSurviveSpecEdit(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	s := newSigner(t)

	// v1: an unmaterialized change (a ticket) with the original spec.
	v1 := object.NewChange(object.Change{Message: "add rate limiting", Timestamp: 1, Author: "director"})
	v1id, err := r.Objects.Put(v1)
	if err != nil {
		t.Fatalf("Put v1: %v", err)
	}
	obj := approve(t, s, v1id, object.StrengthStrong)
	if _, err := Attach(r, obj, "director", 1); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	atts, err := Attestations(r, v1id)
	if err != nil {
		t.Fatalf("Attestations v1: %v", err)
	}
	if got := Derive(atts, object.StrengthStrong); got != StatusApproved {
		t.Fatalf("v1 status = %v, want approved", got)
	}

	// v2: the spec is edited. A new immutable revision, a new hash.
	v2 := object.NewChange(object.Change{Message: "add rate limiting AND audit logging", Timestamp: 2, Author: "director"})
	v2id, err := r.Objects.Put(v2)
	if err != nil {
		t.Fatalf("Put v2: %v", err)
	}
	if v1id.Equal(v2id) {
		t.Fatal("edited spec produced the same hash")
	}
	atts2, err := Attestations(r, v2id)
	if err != nil {
		t.Fatalf("Attestations v2: %v", err)
	}
	if got := Derive(atts2, object.StrengthStrong); got != StatusPending {
		t.Fatalf("v2 status = %v, want pending (approval must not carry forward)", got)
	}
}

// TestVetoBlocksDescendants covers §7.2: a veto on an ancestor revision blocks
// promotion of every descendant, including a descendant created after the veto.
func TestVetoBlocksDescendants(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	s := newSigner(t)

	ancestor := object.NewChange(object.Change{Message: "ancestor", Timestamp: 1, Author: "a"})
	ancID, _ := r.Objects.Put(ancestor)

	// Veto the ancestor.
	veto, err := Sign(s, object.Attestation{Target: ancID, Decision: object.DecisionVeto, Strength: object.StrengthStrong})
	if err != nil {
		t.Fatalf("Sign veto: %v", err)
	}
	if _, err := Attach(r, veto, "a", 1); err != nil {
		t.Fatalf("Attach veto: %v", err)
	}

	// A descendant created AFTER the veto still inherits the block.
	desc := object.NewChange(object.Change{
		Message: "descendant", Timestamp: 2, Author: "a",
		Parents: []multihash.Multihash{ancID},
	})
	descID, _ := r.Objects.Put(desc)

	blocked, at, err := PromotionBlocked(r, descID)
	if err != nil {
		t.Fatalf("PromotionBlocked: %v", err)
	}
	if !blocked || !at.Equal(ancID) {
		t.Fatalf("blocked=%v at=%v, want blocked at ancestor %v", blocked, at, ancID)
	}

	// A sibling with no vetoed ancestor is not blocked.
	clean := object.NewChange(object.Change{Message: "clean", Timestamp: 3, Author: "a"})
	cleanID, _ := r.Objects.Put(clean)
	if b, _, _ := PromotionBlocked(r, cleanID); b {
		t.Fatal("clean change reported blocked")
	}
}

// TestGCRetainsAttestationAndTarget covers §7.2/D4: aggressive GC retains every
// attestation and every attested intent revision.
func TestGCRetainsAttestationAndTarget(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	s := newSigner(t)

	intent := object.NewChange(object.Change{Message: "ticket", Timestamp: 1, Author: "d"})
	intentID, _ := r.Objects.Put(intent)
	obj := approve(t, s, intentID, object.StrengthStrong)
	if _, err := Attach(r, obj, "d", 1); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// Add unrelated garbage that GC should reclaim.
	garbage, _ := r.Objects.Put(object.NewBlob([]byte("garbage")))

	rep, err := gc.Collect(r, nil, false)
	if err != nil {
		t.Fatalf("gc.Collect: %v", err)
	}
	if rep.Deleted == 0 {
		t.Fatal("gc deleted nothing; expected to reclaim the garbage blob")
	}
	if r.Objects.Has(garbage) {
		t.Fatal("gc did not reclaim unreferenced garbage")
	}
	// The intent revision survives (pinned by the attestation note's target).
	if !r.Objects.Has(intentID) {
		t.Fatal("gc reclaimed the attested intent revision")
	}
	// The attestation itself survives and still verifies.
	atts, err := Attestations(r, intentID)
	if err != nil || len(atts) != 1 {
		t.Fatalf("attestations after gc = %v (err %v), want 1", atts, err)
	}
}

// TestVetoGateAdmit exercises the promotion gate directly (tickets M1): a change
// with a vetoed ancestor is refused with ErrVetoed; a clean one is admitted.
func TestVetoGateAdmit(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	s := newSigner(t)

	anc := object.NewChange(object.Change{Message: "anc", Timestamp: 1, Author: "a"})
	ancID, _ := r.Objects.Put(anc)
	veto, _ := Sign(s, object.Attestation{Target: ancID, Decision: object.DecisionVeto, Strength: object.StrengthStrong})
	if _, err := Attach(r, veto, "a", 1); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	child := object.NewChange(object.Change{Message: "child", Timestamp: 2, Author: "a", Parents: []multihash.Multihash{ancID}})
	childID, _ := r.Objects.Put(child)

	if err := (VetoGate{}).Admit(r, childID); !errors.Is(err, ErrVetoed) {
		t.Fatalf("VetoGate.Admit(vetoed descendant) = %v, want ErrVetoed", err)
	}
	clean := object.NewChange(object.Change{Message: "clean", Timestamp: 3, Author: "a"})
	cleanID, _ := r.Objects.Put(clean)
	if err := (VetoGate{}).Admit(r, cleanID); err != nil {
		t.Fatalf("VetoGate.Admit(clean) = %v, want nil", err)
	}
}

// TestApprovalGateAdmit: an ApprovalGate requiring strong admits only a change
// that derives to approved at strong, and still enforces the veto rule.
func TestApprovalGateAdmit(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	s := newSigner(t)
	gate := ApprovalGate{Required: object.StrengthStrong}

	// Unapproved change is refused.
	c := object.NewChange(object.Change{Message: "needs approval", Timestamp: 1, Author: "d"})
	cID, _ := r.Objects.Put(c)
	if err := gate.Admit(r, cID); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("Admit(unapproved) = %v, want ErrNotApproved", err)
	}
	// A weak approval does not satisfy a strong gate.
	weak, _ := Sign(s, object.Attestation{Target: cID, Decision: object.DecisionApprove, Strength: object.StrengthWeak})
	if _, err := Attach(r, weak, "b", 1); err != nil {
		t.Fatalf("Attach weak: %v", err)
	}
	if err := gate.Admit(r, cID); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("Admit(weak under strong) = %v, want ErrNotApproved", err)
	}
	// A strong approval admits it.
	strong, _ := Sign(s, object.Attestation{Target: cID, Decision: object.DecisionApprove, Strength: object.StrengthStrong})
	if _, err := Attach(r, strong, "d", 2); err != nil {
		t.Fatalf("Attach strong: %v", err)
	}
	if err := gate.Admit(r, cID); err != nil {
		t.Fatalf("Admit(strong) = %v, want nil", err)
	}
}
