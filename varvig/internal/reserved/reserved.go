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
)

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
	// NoteScope carries a ticket's declared read set and write set (tickets
	// §3.1) — what makes it schedulable, and the input from which blocking
	// dependencies are derived rather than hand-declared (§3.2).
	NoteScope = "varvig/scope"
)

// reservedNoteNamespaces is the fixed set of governance note namespaces.
var reservedNoteNamespaces = []string{NoteAttest, NoteExternal, NoteScore, NoteScope}

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
