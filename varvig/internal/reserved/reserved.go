// Package reserved declares the ref and note namespaces set aside for the
// governance and ticket layers (tickets §1.3, decision D6). Reserving identity
// is the one thing that cannot be done after first run — the same argument that
// put self-describing multihash in step 1 — so the names are fixed now, in the
// object-store milestone, even though the layers that populate them are built
// purely on top later (tickets §6.4).
//
// Reservation costs nothing at runtime and changes no frozen format: refs are
// named pointers and note namespaces are UTF-8 strings, both of which already
// accept arbitrary values. What this package fixes is the *spelling*, so every
// layer built later attaches to the same names, and so an audit can ask "is
// this a governance ref?" without pattern-guessing. An older binary that has
// never heard of these namespaces still lists, syncs, and leaves them intact —
// it neither errors on nor garbage-collects a reserved-but-empty namespace,
// because an empty namespace is simply the absence of any ref.
package reserved

import "strings"

// Reserved ref namespaces. Ticket identity is a ref moved by compare-and-swap,
// exactly as a branch is (tickets §1.2).
const (
	// TicketsPrefix is the root of ticket identity refs:
	// refs/varvig/tickets/<id>, with the current intent revision optionally
	// separated at refs/varvig/tickets/<id>/spec.
	TicketsPrefix = "refs/varvig/tickets/"

	// PolicyRef points to the blob id of the repository's promotion-policy wasm
	// module (tickets §2.5). A policy is a content-addressed object, versioned
	// alongside the code it guards; the ref names the module in force.
	PolicyRef = "refs/varvig/policy"

	// PrincipalsRef points to the org chart: a tree of principal records
	// (tickets §1.4). Moved by compare-and-swap, so the chart is versioned,
	// hash-pinned, diffable, and auditable through the reflog — "who was allowed
	// to approve billing changes in March" is a query, not an interview.
	PrincipalsRef = "refs/varvig/principals"
)

// Ref namespaces reserved for the Factory layer (Design Notes VIII).
//
// Factory is a peer, not part of the core: it holds no core code path and the
// core populates none of these. They are reserved here for the same reason the
// governance names are — spelling is the thing that cannot be fixed after first
// run. A cell built today and a core built later must agree on where a lease
// lives without either pattern-guessing, and an audit asking "what has this
// repository committed to spend?" should be able to name the namespace rather
// than infer it.
//
// Reserving them costs nothing at runtime and changes no frozen format, exactly
// as for the names above. Nothing in the core reads or writes them, and an
// older binary that has never heard of them still lists, syncs and leaves them
// intact.
//
// They are deliberately **not** nested under refs/varvig/, which is the core's
// own space. These are another layer's.
const (
	// FactoryPrefix is the root of a cell's own published state — today its
	// capabilities object at refs/factory/cells/<cell-id>/capabilities.
	FactoryPrefix = "refs/factory/"

	// AttemptsPrefix holds one immutable ref per attempt, at
	// refs/attempts/<cell-id>/<task-id>/<n>. An attempt ref is created once and
	// never moved: that immutability is what lets two partitioned cells attempt
	// the same task and have both attempts survive reconnect.
	AttemptsPrefix = "refs/attempts/"

	// ClaimsPrefix holds advisory, expiring claims at
	// refs/claims/<cell-id>/<task-id>. Advisory is the whole design: a claim
	// never excludes another cell, it only says "I am working on this", and
	// across a partition it says nothing at all.
	ClaimsPrefix = "refs/claims/"

	// EnvelopesPrefix holds an overseer's spend ceilings at
	// refs/envelopes/<overseer-id>. An envelope is a *shared* ceiling across the
	// cells under that overseer, so it cannot be enforced from a stale view —
	// which is why leases exist alongside it.
	EnvelopesPrefix = "refs/envelopes/"

	// LeasesPrefix holds exclusive spend allocations at
	// refs/leases/<cell-id>/<capability>. Because no other cell can spend a
	// lease, a stale one is safe to act on — that is what lets a disconnected
	// cell keep working.
	LeasesPrefix = "refs/leases/"

	// ReservationsPrefix holds the durable record of one effectful action at
	// refs/reservations/<cell-id>/<idempotency-key>. The ref is created
	// compare-and-swap, and winning that swap is what grants the right to act:
	// it is the mechanism that stops an irreversible action happening twice.
	ReservationsPrefix = "refs/reservations/"
)

// factoryPrefixes is the fixed set of Factory ref namespaces.
var factoryPrefixes = []string{
	FactoryPrefix, AttemptsPrefix, ClaimsPrefix,
	EnvelopesPrefix, LeasesPrefix, ReservationsPrefix,
}

