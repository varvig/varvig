package provenance

import (
	"crypto/ed25519"
	"errors"
	"testing"

	"github.com/dividebyzero/claude-experiments/loom/internal/multihash"
	"github.com/dividebyzero/claude-experiments/loom/internal/object"
)

func newChange(t *testing.T, msg string) *object.Object {
	t.Helper()
	tree, _ := multihash.Sum(multihash.BLAKE3, []byte("tree"))
	prov, _ := multihash.Sum(multihash.BLAKE3, []byte("prov"))
	return object.NewChange(object.Change{Tree: tree, Provenance: prov, Message: msg, Timestamp: 1, Author: "alice"})
}

func newKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return priv
}

func TestSignVerifyRoundTrip(t *testing.T) {
	ch := newChange(t, "add feature")
	priv := newKey(t)
	if err := Sign(ch, priv); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	pub, err := Verify(ch)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !pub.Equal(priv.Public().(ed25519.PublicKey)) {
		t.Fatal("verified public key does not match signer")
	}
}

func TestVerifyDetectsTamper(t *testing.T) {
	ch := newChange(t, "original")
	priv := newKey(t)
	if err := Sign(ch, priv); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	// Tamper with a signed field after signing.
	ch.SetField(3, []byte("tampered message")) // tag 3 = message
	if _, err := Verify(ch); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("err = %v, want ErrBadSignature", err)
	}
}

func TestVerifyTamperedProvenanceReference(t *testing.T) {
	ch := newChange(t, "m")
	priv := newKey(t)
	_ = Sign(ch, priv)
	// Repoint the provenance id: the signature covers it, so verify must fail.
	other, _ := multihash.Sum(multihash.BLAKE3, []byte("other-prov"))
	ch.SetField(6, other) // tag 6 = provenance
	if _, err := Verify(ch); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("err = %v, want ErrBadSignature", err)
	}
}

func TestVerifyUnsigned(t *testing.T) {
	ch := newChange(t, "m")
	if _, err := Verify(ch); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("err = %v, want ErrUnsigned", err)
	}
}

func TestVerifyWrongKeyReportsSigner(t *testing.T) {
	a := newChange(t, "by A")
	privA := newKey(t)
	_ = Sign(a, privA)
	b := newChange(t, "by B")
	privB := newKey(t)
	_ = Sign(b, privB)

	pubA, err := Verify(a)
	if err != nil {
		t.Fatalf("Verify a: %v", err)
	}
	if !pubA.Equal(privA.Public().(ed25519.PublicKey)) || pubA.Equal(privB.Public().(ed25519.PublicKey)) {
		t.Fatal("signer attribution wrong")
	}
}

func TestIdentityPersistence(t *testing.T) {
	dir := t.TempDir()
	k1, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	k2, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !k1.Equal(k2) {
		t.Fatal("identity not stable across loads")
	}
}

func TestBuildIncludesToolHash(t *testing.T) {
	// The running test binary exists, so hashSelf should succeed.
	p := Build("alice")
	if p.Authority != "alice" {
		t.Fatalf("authority = %q", p.Authority)
	}
	if p.ToolHash == nil {
		t.Fatal("tool hash not captured")
	}
}
