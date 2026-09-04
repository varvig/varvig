package core

import (
	"crypto/ed25519"
	"fmt"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/provenance"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// Signer signs a change on behalf of an authority. A local key satisfies it, and
// so does a remote signer that keeps the key in the daemon and round-trips to it
// (design addendum, F4) — either way the core stamps the authority from the
// signer's public key, never from the shell.
type Signer interface {
	Public() ed25519.PublicKey
	Sign([]byte) ([]byte, error)
}

// attachAndSign is the write step common to both a proposal and a commit: it
// stores the provenance, points the change at it, signs the change with the
// proposer's key, and stores the change. It is the single place provenance is
// attached to a change, regardless of which verb or which shell produced it
// (design addendum, U4). The caller supplies the change template with everything
// but Provenance filled in.
func attachAndSign(r *repo.Repo, prov object.Provenance, tmpl object.Change, signer Signer) (changeID, provID multihash.Multihash, err error) {
	// The authority is stamped from the signer, overriding anything a shell put in
	// prov.Authority: a change's claimed authority is the key that signs it, and
	// that determination is the core's alone (design addendum, U4).
	prov.Authority = DerivedAuthorityOf(signer.Public())
	provID, err = r.Objects.Put(object.NewProvenance(prov))
	if err != nil {
		return nil, nil, fmt.Errorf("core: store provenance: %w", err)
	}
	tmpl.Provenance = provID
	change := object.NewChange(tmpl)
	if err := provenance.SignWith(change, signer); err != nil {
		return nil, nil, fmt.Errorf("core: sign change: %w", err)
	}
	changeID, err = r.Objects.Put(change)
	if err != nil {
		return nil, nil, fmt.Errorf("core: store change: %w", err)
	}
	return changeID, provID, nil
}

// localSigner adapts a locally held Ed25519 key to Signer.
type localSigner ed25519.PrivateKey

func (k localSigner) Public() ed25519.PublicKey {
	if len(k) == 0 {
		return nil
	}
	return ed25519.PrivateKey(k).Public().(ed25519.PublicKey)
}

func (k localSigner) Sign(msg []byte) ([]byte, error) {
	return ed25519.Sign(ed25519.PrivateKey(k), msg), nil
}

// signerFor resolves the effective signer for a write: the explicit RemoteSigner
// when one is supplied, otherwise the local key. It is how a task checkout in the
// daemon path signs as the task without ever holding the task key.
func signerFor(key ed25519.PrivateKey, remote Signer) Signer {
	if remote != nil {
		return remote
	}
	return localSigner(key)
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
	// the core attaches it, and stamps its Authority from the Signer, so a shell
	// cannot claim an authority it does not hold the key for (U4).
	Provenance object.Provenance
	Signer     ed25519.PrivateKey
	// RemoteSigner, when set, signs in place of Signer — the task checkout's path
	// to signing as a task whose key lives in the daemon (design addendum, F4).
	RemoteSigner Signer
	Now          int64
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
	signer := signerFor(p.Signer, p.RemoteSigner)
	changeID, provID, err := attachAndSign(r, p.Provenance, object.Change{
		Tree:      p.Tree,
		Parents:   p.Parents,
		Message:   p.Message,
		Timestamp: p.Now,
		Author:    p.Author,
		Fulfills:  p.Fulfills,
	}, signer)
	if err != nil {
		return CommitResult{}, err
	}
	if err := r.Refs.CompareAndSwap(p.Ref, p.ExpectedOld, changeID, p.Author, "commit"); err != nil {
		return CommitResult{}, err
	}
	return CommitResult{Change: changeID, Provenance: provID, SignerKey: signer.Public()}, nil
}
