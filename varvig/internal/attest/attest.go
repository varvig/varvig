// Package attest builds, signs, and verifies governance attestations — the
// signed approve / veto / delegate / request-change decisions of tickets §2.
// An attestation is not a status field; it is a signed decision object bound to
// a *specific intent revision hash*. Status is always derived from the set of
// attestations, never authored (tickets §2.1).
//
// Three properties this package exists to guarantee:
//
//   - Binding to a version (§2.2). A signature covers the target hash, so
//     editing the spec — which yields a new revision hash — leaves the approval
//     attached to the bytes that were actually read and signed. It does not
//     carry forward. Derive() reflects this for free.
//   - Honest strength (§2.4). Strength is recorded at signing time and never
//     upgraded. A bridge, which signs on behalf of a keyless principal, can only
//     ever produce a weak attestation; VerifyWithPrincipal enforces this so a
//     compromised bridge cannot mint a strong approval.
//   - Tamper evidence. The signature is over the object's SignableBytes, which
//     includes the target and strength, so any mutation invalidates it.
package attest

import (
	"crypto/ed25519"
	"errors"
	"fmt"

	"github.com/dividebyzero/claude-experiments/varvig/internal/identity"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/sshkey"
)

// schemeEd25519 is the signature scheme tag stored first in the signature blob,
// matching the change-signing convention in package provenance.
const schemeEd25519 uint64 = 1

var (
	// ErrUnsigned is returned when an attestation carries no signature.
	ErrUnsigned = errors.New("attest: attestation is not signed")
	// ErrBadSignature is returned when a signature does not verify.
	ErrBadSignature = errors.New("attest: signature does not verify")
	// ErrUnknownScheme is returned for an unrecognized signature scheme.
	ErrUnknownScheme = errors.New("attest: unknown signature scheme")
	// ErrStrengthKind is returned when an attestation's strength is inconsistent
	// with the signing principal's kind (tickets §2.4): a bridge may only produce
	// weak, and a keyholder may not produce weak.
	ErrStrengthKind = errors.New("attest: strength not permitted for signer kind")
	// ErrUnknownPrincipal is returned when the signer is not a known principal.
	ErrUnknownPrincipal = errors.New("attest: signer is not a known principal")
)

// Sign builds an attestation object from a and signs it with signer. The
// signature covers SignableBytes (everything but the signature field), so it
// commits to the target hash, decision, and strength.
func Sign(signer identity.Signer, a object.Attestation) (*object.Object, error) {
	if signer == nil {
		return nil, errors.New("attest: nil signer (identity cannot sign; try ssh-agent)")
	}
	if a.Target == nil {
		return nil, errors.New("attest: attestation has no target")
	}
	if a.Strength == object.StrengthUnknown || a.Decision == object.DecisionUnknown {
		return nil, errors.New("attest: decision and strength are required")
	}
	obj := object.NewAttestation(a)
	sig, err := signer.Sign(obj.SignableBytes())
	if err != nil {
		return nil, err
	}
	if len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("attest: signer returned %d-byte signature", len(sig))
	}
	obj.SetSignature(encodeSig(schemeEd25519, signer.Public(), sig))
	return obj, nil
}

// Verify checks an attestation object's signature and returns the signing
// public key together with the decoded attestation. It does not consult a
// principal registry; use VerifyWithPrincipal to also enforce the strength/kind
// rule.
func Verify(obj *object.Object) (ed25519.PublicKey, object.Attestation, error) {
	a, err := obj.AsAttestation()
	if err != nil {
		return nil, object.Attestation{}, err
	}
	blob, ok := obj.RawSignature()
	if !ok {
		return nil, object.Attestation{}, ErrUnsigned
	}
	scheme, pub, sig, err := decodeSig(blob)
	if err != nil {
		return nil, object.Attestation{}, err
	}
	if scheme != schemeEd25519 {
		return nil, object.Attestation{}, ErrUnknownScheme
	}
	if len(pub) != ed25519.PublicKeySize {
		return nil, object.Attestation{}, ErrBadSignature
	}
	if !ed25519.Verify(pub, obj.SignableBytes(), sig) {
		return nil, object.Attestation{}, ErrBadSignature
	}
	return pub, a, nil
}

// KindResolver maps a signer's SSH fingerprint to its principal kind. The org
// chart is a set of content-addressed principal records (tickets §1.4); a
// caller supplies whatever view of it applies. PrincipalSet is a simple
// in-memory implementation.
type KindResolver interface {
	KindOf(fingerprint string) (object.Kind, bool)
}

