// Package bridge is the vendor-neutral seam an external-tracker peer builds on
// (tickets §5). It carries no knowledge of any specific tracker: the connector
// — the thing that talks to a real tracker's API — is a separate peer holding a
// bridge-kind key (§5.1, DESIGN.md §3.1/§3.3), and everything here speaks only
// in generic terms so the core never learns a vendor's name.
//
// Concretely the seam provides three things:
//
//   - an external link (an opaque `system` tag chosen by the peer, plus a
//     foreign id) and per-direction sync watermarks, stored as a note in the
//     reserved varvig/external namespace (§5.1);
//   - echo suppression built on those watermarks, so a varvig → tracker →
//     varvig round-trip produces no new intent revision (§5.4);
//   - inbound application and workflow transitions expressed through the core's
//     existing primitives, so the connector inherits the core's guarantees: an
//     inbound spec edit is an ordinary ticket revision (approvals do not carry,
//     §2.2), and a workflow transition is only ever a weak attestation, which a
//     bridge-kind key is the only thing that can sign (§2.4).
//
// The connector is untrusted. It cannot mint a strong decision, forge a
// principal, or delete a veto — those are capped by the core (§7.5) — so it can
// run anywhere.
package bridge

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/attest"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/notes"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/reserved"
	"github.com/dividebyzero/claude-experiments/varvig/internal/ticket"
)

// keySigner adapts an Ed25519 private key to identity.Signer, so the same key
// signs both ticket revisions (ticket.Revise) and attestations (attest.Sign).
type keySigner ed25519.PrivateKey

func (k keySigner) Public() ed25519.PublicKey {
	return ed25519.PrivateKey(k).Public().(ed25519.PublicKey)
}
func (k keySigner) Sign(data []byte) ([]byte, error) {
	return ed25519.Sign(ed25519.PrivateKey(k), data), nil
}

// ErrLocalWins reports that an inbound tracker edit was dropped because the
// local ticket advanced since it was last pushed. On a concurrent edit the
// tracker loses, deterministically (§7.5).
var ErrLocalWins = errors.New("bridge: local ticket changed since last push; tracker edit dropped")

// Link is a ticket's binding to a row in some external system. System is an
// opaque tag the peer chooses (the core neither enumerates nor branches on it);
// ForeignID is the row's id over there. LastPushed and LastPulled are the sync
// watermarks that suppress echo: the ticket revision last sent out, and the
// content hash of the inbound spec last applied.
type Link struct {
	System     string `json:"system"`
	ForeignID  string `json:"foreign_id"`
	LastPushed string `json:"last_pushed,omitempty"`
	LastPulled string `json:"last_pulled,omitempty"`
	// LastTransition and LastTransitionHead are the transition watermark: the
	// decision last recorded and the head it was recorded on. A workflow
	// transition maps to a weak attestation, and a tracker reports the same
	// state on every poll, so without this a re-sync would append a duplicate
	// attestation each time. Keyed on the head too, so a new revision correctly
	// re-applies the transition.
	LastTransition     string `json:"last_transition,omitempty"`
	LastTransitionHead string `json:"last_transition_head,omitempty"`
	// PriorityNudge is an external priority signal in [0,1] the peer projects
	// from the tracker (§5.2). It is only ever a scoring *input* — a nudge, never
	// authoritative: the scorer decides how much to trust it (its weight starts
	// at zero and is learned from recorded decisions), and a governance refusal
	// can never be outranked (§7.4). Zero means unset.
	PriorityNudge float64 `json:"priority_nudge,omitempty"`
	// Assignee mirrors the tracker's assignee as opaque text (§5.2). It is
	// informational only — varvig has no assignment governance and never grants a
	// principal authority from it — surfaced so a linked ticket can show who the
	// tracker thinks owns the row. Empty means unset.
	Assignee string `json:"assignee,omitempty"`
}

// SetLink records (replaces) a ticket's external link as a note in the reserved
// varvig/external namespace. Newest note wins, like any other accreted note.
func SetLink(r *repo.Repo, ticketID multihash.Multihash, link Link, author string, now int64) error {
	payload, err := json.Marshal(link)
	if err != nil {
		return err
	}
	_, err = notes.New(r).Add(reserved.NoteExternal, ticketID, payload, author, now)
	return err
}

