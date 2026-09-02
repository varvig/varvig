package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/attest"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// promoteSigner is an in-process Ed25519 signer implementing identity.Signer.
type promoteSigner struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

func (s promoteSigner) Public() ed25519.PublicKey     { return s.pub }
func (s promoteSigner) Sign(b []byte) ([]byte, error) { return ed25519.Sign(s.priv, b), nil }

func hashOf(t *testing.T, s string) multihash.Multihash {
	t.Helper()
	h, err := multihash.Sum(multihash.SHA2_256, []byte(s))
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	return h
}

// TestCheckPromotionNotStale exercises the ticket→commit staleness guard: a
// commit implementing an approved revision may promote; one implementing an
// unapproved (edited-past) revision is refused; a commit fulfilling nothing is
// unaffected. Mirrors the design's §8 promotion tests.
func TestCheckPromotionNotStale(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer := promoteSigner{priv: priv, pub: pub}

	approvedRev := hashOf(t, "approved-revision")
	editedRev := hashOf(t, "edited-revision-nobody-approved")

	// Approve only approvedRev.
	att, err := attest.Sign(signer, object.Attestation{
		Target: approvedRev, Decision: object.DecisionApprove, Strength: object.StrengthStrong,
	})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := attest.Attach(r, att, "director", 1); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	put := func(fulfills multihash.Multihash) multihash.Multihash {
		id, err := r.Objects.Put(object.NewChange(object.Change{Message: "impl", Timestamp: 2, Author: "eng", Fulfills: fulfills}))
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		return id
	}

	// Implements the approved revision -> allowed.
	if err := checkPromotionNotStale(r, put(approvedRev)); err != nil {
		t.Fatalf("approved-revision implementation should promote: %v", err)
	}
	// Implements an edited, unapproved revision -> refused (stale).
	if err := checkPromotionNotStale(r, put(editedRev)); err == nil {
		t.Fatal("stale implementation (unapproved revision) should be refused")
	}
	// Fulfills nothing -> unaffected.
	none, err := r.Objects.Put(object.NewChange(object.Change{Message: "hack", Timestamp: 3, Author: "eng"}))
	if err != nil {
		t.Fatal(err)
	}
	if err := checkPromotionNotStale(r, none); err != nil {
		t.Fatalf("a commit fulfilling nothing must not be blocked: %v", err)
	}
}