// VerifyWithPrincipal verifies the signature and then enforces the strength
// rule against the signer's kind (tickets §2.4). It returns the signer's
// fingerprint and the decoded attestation.
//
// The rule, and why it is here rather than at signing time: a signer controls
// its own bytes, so a compromised bridge could sign a strong attestation
// directly. The binding check must therefore run at verification, where the
// principal registry — not the payload — decides what the signer is allowed to
// assert. A bridge may produce only weak; a human or agent may produce strong
// or delegated but never weak (weak means "the principal holds no key").
func VerifyWithPrincipal(obj *object.Object, r KindResolver) (string, object.Attestation, error) {
	pub, a, err := Verify(obj)
	if err != nil {
		return "", object.Attestation{}, err
	}
	fp := Fingerprint(pub)
	kind, ok := r.KindOf(fp)
	if !ok {
		return fp, object.Attestation{}, ErrUnknownPrincipal
	}
	if err := CheckStrengthKind(a.Strength, kind); err != nil {
		return fp, object.Attestation{}, err
	}
	return fp, a, nil
}

// CheckStrengthKind enforces the §2.4 rule tying an attestation's strength to
// the signing principal's kind: a bridge may produce only weak (it signs on
// behalf of a keyless principal), and a keyholder may not produce weak. It is
// exported so an authoring path can reject an inconsistent decision early, at
// signing time, with the same rule verification applies later.
func CheckStrengthKind(s object.Strength, k object.Kind) error {
	switch k {
	case object.KindBridge:
		if s != object.StrengthWeak {
			return fmt.Errorf("%w: bridge may only produce weak, got %s", ErrStrengthKind, s)
		}
	case object.KindHuman, object.KindAgent:
		if s == object.StrengthWeak {
			return fmt.Errorf("%w: keyholder may not produce weak", ErrStrengthKind)
		}
	default:
		return fmt.Errorf("%w: unknown principal kind", ErrStrengthKind)
	}
	return nil
}

// Fingerprint returns the SSH SHA256 fingerprint of an Ed25519 public key — the
// identifier the trust store and principal registry are keyed by.
func Fingerprint(pub ed25519.PublicKey) string {
	return sshkey.PublicKey{Key: pub}.Fingerprint()
}

// PrincipalSet is an in-memory fingerprint→kind map implementing KindResolver.
// Build it from principal objects with Add.
type PrincipalSet map[string]object.Kind

// Add records a principal by fingerprint of its key.
func (ps PrincipalSet) Add(p object.Principal) {
	ps[Fingerprint(p.Key)] = p.Kind
}

// KindOf implements KindResolver.
func (ps PrincipalSet) KindOf(fingerprint string) (object.Kind, bool) {
	k, ok := ps[fingerprint]
	return k, ok
}

// --- signature blob: uvarint(scheme) bytes(pub) bytes(sig) ---

func encodeSig(scheme uint64, pub, sig []byte) []byte {
	var b []byte
	b = appendUvarint(b, scheme)
	b = appendBytes(b, pub)
	b = appendBytes(b, sig)
	return b
}

func decodeSig(b []byte) (scheme uint64, pub, sig []byte, err error) {
	scheme, n, err := readUvarint(b)
	if err != nil {
		return 0, nil, nil, err
	}
	b = b[n:]
	pub, b, err = takeBytes(b)
	if err != nil {
		return 0, nil, nil, err
	}
	sig, _, err = takeBytes(b)
	if err != nil {
		return 0, nil, nil, err
	}
	return scheme, pub, sig, nil
}

func appendUvarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func appendBytes(b, v []byte) []byte {
	b = appendUvarint(b, uint64(len(v)))
	return append(b, v...)
}

func readUvarint(b []byte) (uint64, int, error) {
	var x uint64
	var s uint
	for i := 0; i < len(b); i++ {
		ch := b[i]
		if i == 10 {
			return 0, 0, errors.New("attest: varint overflow")
		}
		if ch < 0x80 {
			if i == 9 && ch > 1 {
				return 0, 0, errors.New("attest: varint overflow")
			}
			return x | uint64(ch)<<s, i + 1, nil
		}
		x |= uint64(ch&0x7f) << s
		s += 7
	}
	return 0, 0, errors.New("attest: truncated varint")
}

func takeBytes(b []byte) (val, rest []byte, err error) {
	n, k, err := readUvarint(b)
	if err != nil {
		return nil, nil, err
	}
	b = b[k:]
	if n > uint64(len(b)) {
		return nil, nil, errors.New("attest: truncated field")
	}
	return b[:n], b[n:], nil
}