// GetLink returns a ticket's external link, if one is set.
func GetLink(r *repo.Repo, ticketID multihash.Multihash) (Link, bool, error) {
	chain, err := notes.New(r).List(reserved.NoteExternal, ticketID)
	if err != nil {
		return Link{}, false, err
	}
	if len(chain) == 0 {
		return Link{}, false, nil
	}
	var l Link
	if err := json.Unmarshal(chain[0].Note.Payload, &l); err != nil {
		return Link{}, false, err
	}
	return l, true, nil
}

// ErrNoLink reports that an operation needs an external link but none is set.
var ErrNoLink = errors.New("bridge: ticket has no external link")

// SetNudge stores an external priority nudge on the ticket's link, clamped to
// [0,1] (0 clears it). It is a projected tracker signal, so it requires an
// existing link — a nudge without a linked row is meaningless (§5.2).
func SetNudge(r *repo.Repo, ticketID multihash.Multihash, nudge float64, author string, now int64) error {
	link, ok, err := GetLink(r, ticketID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNoLink
	}
	if nudge < 0 {
		nudge = 0
	}
	if nudge > 1 {
		nudge = 1
	}
	if link.PriorityNudge == nudge {
		return nil // unchanged: don't accrete a note on every poll
	}
	link.PriorityNudge = nudge
	return SetLink(r, ticketID, link, author, now)
}

// SetAssignee mirrors the tracker's assignee onto the ticket's link as opaque
// text (empty clears it). Informational only; it requires an existing link.
func SetAssignee(r *repo.Repo, ticketID multihash.Multihash, name string, author string, now int64) error {
	link, ok, err := GetLink(r, ticketID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNoLink
	}
	if link.Assignee == name {
		return nil // unchanged: don't accrete a note on every poll
	}
	link.Assignee = name
	return SetLink(r, ticketID, link, author, now)
}

// Linked pairs a ticket id with its external link, for enumeration.
type Linked struct {
	TicketID multihash.Multihash
	Link     Link
}

// ListLinks returns every ticket that has an external link, sorted by ticket id
// for determinism. If system is non-empty, only links to that system are
// returned. This is what lets a connector sync all the tickets it mirrors in one
// pass rather than being handed each one (tickets §5).
func ListLinks(r *repo.Repo, system string) ([]Linked, error) {
	names, err := r.Refs.List()
	if err != nil {
		return nil, err
	}
	prefix := "refs/notes/" + reserved.NoteExternal + "/"
	var out []Linked
	for _, n := range names {
		if !strings.HasPrefix(n, prefix) {
			continue
		}
		id, err := multihash.ParseHex(strings.TrimPrefix(n, prefix))
		if err != nil {
			continue
		}
		link, ok, err := GetLink(r, id)
		if err != nil {
			return nil, err
		}
		if !ok || (system != "" && link.System != system) {
			continue
		}
		out = append(out, Linked{TicketID: id, Link: link})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TicketID.Hex() < out[j].TicketID.Hex() })
	return out, nil
}

// NeedsPush reports whether the ticket's head differs from what was last pushed
// to the tracker — i.e. there is local work the tracker has not seen. It is
// false when nothing changed locally, which is the outbound half of echo
// suppression (§5.4).
func NeedsPush(r *repo.Repo, ticketID multihash.Multihash) (bool, error) {
	head, err := ticket.Head(r, ticketID)
	if err != nil {
		return false, err
	}
	link, ok, err := GetLink(r, ticketID)
	if err != nil {
		return false, err
	}
	if !ok {
		return true, nil // never pushed
	}
	return head.Hex() != link.LastPushed, nil
}

// MarkPushed records that the ticket's current head has been sent to the
// tracker, so a subsequent outbound sync with no local change is a no-op.
func MarkPushed(r *repo.Repo, ticketID multihash.Multihash, author string, now int64) error {
	head, err := ticket.Head(r, ticketID)
	if err != nil {
		return err
	}
	link, _, err := GetLink(r, ticketID)
	if err != nil {
		return err
	}
	link.LastPushed = head.Hex()
	return SetLink(r, ticketID, link, author, now)
}

