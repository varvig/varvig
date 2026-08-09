package refupdate

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/dividebyzero/claude-experiments/varvig/internal/identity"
)

// signedMagic frames a serialized SignedUpdate (payload + detached signature).
var signedMagic = [4]byte{'V', 'R', 'S', '1'}

// ErrBadSignature is returned when a signature does not verify against the
// public key embedded in the payload.
var ErrBadSignature = errors.New("refupdate: signature does not verify")

// SignedUpdate is a payload together with the detached Ed25519 signature over
// its canonical bytes. The signature is *not* part of the payload, so the bytes
// that get signed and the bytes that get verified are identical (§5.1).
type SignedUpdate struct {
	Payload *Payload
	Sig     []byte // 64-byte Ed25519 signature over Payload.CanonicalBytes()
}

// NewNonce returns a fresh random 16-byte nonce.
func NewNonce() ([]byte, error) {
	n := make([]byte, NonceLen)
	if _, err := rand.Read(n); err != nil {
		return nil, err
	}
	return n, nil
}

// Sign produces a SignedUpdate: it fills in the signer's public key from the
// signer, builds the canonical payload, and signs it. The signer's key must
// match the SignerKey the payload will carry, so Sign takes the parameters and
// derives the key from signer.Public().
func Sign(signer identity.Signer, p Params) (*SignedUpdate, error) {
	if signer == nil {
		return nil, errors.New("refupdate: nil signer (identity cannot sign; try ssh-agent)")
	}
	p.SignerKey = signer.Public()
	payload, err := New(p)
	if err != nil {
		return nil, err
	}
	sig, err := signer.Sign(payload.CanonicalBytes())
	if err != nil {
		return nil, err
	}
	if len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("refupdate: signer returned %d-byte signature", len(sig))
	}
	return &SignedUpdate{Payload: payload, Sig: sig}, nil
}

// VerifySignature checks the detached signature against the payload's embedded
// public key. It is step 5 of the pipeline, exposed separately so callers can
// verify a signature without a trust store (e.g. tests, relays).
func (su *SignedUpdate) VerifySignature() error {
	pub := su.Payload.SignerKey()
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: bad public key", ErrBadSignature)
	}
	if !ed25519.Verify(pub, su.Payload.CanonicalBytes(), su.Sig) {
		return ErrBadSignature
	}
	return nil
}

// Encode serializes a SignedUpdate to transport bytes: magic, then the
// length-prefixed canonical payload, then the length-prefixed signature.
func (su *SignedUpdate) Encode() []byte {
	var b []byte
	b = append(b, signedMagic[:]...)
	b = appendBytes(b, su.Payload.CanonicalBytes())
	b = appendBytes(b, su.Sig)
	return b
}

// DecodeSigned parses transport bytes back into a SignedUpdate, validating the
// payload's canonical framing.
func DecodeSigned(b []byte) (*SignedUpdate, error) {
	c := &cursor{b: b}
	m, err := c.take(4)
	if err != nil || !bytes.Equal(m, signedMagic[:]) {
		return nil, fmt.Errorf("%w: bad signed-update magic", ErrMalformed)
	}
	payloadBytes, err := c.takeBytes()
	if err != nil {
		return nil, err
	}
	sig, err := c.takeBytes()
	if err != nil {
		return nil, err
	}
	if !c.empty() {
		return nil, fmt.Errorf("%w: trailing bytes after signed update", ErrMalformed)
	}
	payload, err := Decode(payloadBytes)
	if err != nil {
		return nil, err
	}
	return &SignedUpdate{Payload: payload, Sig: append([]byte(nil), sig...)}, nil
}