// Reserved note namespaces. A note namespace N lives at refs/notes/N/<target>;
// these are the N values (tickets §1.3). Signed decisions, foreign tracker
// bindings, and cached scoring output all accrete onto immutable objects as
// notes, touching no hash.
const (
	// NoteAttest carries signed approve / veto / delegate / request-change
	// decisions, each bound to a specific intent revision hash (tickets §2.1).
	NoteAttest = "varvig/attest"
	// NoteExternal maps ticket refs to foreign tracker IDs and holds the
	// per-direction sync watermarks that suppress echo (tickets §5.1, §5.4).
	NoteExternal = "varvig/external"
	// NoteScore holds computed and cached scoring output. Status and score are
	// always derived and cached here, never authored (tickets §2.1, §3.3).
	NoteScore = "varvig/score"
	// NoteCheck holds verification evidence: the result of running the repo's
	// declared checks over a proposal's tree (build spec P1.3, §1.3's evidence
	// invariant). Each record binds to the tree hash it was produced against, so an
	// edit after checking is detectable and stale evidence never counts as a pass.
	// It is a reserved namespace so it always replicates to peers — evidence a peer
	// cannot see is evidence that does not exist.
	NoteCheck = "varvig/check"
	// NoteScope carries a ticket's declared read set and write set (tickets
	// §3.1) — what makes it schedulable, and the input from which blocking
	// dependencies are derived rather than hand-declared (§3.2).
	NoteScope = "varvig/scope"
	// NoteDiscussion carries free-form ticket comments — human or agent notes,
	// and comments mirrored in from an external tracker (tickets §5.2). It is not
	// a governance namespace: comments are ungoverned data, never signed and
	// never consulted by scoring or attestation, so it is deliberately absent
	// from reservedNoteNamespaces below.
	NoteDiscussion = "varvig/discussion"
	// NoteArtifacts is a ticket's per-ticket index of attached external artifacts
	// (federation §1): one note per attach, keyed by ticket id, its payload naming
	// the artifact-ref object. Like NoteDiscussion it is ungoverned evidence —
	// never signed, never consulted by scoring or attestation, and deliberately
	// absent from reservedNoteNamespaces — so attaching an artifact never touches
	// the intent chain or a ticket's approvals.
	NoteArtifacts = "varvig/artifacts"
	// NoteArtifactRef pins an attached artifact-ref for reachability: a note keyed
	// by the artifact-ref object id (so GC marks it reachable-through, exactly as
	// Change.Artifacts would), its payload naming the ticket it belongs to. Also
	// ungoverned.
	NoteArtifactRef = "varvig/artifact-ref"
	// NoteBlocked carries blocked-on-scope reports and the widening decisions that
	// answer them (build spec P1.2), each keyed by the intent revision the task
	// ran under. A report is signed by the task and routes to whoever holds scope
	// authority, the same path an approval request travels; a widening is signed
	// by that authority. It is not a governance decision consulted by scoring or
	// status derivation — it is a routable request and its answer — so like
	// discussion and artifacts it is deliberately absent from reservedNoteNamespaces.
	NoteBlocked = "varvig/blocked"
)

// reservedNoteNamespaces is the fixed set of governance note namespaces.
var reservedNoteNamespaces = []string{NoteAttest, NoteExternal, NoteScore, NoteScope, NoteCheck}

// IsTicketRef reports whether name is (or is nested under) the reserved ticket
// ref namespace.
func IsTicketRef(name string) bool {
	return strings.HasPrefix(name, TicketsPrefix)
}

// IsReservedNoteNamespace reports whether ns is one of the reserved governance
// note namespaces, or nested under one (e.g. a future "varvig/attest/...").
func IsReservedNoteNamespace(ns string) bool {
	for _, r := range reservedNoteNamespaces {
		if ns == r || strings.HasPrefix(ns, r+"/") {
			return true
		}
	}
	return false
}

// NoteNamespaces returns the reserved governance note namespaces. The returned
// slice is a copy; callers may not mutate the reservation.
func NoteNamespaces() []string {
	return append([]string(nil), reservedNoteNamespaces...)
}

// IsFactoryRef reports whether name is nested under one of the Factory ref
// namespaces.
//
// It answers a question, and grants nothing: these names are reserved so a later
// layer attaches to the same spelling, not so the core polices them. Nothing
// here refuses a write, and the core neither reads nor writes these refs.
func IsFactoryRef(name string) bool {
	for _, p := range factoryPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// FactoryPrefixes returns the reserved Factory ref namespaces. The returned
// slice is a copy; callers may not mutate the reservation.
func FactoryPrefixes() []string {
	return append([]string(nil), factoryPrefixes...)
}
