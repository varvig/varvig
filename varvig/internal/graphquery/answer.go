// Package graphquery is the context graph's read surface (builder G5).
//
// Two properties shape every type here, and both exist to prevent a specific
// wrong answer rather than to be tidy.
//
// **No result without coverage.** A query that returns no dependency edge for an
// unanalyzed language, in a form indistinguishable from a genuine absence of
// dependency, is §5's degradation failure exactly: the system claiming a safety
// it cannot deliver. So coverage is not an optional field on a result — the type
// does not exist in that shape, and the "cleaner" API that omits it cannot be
// written. This is the mitigation most likely to be traded away for ergonomics
// (GRAPH.md §11.4), so the trade is made unavailable.
//
// **No flat edge list.** Derived, imported and asserted edges have different
// authority: one is reproducible, one is a foreign system's word, one is a
// claim. A caller that receives them mixed will treat them alike, and an
// assertion will end up gating something (§11.3). Results are therefore
// partitioned, and merging requires an explicit call that still carries each
// edge's class.
package graphquery

// Answer is a three-valued reply to "does this edge hold": present, absent under
// coverage, or unknown outside coverage.
//
// The three are genuinely different and collapsing them is the bug. "No edge
// from a.rb to b.rb" means one thing when an analyzer read both files and
// another when nothing has ever parsed Ruby — and a caller that cannot tell
// them apart will conclude there is no dependency.
//
// So Answer has no boolean accessor for "present". A caller that wants a boolean
// must call PresentOr and say, in the call, what an unknown means for their
// purpose. `if a.PresentOr(false)` reads as a decision; `if a.Present` would
// read as a fact, and would silently be the wrong one a third of the time.
type Answer struct {
	// state is unexported and has no valid zero value, so a zero Answer cannot
	// masquerade as a real one.
	state State
	// path and reason let a result explain itself without a caller having to
	// reconstruct why.
	subject string
	reason  string
}

// State is what an Answer says. There is deliberately no zero value in the
// enumeration: an unset State is a bug, not a default.
type State uint8

const (
	// StatePresent: the edge was observed under this tree.
	StatePresent State = iota + 1
	// StateAbsentUnderCoverage: an analyzer read the files involved and found no
	// such edge. This is a fact about the code.
	StateAbsentUnderCoverage
	// StateUnknownOutsideCoverage: no analyzer covers one of the files involved,
	// so nothing has looked. This is a fact about the tooling, and it is not
	// evidence of absence.
	StateUnknownOutsideCoverage
)

func (s State) String() string {
	switch s {
	case StatePresent:
		return "present"
	case StateAbsentUnderCoverage:
		return "absent-under-coverage"
	case StateUnknownOutsideCoverage:
		return "unknown-outside-coverage"
	}
	return "invalid"
}

// State returns what the answer says. Switching on it is the exhaustive way to
// handle an Answer; Go cannot force the switch to be total, so the alternative
// route to a boolean is deliberately narrow.
func (a Answer) State() State { return a.state }

// Subject is what was asked about, for a message a human will read.
func (a Answer) Subject() string { return a.subject }

// Reason explains an absent or unknown answer — which file was not covered, or
// that coverage was complete and there simply is no edge.
func (a Answer) Reason() string { return a.reason }

// PresentOr collapses the answer to a boolean, and is the only way to do so.
//
// unknownMeans is what an unknown should count as *for this caller's purpose*,
// and there is no default because there is no safe one: a conflict detector must
// treat unknown as "may depend" and refuse, while a UI listing known
// dependencies should treat it as "not shown". Requiring the argument makes the
// choice appear at the call site, where the consequence lives.
func (a Answer) PresentOr(unknownMeans bool) bool {
	switch a.state {
	case StatePresent:
		return true
	case StateAbsentUnderCoverage:
		return false
	default:
		return unknownMeans
	}
}

// present, absent and unknown are the only constructors, so every Answer in
// existence has a valid state.
func present(subject string) Answer {
	return Answer{state: StatePresent, subject: subject, reason: "observed under this tree"}
}

func absent(subject string) Answer {
	return Answer{state: StateAbsentUnderCoverage, subject: subject,
		reason: "an analyzer read both endpoints and found no such edge"}
}

func unknown(subject, why string) Answer {
	return Answer{state: StateUnknownOutsideCoverage, subject: subject, reason: why}
}
