// Package core is the one library both shells — the CLI (cmd/varvig) and the MCP
// gate (internal/mcp) — call to carry out a verb. Neither shell reimplements a
// verb: the shell parses its own inputs and formats its own output, and the
// behaviour that must be identical between them lives here, once.
//
// This is the structural fix for the CLI/gate disparity (design addendum, Tier
// U): a change proposed through the CLI inside a task checkout and one proposed
// through the gate must be signed the same way, carry the same provenance, and be
// recorded the same way. When each shell owned its own copy of that finalization,
// they drifted — the gate read its provenance back to confirm it, the CLI did
// not; the gate recorded a context-read set, the CLI dropped it. One code path
// removes the room for that drift and is the single place provenance is attached
// (U4).
package core

import (
	"crypto/ed25519"
	"fmt"
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/provenance"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/spec"
)

// ProposeParams is everything a proposal's finalization needs, assembled by a
// shell. How the proposed Tree was built — overlaying sent file contents,
// observing a checkout, or scanning a working tree — is the shell's business; how
// it is signed, what provenance it carries, and how it is recorded is not.
type ProposeParams struct {
	// Base is the parent change the proposal builds on, or nil for an unborn base.
	Base multihash.Multihash
	// Tree is the already-built proposed tree the change points at.
	Tree multihash.Multihash

	// Message is the change's one-line intent; Reasoning is the plan behind it.
	// Both are persisted into provenance (the message/reasoning split, §1.1).
	Message   string
	Reasoning string

	// Authority is the provenance authority (a task fingerprint, or a session
	// principal); Author is the change's author. They are usually the same string.
	Authority string
	Author    string

	// ContextRead is the read set (object hashes) folded into provenance as the
	// access record §1.1 requires. May be empty when no read log was kept.
	ContextRead []string

	// Signer signs the change with the proposer's key.
	Signer ed25519.PrivateKey

	// SpecTask is the speculation-pool bucket the proposal is recorded under.
	SpecTask string

	// Now is the proposal timestamp (Unix seconds), passed in so a caller can pin
	// the clock.
	Now int64
}

// StoredIntent is the provenance read back from the store after the write — not
// echoed from the request — so a caller can confirm at the write point that what
// it sent actually landed (C0.4).
type StoredIntent struct {
	TaskSpec    string
	ContextRead string
	Reasoning   string
}

// ProposeResult is the outcome of a finalized proposal. It never moves a ref: a
// proposal is a signed, speculative change recorded in the pool, and promotion is
// a separate, human-gated step.
type ProposeResult struct {
	Change     multihash.Multihash
	Provenance multihash.Multihash
	Parents    []multihash.Multihash
	Stored     StoredIntent
}

// Propose finalizes a proposal: it attaches provenance, signs and stores the
// change, records it in the speculation pool, and reads the provenance back so
// the caller can confirm what was stored. It is the single write-finalization
// path both shells use.
func Propose(r *repo.Repo, p ProposeParams) (ProposeResult, error) {
	prov := object.NewProvenance(object.Provenance{
		Authority:   p.Authority,
		TaskSpec:    p.Message,
		ContextRead: strings.Join(p.ContextRead, " "),
		Reasoning:   p.Reasoning,
	})
	provID, err := r.Objects.Put(prov)
	if err != nil {
		return ProposeResult{}, fmt.Errorf("core: store provenance: %w", err)
	}

	var parents []multihash.Multihash
	if p.Base != nil {
		parents = append(parents, p.Base)
	}
	change := object.NewChange(object.Change{
		Tree:       p.Tree,
		Parents:    parents,
		Message:    p.Message,
		Timestamp:  p.Now,
		Author:     p.Author,
		Provenance: provID,
	})
	if err := provenance.Sign(change, p.Signer); err != nil {
		return ProposeResult{}, fmt.Errorf("core: sign change: %w", err)
	}
	changeID, err := r.Objects.Put(change)
	if err != nil {
		return ProposeResult{}, fmt.Errorf("core: store change: %w", err)
	}

	if err := spec.Open(r.GitDir()).Add(p.SpecTask, changeID, p.Now); err != nil {
		return ProposeResult{}, fmt.Errorf("core: record proposal: %w", err)
	}

	// Read the provenance back straight from the store — not through any
	// read-logging path, which would fold these hashes into the task's own read
	// set — so the caller confirms what landed, not an echo of what it sent.
	storedProv, err := r.Objects.Get(provID)
	if err != nil {
		return ProposeResult{}, fmt.Errorf("core: read back provenance: %w", err)
	}
	pv, err := storedProv.AsProvenance()
	if err != nil {
		return ProposeResult{}, fmt.Errorf("core: stored provenance unreadable: %w", err)
	}

	return ProposeResult{
		Change:     changeID,
		Provenance: provID,
		Parents:    parents,
		Stored: StoredIntent{
			TaskSpec:    pv.TaskSpec,
			ContextRead: pv.ContextRead,
			Reasoning:   pv.Reasoning,
		},
	}, nil
}
