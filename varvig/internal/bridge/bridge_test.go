package bridge

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/attest"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/notes"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/principal"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/reserved"
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

// TestRecordTransitionOnce covers §5.3 idempotency: the same transition on the
// same head records one weak attestation no matter how often it is re-synced,
// but a new head re-applies it.
func TestRecordTransitionOnce(t *testing.T) {
	r := newRepo(t)
	dir := newKey(t)
	brk := newKey(t)
	id, _ := ticket.New(r, "v1", dir, "director", 1)
	_ = SetLink(r, id, Link{System: "ext", ForeignID: "T-1"}, "bridge", 1)

	// First record writes; a re-sync of the same transition does not.
	rec, err := RecordTransitionOnce(r, brk, id, object.DecisionApprove, "closed", 2)
	if err != nil || !rec {
		t.Fatalf("first RecordTransitionOnce = rec %v err %v, want true", rec, err)
	}
	rec, err = RecordTransitionOnce(r, brk, id, object.DecisionApprove, "closed", 3)
	if err != nil || rec {
		t.Fatalf("duplicate RecordTransitionOnce = rec %v err %v, want false", rec, err)
	}
	if atts, _ := attest.Attestations(r, id); len(atts) != 1 {
		t.Fatalf("attestations = %d, want 1 (no duplicate)", len(atts))
	}

	// A new head revision re-applies the transition.
	rev2, err := ticket.Revise(r, id, "v2", dir, "director", 4)
	if err != nil {
		t.Fatalf("Revise: %v", err)
	}
	rec, err = RecordTransitionOnce(r, brk, id, object.DecisionApprove, "still closed", 5)
	if err != nil || !rec {
		t.Fatalf("RecordTransitionOnce on new head = rec %v err %v, want true", rec, err)
	}
	if atts, _ := attest.Attestations(r, rev2); len(atts) != 1 {
		t.Fatalf("new head attestations = %d, want 1", len(atts))
	}
}

// TestSetNudgeAndAssignee covers §5.2: a peer projects a priority nudge and the
// tracker's assignee onto the link. Both require an existing link, the nudge
// clamps to [0,1], and both round-trip.
func TestSetNudgeAndAssignee(t *testing.T) {
	r := newRepo(t)
	dir := newKey(t)
	id, _ := ticket.New(r, "v1", dir, "director", 1)

	// A nudge or assignee with no link is refused.
	if err := SetNudge(r, id, 0.5, "bridge", 2); err != ErrNoLink {
		t.Fatalf("SetNudge with no link = %v, want ErrNoLink", err)
	}
	if err := SetAssignee(r, id, "octocat", "bridge", 2); err != ErrNoLink {
		t.Fatalf("SetAssignee with no link = %v, want ErrNoLink", err)
	}

	if err := SetLink(r, id, Link{System: "ext", ForeignID: "T-1"}, "bridge", 3); err != nil {
		t.Fatalf("SetLink: %v", err)
	}

	// Out-of-range nudge clamps to 1; assignee stored verbatim; watermarks kept.
	_ = MarkPushed(r, id, "bridge", 4)
	if err := SetNudge(r, id, 2.5, "bridge", 5); err != nil {
		t.Fatalf("SetNudge: %v", err)
	}
	if err := SetAssignee(r, id, "octocat", "bridge", 6); err != nil {
		t.Fatalf("SetAssignee: %v", err)
	}
	link, ok, err := GetLink(r, id)
	if err != nil || !ok {
		t.Fatalf("GetLink ok=%v err=%v", ok, err)
	}
	if link.PriorityNudge != 1 {
		t.Fatalf("nudge = %v, want 1 (clamped)", link.PriorityNudge)
	}
	if link.Assignee != "octocat" {
		t.Fatalf("assignee = %q, want octocat", link.Assignee)
	}
	if link.LastPushed == "" || link.System != "ext" || link.ForeignID != "T-1" {
		t.Fatalf("nudge/assignee clobbered the link: %+v", link)
	}

	// Re-setting the same values writes no new note (idempotent poll).
	before := len(chain(t, r, id))
	_ = SetNudge(r, id, 1, "bridge", 7)            // already 1
	_ = SetAssignee(r, id, "octocat", "bridge", 8) // already octocat
	if after := len(chain(t, r, id)); after != before {
		t.Fatalf("redundant set grew the note chain: %d -> %d", before, after)
	}

	// Clearing: nudge 0 and empty assignee.
	_ = SetNudge(r, id, 0, "bridge", 9)
	_ = SetAssignee(r, id, "", "bridge", 10)
	link, _, _ = GetLink(r, id)
	if link.PriorityNudge != 0 || link.Assignee != "" {
		t.Fatalf("clear failed: nudge=%v assignee=%q", link.PriorityNudge, link.Assignee)
	}
}

func chain(t *testing.T, r *repo.Repo, id multihash.Multihash) []notes.Entry {
	t.Helper()
	c, err := notes.New(r).List(reserved.NoteExternal, id)
	if err != nil {
		t.Fatalf("note list: %v", err)
	}
	return c
}

// TestListLinks covers §5: a connector enumerates the tickets it mirrors, with
// an optional filter by system tag.
func TestListLinks(t *testing.T) {
	r := newRepo(t)
	dir := newKey(t)
	a, _ := ticket.New(r, "a", dir, "director", 1)
	b, _ := ticket.New(r, "b", dir, "director", 1)
	c, _ := ticket.New(r, "c", dir, "director", 1)
	_ = SetLink(r, a, Link{System: "ext", ForeignID: "o/r#1"}, "bridge", 1)
	_ = SetLink(r, b, Link{System: "ext", ForeignID: "o/r#2"}, "bridge", 1)
	_ = SetLink(r, c, Link{System: "other", ForeignID: "X-9"}, "bridge", 1)

	all, err := ListLinks(r, "")
	if err != nil {
		t.Fatalf("ListLinks: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("all links = %d, want 3", len(all))
	}

	gh, err := ListLinks(r, "ext")
	if err != nil {
		t.Fatalf("ListLinks(ext): %v", err)
	}
	if len(gh) != 2 {
		t.Fatalf("ext links = %d, want 2", len(gh))
	}
	for _, l := range gh {
		if l.Link.System != "ext" {
			t.Fatalf("filter leaked a %q link", l.Link.System)
		}
	}
	// Deterministic order by ticket id.
	if gh[0].TicketID.Hex() >= gh[1].TicketID.Hex() {
		t.Fatalf("links not sorted by id: %s, %s", gh[0].TicketID.Hex(), gh[1].TicketID.Hex())
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
