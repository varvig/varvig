package core

import (
	"crypto/ed25519"
	"crypto/rand"
	"reflect"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/provenance"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// TestAuthorityDerivedFromSignerNotShell: the provenance authority a proposal and
// a commit carry is the fingerprint of the signing key, whichever shell produced
// it — the core derives it, a shell cannot set it (design addendum, U4).
func TestAuthorityDerivedFromSignerNotShell(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	want := DerivedAuthority(priv)
	tree, _ := r.Objects.Put(object.NewTree(nil))

	// Propose: the provenance authority is the signer's fingerprint.
	pr, err := Propose(r, CLICapabilities(), ProposeParams{
		Tree: tree, Message: "m", Author: "a-display-name", Signer: priv, SpecTask: "t", Now: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := storedAuthority(t, r, pr.Provenance); got != want {
		t.Fatalf("proposal authority = %q, want the signer fingerprint %q", got, want)
	}

	// Commit: even though the shell hands in a provenance whose Authority says
	// something else, the core stamps the signer's fingerprint.
	cr, err := Commit(r, CLICapabilities(), CommitParams{
		Ref: "refs/heads/main", ExpectedOld: nil, Tree: tree, Message: "m",
		Author: "a-display-name", Provenance: object.Provenance{Authority: "SHA256:i-claim-to-be-someone-else"},
		Signer: priv, Now: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := storedAuthority(t, r, cr.Provenance); got != want {
		t.Fatalf("commit authority = %q, want the signer fingerprint %q (shell claim must be overridden)", got, want)
	}
}

func storedAuthority(t *testing.T, r *repo.Repo, provID multihash.Multihash) string {
	t.Helper()
	obj, err := r.Objects.Get(provID)
	if err != nil {
		t.Fatal(err)
	}
	pv, err := obj.AsProvenance()
	if err != nil {
		t.Fatal(err)
	}
	return pv.Authority
}

// TestVerifyAuthorityRejectsForgery: a change whose provenance claims an authority
// other than the key that signed it is rejected, naming the disagreement; a
// core-produced change passes.
func TestVerifyAuthorityRejectsForgery(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	tree, _ := r.Objects.Put(object.NewTree(nil))

	// Core-produced change verifies.
	pr, err := Propose(r, CLICapabilities(), ProposeParams{
		Tree: tree, Message: "m", Author: "a", Signer: priv, SpecTask: "t", Now: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAuthority(r, pr.Change); err != nil {
		t.Fatalf("a core-produced change must verify: %v", err)
	}

	// Hand-forged: a provenance claiming a different authority, signed by priv.
	provID, _ := r.Objects.Put(object.NewProvenance(object.Provenance{Authority: "SHA256:forged", TaskSpec: "x"}))
	change := object.NewChange(object.Change{Tree: tree, Provenance: provID, Message: "forged", Author: "x", Timestamp: 3})
	if err := provenance.Sign(change, priv); err != nil {
		t.Fatal(err)
	}
	cid, _ := r.Objects.Put(change)
	if err := VerifyAuthority(r, cid); err == nil {
		t.Fatal("a change claiming an authority it did not sign with must be rejected")
	}
}

// TestProposeParamsHasNoAuthorityField is the structural guard (design addendum,
// U4): the shell layer has no field through which to attach a provenance
// authority — the core derives it.
func TestProposeParamsHasNoAuthorityField(t *testing.T) {
	if _, ok := reflect.TypeOf(ProposeParams{}).FieldByName("Authority"); ok {
		t.Fatal("ProposeParams must not expose an Authority field — the core derives authority from the signer")
	}
}
