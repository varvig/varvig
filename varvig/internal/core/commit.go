package core

import (
	"crypto/ed25519"
	"fmt"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/provenance"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// attachAndSign is the write step common to both a proposal and a commit: it
// stores the provenance, points the change at it, signs the change with the
// proposer's key, and stores the change. It is the single place provenance is
// attached to a change, regardless of which verb or which shell produced it
// (design addendum, U4). The caller supplies the change template with everything
// but Provenance filled in.
func attachAndSign(r *repo.Repo, prov object.Provenance, tmpl object.Change, signer ed25519.PrivateKey) (changeID, provID multihash.Multihash, err error) {
	provID, err = r.Objects.Put(object.NewProvenance(prov))
	if err != nil {
		return nil, nil, fmt.Errorf("core: store provenance: %w", err)
	}
	tmpl.Provenance = provID
	change := object.NewChange(tmpl)
	if err := provenance.Sign(change, signer); err != nil {
		return nil, nil, fmt.Errorf("core: sign change: %w", err)
	}
	changeID, err = r.Objects.Put(change)
	if err != nil {
		return nil, nil, fmt.Errorf("core: store change: %w", err)
	}
	return changeID, provID, nil
}

// CommitParams is everything committing a change needs, assembled by a shell. The
// shell owns building the tree, resolving parents and any Fulfills link, and
// running acceptance hooks; the core owns attaching provenance, signing, storing,
// and advancing the ref — the parts that must be identical no matter which shell
// commits (design addendum, U1/U4).
type CommitParams struct {
	// Ref is the ref to advance (usually HEAD's target); ExpectedOld is its
	// current value for the compare-and-swap (nil for an unborn ref).
	Ref         string
	ExpectedOld multihash.Multihash

	Tree     multihash.Multihash
	Parents  []multihash.Multihash
	Message  string
	Author   string
	Fulfills multihash.Multihash // the intent revision this materializes, or nil

	// Provenance is the provenance to attach. The shell builds it (e.g. from the
	// environment via provenance.Build) so generator pinning stays a shell concern;
	// the core attaches it so no change escapes without it.
	Provenance object.Provenance
	Signer     ed25519.PrivateKey
	Now        int64
}

// CommitResult reports a completed commit: the change stored and the public key
// it was signed by.
type CommitResult struct {
	Change     multihash.Multihash
	Provenance multihash.Multihash
	SignerKey  ed25519.PublicKey
}

// Commit finalizes a commit: it attaches provenance, signs and stores the change
// (carrying any Fulfills link), and advances the ref by compare-and-swap. Unlike
// a proposal it moves a ref, so it is the human/CLI write path — the gate never
// calls it. The CAS makes a concurrent move a clean conflict rather than a lost
// update.
func Commit(r *repo.Repo, caps CapabilitySet, p CommitParams) (CommitResult, error) {
	// Advancing a ref is the capability the gate never holds: a propose-only task
	// can create a change but never move a ref (design addendum, U3).
	if err := caps.Require(CapAdvanceRef); err != nil {
		return CommitResult{}, err
	}
	changeID, provID, err := attachAndSign(r, p.Provenance, object.Change{
		Tree:      p.Tree,
		Parents:   p.Parents,
		Message:   p.Message,
		Timestamp: p.Now,
		Author:    p.Author,
		Fulfills:  p.Fulfills,
	}, p.Signer)
	if err != nil {
		return CommitResult{}, err
	}
	if err := r.Refs.CompareAndSwap(p.Ref, p.ExpectedOld, changeID, p.Author, "commit"); err != nil {
		return CommitResult{}, err
	}
	var pub ed25519.PublicKey
	if p.Signer != nil {
		pub = p.Signer.Public().(ed25519.PublicKey)
	}
	return CommitResult{Change: changeID, Provenance: provID, SignerKey: pub}, nil
}
