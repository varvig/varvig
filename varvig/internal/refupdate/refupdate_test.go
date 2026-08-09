package refupdate

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
)

func testSignerKey(t *testing.T) (ed25519.PrivateKey, ed25519.PublicKey) {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i * 7)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return priv, priv.Public().(ed25519.PublicKey)
}

func mustHash(t *testing.T, s string) multihash.Multihash {
	t.Helper()
	h, err := multihash.Sum(multihash.Default, []byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func sampleParams(t *testing.T, pub ed25519.PublicKey) Params {
	nonce, err := NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	return Params{
		Ref:         "refs/heads/main",
		ExpectedOld: mustHash(t, "old-tip"),
		New:         mustHash(t, "new-tip"),
		Scope:       "/",
		SignerKey:   pub,
		Nonce:       nonce,
		NotAfter:    2000000000,
		Evidence:    []multihash.Multihash{mustHash(t, "e2"), mustHash(t, "e1")},
	}
}

func TestPayloadEncodeDecodeRoundTrip(t *testing.T) {
	_, pub := testSignerKey(t)
	p, err := New(sampleParams(t, pub))
	if err != nil {
		t.Fatal(err)
	}
	b := p.CanonicalBytes()
	got, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.CanonicalBytes(), b) {
		t.Fatal("canonical bytes not stable across decode")
	}
	if got.Ref() != "refs/heads/main" || got.Scope() != "/" {
		t.Fatalf("fields wrong: ref=%q scope=%q", got.Ref(), got.Scope())
	}
	if got.NotAfter() != 2000000000 {
		t.Fatalf("not_after=%d", got.NotAfter())
	}
	ev, err := got.Evidence()
	if err != nil || len(ev) != 2 {
		t.Fatalf("evidence: %v err=%v", ev, err)
	}
	// Evidence must be sorted regardless of input order.
	if bytes.Compare(ev[0], ev[1]) >= 0 {
		t.Fatal("evidence not sorted")
	}
}

func TestDecodeRejectsUnknownCritical(t *testing.T) {
	_, pub := testSignerKey(t)
	p, _ := New(sampleParams(t, pub))
	// Inject an unknown critical field (tag 9, below CriticalMax).
	p.fields = append(p.fields, field{tag: 9, val: []byte("x")})
	sortFields(p)
	if _, err := Decode(p.CanonicalBytes()); !errors.Is(err, ErrUnknownCritical) {
		t.Fatalf("expected ErrUnknownCritical, got %v", err)
	}
}

func TestDecodePreservesUnknownNonCritical(t *testing.T) {
	_, pub := testSignerKey(t)
	p, _ := New(sampleParams(t, pub))
	// A non-critical extension (tag >= CriticalMax) must survive round-trip.
	p.fields = append(p.fields, field{tag: 100, val: []byte("future")})
	sortFields(p)
	b := p.CanonicalBytes()
	got, err := Decode(b)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(got.CanonicalBytes(), b) {
		t.Fatal("unknown non-critical field not preserved")
	}
	v, ok := got.get(100)
	if !ok || string(v) != "future" {
		t.Fatal("extension field lost")
	}
}

func TestDecodeRejectsNonCanonical(t *testing.T) {
	_, pub := testSignerKey(t)
	p, _ := New(sampleParams(t, pub))
	b := p.CanonicalBytes()
	// Trailing byte.
	if _, err := Decode(append(b, 0)); err == nil {
		t.Fatal("expected error on trailing bytes")
	}
	// Bad magic.
	bad := append([]byte(nil), b...)
	bad[0] = 'X'
	if _, err := Decode(bad); err == nil {
		t.Fatal("expected error on bad magic")
	}
}

func sortFields(p *Payload) {
	// insertion sort by tag (tests only)
	for i := 1; i < len(p.fields); i++ {
		for j := i; j > 0 && p.fields[j-1].tag > p.fields[j].tag; j-- {
			p.fields[j-1], p.fields[j] = p.fields[j], p.fields[j-1]
		}
	}
}
