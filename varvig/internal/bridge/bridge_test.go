package bridge

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/attest"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/principal"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/ticket"
)

func newRepo(t *testing.T) *repo.Repo {
	t.Helper()
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	return r
}

func newKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return priv
}

func fp(priv ed25519.PrivateKey) string {
	return attest.Fingerprint(priv.Public().(ed25519.PublicKey))
}

// TestRoundTripSuppressesEcho covers §5.4/§7.5: varvig → tracker → varvig
// produces no new intent revision.
func TestRoundTripSuppressesEcho(t *testing.T) {
	r := newRepo(t)
	dir := newKey(t)
	brk := newKey(t)

	id, err := ticket.New(r, "the spec", dir, "director", 1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := SetLink(r, id, Link{System: "ext", ForeignID: "T-1"}, "bridge", 1); err != nil {
		t.Fatalf("SetLink: %v", err)
	}
	// Push the head out to the tracker.
	if err := MarkPushed(r, id, "bridge", 2); err != nil {
		t.Fatalf("MarkPushed: %v", err)
	}
	if need, _ := NeedsPush(r, id); need {
		t.Fatal("NeedsPush true right after MarkPushed")
	}
	// The tracker echoes the same spec back inbound: no new revision.
	head, changed, err := ApplyInbound(r, id, "the spec", brk, "ext:alice", 3)
	if err != nil {
		t.Fatalf("ApplyInbound: %v", err)
	}
	if changed || !head.Equal(id) {
		t.Fatalf("echo created a revision: changed=%v head=%s", changed, head.Hex())
	}
}

// TestInboundEditRevisesAndDropsApproval covers §5.3/§2.2: an inbound spec edit
// becomes a new revision authored by the bridge principal, and an approval on
// the prior revision does not carry to it.
func TestInboundEditRevisesAndDropsApproval(t *testing.T) {
	r := newRepo(t)
	dir := newKey(t)
	brk := newKey(t)

	id, _ := ticket.New(r, "v1", dir, "director", 1)
	// Approve v1.
	ap, _ := attest.Sign(keySigner(dir), object.Attestation{Target: id, Decision: object.DecisionApprove, Strength: object.StrengthStrong})
	if _, err := attest.Attach(r, ap, "director", 1); err != nil {
		t.Fatalf("Attach approve: %v", err)
	}
	if atts, _ := attest.Attestations(r, id); attest.Derive(atts, object.StrengthStrong) != attest.StatusApproved {
		t.Fatal("v1 should be approved")
	}
	_ = MarkPushed(r, id, "bridge", 2)

	// Inbound edit from the tracker.
	rev, changed, err := ApplyInbound(r, id, "v2 edited in tracker", brk, "ext:alice", 3)
	if err != nil || !changed {
		t.Fatalf("ApplyInbound: changed=%v err=%v", changed, err)
	}
	if rev.Equal(id) {
		t.Fatal("inbound edit did not create a new revision")
	}
	// The new revision is authored by the bridge principal and is unapproved.
	obj, _ := r.Objects.Get(rev)
	c, _ := obj.AsChange()
	if c.Author != "ext:alice" {
		t.Fatalf("revision author = %q, want ext:alice", c.Author)
	}
	if atts, _ := attest.Attestations(r, rev); attest.Derive(atts, object.StrengthStrong) != attest.StatusPending {
		t.Fatal("approval carried across an inbound spec edit — must not")
	}
}

// TestTrackerLoses covers §7.5: on concurrent edits, the local ticket wins and
// the inbound tracker edit is dropped.
func TestTrackerLoses(t *testing.T) {
	r := newRepo(t)
	dir := newKey(t)
	brk := newKey(t)

	id, _ := ticket.New(r, "v1", dir, "director", 1)
	_ = MarkPushed(r, id, "bridge", 2)
	// Local edit advances the head past what was pushed.
	if _, err := ticket.Revise(r, id, "v2 local", dir, "director", 3); err != nil {
		t.Fatalf("local Revise: %v", err)
	}
	// A conflicting inbound edit is dropped.
	_, changed, err := ApplyInbound(r, id, "v2 tracker", brk, "ext:alice", 4)
	if !errors.Is(err, ErrLocalWins) {
		t.Fatalf("ApplyInbound = changed=%v err=%v, want ErrLocalWins", changed, err)
	}
	if info, _ := ticket.Get(r, id); info.Spec != "v2 local" {
		t.Fatalf("local edit was clobbered: head spec = %q", info.Spec)
	}
}

// TestTransitionIsWeak covers §5.3/§7.5: a workflow transition is only ever a
// weak attestation, and weak cannot satisfy a strong policy.
func TestTransitionIsWeak(t *testing.T) {
	r := newRepo(t)
	dir := newKey(t)
	brk := newKey(t)
	id, _ := ticket.New(r, "spec", dir, "director", 1)

	if _, err := RecordTransition(r, brk, id, object.DecisionApprove, "moved to Done", 2); err != nil {
		t.Fatalf("RecordTransition: %v", err)
	}
	atts, _ := attest.Attestations(r, id)
	if len(atts) != 1 || atts[0].Strength != object.StrengthWeak {
		t.Fatalf("transition strength = %+v, want one weak", atts)
	}
	// A weak transition does not approve under a strong policy.
	if attest.Derive(atts, object.StrengthStrong) == attest.StatusApproved {
		t.Fatal("a weak workflow transition satisfied a strong policy")
	}
	// It does count under a weak policy (it is a real, if weak, signal).
	if attest.Derive(atts, object.StrengthWeak) != attest.StatusApproved {
		t.Fatal("weak transition did not register even under a weak policy")
	}
}

// TestBridgeCannotMintStrong covers §7.5: with the bridge registered as
// kind=bridge, nothing it signs can be a strong decision — even RecordTransition
// is capped, and a direct attempt to sign strong is refused at verification.
func TestBridgeCannotMintStrong(t *testing.T) {
	r := newRepo(t)
	reg := principal.Open(r)
	brk := newKey(t)
	_ = reg.Add(object.Principal{Key: brk.Public().(ed25519.PublicKey), Name: "ext-tracker", Kind: object.KindBridge}, "admin", 1)

	target := []byte("intent")
	// A malicious bridge signs a strong attestation directly.
	strong, _ := attest.Sign(keySigner(brk), object.Attestation{Target: target, Decision: object.DecisionApprove, Strength: object.StrengthStrong})
	if _, _, err := attest.VerifyWithPrincipal(strong, reg); !errors.Is(err, attest.ErrStrengthKind) {
		t.Fatalf("bridge strong via registry = %v, want ErrStrengthKind", err)
	}
	// Its weak transition is accepted.
	weak, _ := attest.Sign(keySigner(brk), object.Attestation{Target: target, Decision: object.DecisionApprove, Strength: object.StrengthWeak})
	if _, _, err := attest.VerifyWithPrincipal(weak, reg); err != nil {
		t.Fatalf("bridge weak via registry = %v, want ok", err)
	}
}

// TestBridgeCannotForgePrincipal covers §7.5: the verified signer of anything
// the bridge writes is always the bridge's own key, regardless of any label,
// so it cannot impersonate a director.
func TestBridgeCannotForgePrincipal(t *testing.T) {
	r := newRepo(t)
	dir := newKey(t)
	brk := newKey(t)
	id, _ := ticket.New(r, "spec", dir, "director", 1)

	// The bridge records a transition, passing a director-looking author label.
	if _, err := RecordTransition(r, brk, id, object.DecisionApprove, "", 2); err != nil {
		t.Fatalf("RecordTransition: %v", err)
	}
	entries, err := attest.List(r, id)
	if err != nil || len(entries) != 1 {
		t.Fatalf("List = %d err=%v", len(entries), err)
	}
	got := attest.Fingerprint(entries[0].SignerKey)
	if got != fp(brk) {
		t.Fatalf("verified signer = %s, want the bridge key %s", got, fp(brk))
	}
	if got == fp(dir) {
		t.Fatal("bridge attestation resolved to the director's identity — forgery")
	}
}

// TestBridgeCannotDeleteVeto covers §7.5: a veto a director places survives
// anything the bridge does; an inbound edit cannot erase it, and the veto still
// blocks promotion of the descendant it produced.
func TestBridgeCannotDeleteVeto(t *testing.T) {
	r := newRepo(t)
	dir := newKey(t)
	brk := newKey(t)
	id, _ := ticket.New(r, "v1", dir, "director", 1)

	// Director vetoes v1.
	veto, _ := attest.Sign(keySigner(dir), object.Attestation{Target: id, Decision: object.DecisionVeto, Strength: object.StrengthStrong})
	if _, err := attest.Attach(r, veto, "director", 1); err != nil {
		t.Fatalf("Attach veto: %v", err)
	}
	_ = MarkPushed(r, id, "bridge", 2)

	// The bridge applies an inbound edit — the only mutation it can make.
	rev, changed, err := ApplyInbound(r, id, "v2 from tracker", brk, "ext:alice", 3)
	if err != nil || !changed {
		t.Fatalf("ApplyInbound: changed=%v err=%v", changed, err)
	}
	// The veto on v1 still stands...
	if atts, _ := attest.Attestations(r, id); attest.Derive(atts, object.StrengthStrong) != attest.StatusVetoed {
		t.Fatal("the veto was erased by the bridge")
	}
	// ...and it still blocks the descendant the bridge just created.
	blocked, at, err := attest.PromotionBlocked(r, rev)
	if err != nil {
		t.Fatalf("PromotionBlocked: %v", err)
	}
	if !blocked || !at.Equal(id) {
		t.Fatalf("veto no longer blocks the descendant: blocked=%v at=%v", blocked, at)
	}
}