// ApplyInbound applies a spec edit that arrived from the tracker. The edit
// becomes an ordinary new intent revision authored by the bridge principal
// (§5.3), signed by signer — so approvals bound to the prior revision do not
// carry (§2.2) — unless it is a no-op or an echo, in which case nothing is
// created:
//
//   - echo: the inbound spec is the one we last applied, or it matches the
//     current head — no revision (§5.4);
//   - tracker loses: the local head advanced past what was last pushed, so a
//     concurrent tracker edit is dropped with ErrLocalWins (§7.5).
//
// It returns the resulting head revision and whether a new revision was created.
func ApplyInbound(r *repo.Repo, ticketID multihash.Multihash, spec string, priv ed25519.PrivateKey, author string, now int64) (multihash.Multihash, bool, error) {
	info, err := ticket.Get(r, ticketID)
	if err != nil {
		return nil, false, err
	}
	link, _, err := GetLink(r, ticketID)
	if err != nil {
		return nil, false, err
	}
	h := hashSpec(spec)

	// Echo: we already pulled this exact spec, or it changes nothing.
	if link.LastPulled == h || info.Spec == spec {
		if link.LastPulled != h {
			link.LastPulled = h
			if err := SetLink(r, ticketID, link, author, now); err != nil {
				return nil, false, err
			}
		}
		return info.Head, false, nil
	}

	// Tracker loses: local work is ahead of the last push, so drop the inbound
	// edit rather than clobber it.
	if link.LastPushed != "" && info.Head.Hex() != link.LastPushed {
		return info.Head, false, ErrLocalWins
	}

	rev, err := ticket.Revise(r, ticketID, spec, priv, author, now)
	if err != nil {
		return nil, false, err
	}
	link.LastPulled = h
	link.LastPushed = rev.Hex()
	if err := SetLink(r, ticketID, link, author, now); err != nil {
		return nil, false, err
	}
	return rev, true, nil
}

// RecordTransition records a tracker workflow transition as a weak attestation
// on a ticket revision (§5.3): a bridge asserting that a transition occurred,
// not a first-class signature. Strength is forced to weak here, and the core
// independently refuses anything stronger from a bridge-kind key (§2.4), so a
// transition can never masquerade as a real approval.
func RecordTransition(r *repo.Repo, priv ed25519.PrivateKey, target multihash.Multihash, decision object.Decision, rationale string, now int64) (multihash.Multihash, error) {
	signer := keySigner(priv)
	obj, err := attest.Sign(signer, object.Attestation{
		Target:    target,
		Decision:  decision,
		Strength:  object.StrengthWeak,
		Rationale: rationale,
		Timestamp: now,
	})
	if err != nil {
		return nil, err
	}
	return attest.Attach(r, obj, attest.Fingerprint(signer.Public()), now)
}

// RecordTransitionOnce records a workflow transition as a weak attestation, but
// only if it is new — a different decision, or the same decision on a newer head
// revision. A tracker reports the same state every poll; this makes re-syncs
// idempotent (tickets §5.3) instead of appending a duplicate weak attestation
// each time. It returns whether an attestation was written.
func RecordTransitionOnce(r *repo.Repo, priv ed25519.PrivateKey, ticketID multihash.Multihash, decision object.Decision, rationale string, now int64) (bool, error) {
	head, err := ticket.Head(r, ticketID)
	if err != nil {
		return false, err
	}
	link, _, err := GetLink(r, ticketID)
	if err != nil {
		return false, err
	}
	if link.LastTransition == decision.String() && link.LastTransitionHead == head.Hex() {
		return false, nil // already recorded for this head
	}
	if _, err := RecordTransition(r, priv, head, decision, rationale, now); err != nil {
		return false, err
	}
	link.LastTransition = decision.String()
	link.LastTransitionHead = head.Hex()
	if err := SetLink(r, ticketID, link, attest.Fingerprint(keySigner(priv).Public()), now); err != nil {
		return false, err
	}
	return true, nil
}

func hashSpec(spec string) string {
	mh, err := multihash.Sum(multihash.Default, []byte(spec))
	if err != nil {
		return ""
	}
	return mh.Hex()
}
