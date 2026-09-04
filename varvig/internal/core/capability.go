package core

import (
	"errors"
	"fmt"
	"sort"
)

// Capability is a named authority a shell holds and the core checks (design
// addendum, U3). One shared core means a CLI-only affordance — moving a ref,
// fetching over the network, running hooks, reading outside scope — becomes
// reachable through the gate unless authority is explicit. So authority is a
// parameter the shell passes in, never inferred from the call site, the
// environment, or a config file, and the core refuses a verb whose capability the
// caller does not hold rather than quietly doing less.
type Capability string

const (
	// CapPropose creates a signed, speculative change. Both shells hold it — it is
	// the floor, the one thing a propose-only task can do.
	CapPropose Capability = "propose"
	// CapAdvanceRef moves a ref: commit, promote, update-ref. The gate never holds
	// it — promotion is human-gated and an agent is propose-only.
	CapAdvanceRef Capability = "advance-ref"
	// CapReadAnyPath reads outside the task's scope. The gate never holds it: the
	// capability is the read set.
	CapReadAnyPath Capability = "read-any-path"
	// CapNetwork fetches, clones, or pushes to a peer. An operator concern, not a
	// task's.
	CapNetwork Capability = "network"
	// CapRunHooks executes acceptance hooks. A commit-side concern the gate, which
	// only proposes, never reaches.
	CapRunHooks Capability = "run-hooks"
)

// ErrCapability is returned when a verb is attempted without the capability it
// requires. It is a named refusal, never a degraded result (A3).
var ErrCapability = errors.New("core: capability not held")

// CapabilitySet is the authority a caller carries. It is constructed by a shell
// and passed into the core; the core reads it, never widens it.
type CapabilitySet map[Capability]bool

// NewCapabilitySet builds a set from the given capabilities.
func NewCapabilitySet(caps ...Capability) CapabilitySet {
	s := make(CapabilitySet, len(caps))
	for _, c := range caps {
		s[c] = true
	}
	return s
}

// CLICapabilities is the full authority a human at the CLI holds.
func CLICapabilities() CapabilitySet {
	return NewCapabilitySet(CapPropose, CapAdvanceRef, CapReadAnyPath, CapNetwork, CapRunHooks)
}

// GateCapabilities is the authority a propose-only task gate holds — a strict
// subset of the CLI's. It can propose and nothing else; every capability that
// would let a task change shared state or read past its scope is withheld.
func GateCapabilities() CapabilitySet {
	return NewCapabilitySet(CapPropose)
}

// Has reports whether the set holds a capability.
func (s CapabilitySet) Has(c Capability) bool { return s[c] }

// Require returns a named refusal when the set lacks a capability, so a verb can
// gate on it at its entry point.
func (s CapabilitySet) Require(c Capability) error {
	if !s.Has(c) {
		return fmt.Errorf("%w: %s", ErrCapability, c)
	}
	return nil
}

// SubsetOf reports whether every capability in s is also in other.
func (s CapabilitySet) SubsetOf(other CapabilitySet) bool {
	for c := range s {
		if !other.Has(c) {
			return false
		}
	}
	return true
}

// Equal reports whether two sets hold exactly the same capabilities.
func (s CapabilitySet) Equal(other CapabilitySet) bool {
	return len(s) == len(other) && s.SubsetOf(other)
}

// List returns the capabilities in the set, sorted — for logging and diagnostics.
func (s CapabilitySet) List() []Capability {
	out := make([]Capability, 0, len(s))
	for c := range s {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
