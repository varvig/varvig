package core

import (
	"crypto/ed25519"
	"fmt"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/provenance"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/sshkey"
)

// DerivedAuthority is the provenance authority for a change: the SSH fingerprint
// of the key that signs it. It is computed in the core from the signer, never
// taken from a shell (design addendum, U4) — a change's claimed authority must be
// the key that actually signed it, so no shell can stamp a change with an
// authority it does not hold the key for.
func DerivedAuthority(signer ed25519.PrivateKey) string {
	if signer == nil {
		return ""
	}
	return DerivedAuthorityOf(signer.Public().(ed25519.PublicKey))
}

// DerivedAuthorityOf is DerivedAuthority for a public key alone — used where the
// signer keeps its private key elsewhere (the daemon) and only the public half is
// in hand. The fingerprint is a property of the public key, so the two agree.
func DerivedAuthorityOf(pub ed25519.PublicKey) string {
	if len(pub) == 0 {
		return ""
	}
	return sshkey.PublicKey{Key: pub}.Fingerprint()
}

// VerifyAuthority re-checks that a change's self-described provenance authority
// matches the key that signed it — the record. It is the disagreement check the
// promotion path runs (design addendum, U4): a change whose provenance claims one
// authority but is signed by another key is rejected, naming the mismatch, rather
// than trusted on its self-description.
func VerifyAuthority(r *repo.Repo, changeID multihash.Multihash) error {
	obj, err := r.Objects.Get(changeID)
	if err != nil {
		return err
	}
	c, err := obj.AsChange()
	if err != nil {
		return nil // not a change: nothing to verify
	}
	if c.Provenance == nil {
		return nil // no self-description to disagree with its record
	}
	pub, err := provenance.Verify(obj)
	if err != nil {
		return fmt.Errorf("core: change %s does not verify: %w", changeID.Hex(), err)
	}
	provObj, err := r.Objects.Get(c.Provenance)
	if err != nil {
		return err
	}
	pv, err := provObj.AsProvenance()
	if err != nil {
		return err
	}
	want := sshkey.PublicKey{Key: pub}.Fingerprint()
	if pv.Authority != want {
		return fmt.Errorf("core: change %s claims authority %q but is signed by %q — "+
			"its self-description disagrees with its record", changeID.Hex(), pv.Authority, want)
	}
	return nil
}
