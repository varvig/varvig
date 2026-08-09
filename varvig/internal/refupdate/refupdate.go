// Package refupdate implements signed ref updates — the single most important
// mechanism in the auth design (§5). A ref update is a *signed assertion*, not
// merely an operation performed over an authenticated channel: the proof of
// authority travels in the payload. That is what lets a ref update be relayed
// through peers nobody trusts and still be verified at its destination (§5.3);
// without it the system would be quietly server-centric.
//
// The payload is a compare-and-swap request — move `ref` from `expected_old` to
// `new` — carrying the signer's public key, a scope, an anti-replay nonce, and
// an expiry. It is encoded canonically (deterministic, minimal varints, sorted
// unique fields, no trailing bytes) so the exact bytes that were signed can be
// reconstructed byte-for-byte by any implementation (§5.1).
//
// # Critical vs non-critical fields (§5.2 step 1)
//
// Field tags below CriticalMax are critical: a decoder that meets an unknown
// one must reject the update rather than act on a request it does not fully
// understand. Tags at or above CriticalMax are non-critical extensions: an
// unknown one is preserved and re-emitted (and, because it is inside the signed
// bytes, cannot be stripped without breaking the signature).
package refupdate

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"sort"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/sshkey"
)

// magic frames a canonical payload. A distinct family tag keeps ref updates
// from ever being confused with stored objects (which use "VVG1").
var magic = [4]byte{'V', 'R', 'U', '1'}

// CriticalMax is the exclusive upper bound of the critical tag range. Tags
// 1..CriticalMax-1 must be understood; tags >= CriticalMax are preserved when
// unknown.
const CriticalMax = 64

// Field tags. Append-only, exactly like the object format: new capabilities
// take new tags; existing tags never change meaning.
const (
	tagRef         = 1 // string: the ref name, e.g. "refs/heads/main"
	tagExpectedOld = 2 // multihash or empty: the CAS precondition (empty = absent)
	tagNew         = 3 // multihash or empty: the new value (empty = delete)
	tagScope       = 4 // string: the scope this update claims
	tagSigner      = 5 // 32 bytes: the signer's Ed25519 public key
	tagNonce       = 6 // 16 bytes: anti-replay nonce
	tagNotAfter    = 7 // uvarint: expiry, unix seconds
	tagEvidence    = 8 // list of multihashes: supporting objects (optional)
)

// NonceLen is the required nonce width (auth design §5.1, "random 16 bytes").
const NonceLen = 16

var (
	// ErrMalformed marks a payload that is not in canonical form.
	ErrMalformed = errors.New("refupdate: malformed payload")
	// ErrUnknownCritical is returned when decoding meets an unknown critical
	// field — the update must be rejected, not partially honored.
	ErrUnknownCritical = errors.New("refupdate: unknown critical field")
)

type field struct {
	tag uint64
	val []byte
}

// Payload is a decoded, canonical ref-update request. Unknown non-critical
// fields are retained in the field list so canonical bytes round-trip exactly.
type Payload struct {
	fields []field // invariant: sorted ascending by tag, unique
}

// Params are the inputs to a new payload. ExpectedOld nil means "the ref must
// be absent" (a creation); New nil means "delete the ref".
type Params struct {
	Ref         string
	ExpectedOld multihash.Multihash
	New         multihash.Multihash
	Scope       string
	SignerKey   ed25519.PublicKey
	Nonce       []byte
	NotAfter    int64
	Evidence    []multihash.Multihash
}

// New builds a canonical payload from typed parameters.
func New(p Params) (*Payload, error) {
	if p.Ref == "" {
		return nil, fmt.Errorf("%w: empty ref", ErrMalformed)
	}
	if len(p.SignerKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: signer key must be %d bytes", ErrMalformed, ed25519.PublicKeySize)
	}
	if len(p.Nonce) != NonceLen {
		return nil, fmt.Errorf("%w: nonce must be %d bytes", ErrMalformed, NonceLen)
	}
	if p.Scope == "" {
		p.Scope = "/"
	}
	fields := []field{
		{tag: tagRef, val: []byte(p.Ref)},
		{tag: tagExpectedOld, val: append([]byte(nil), p.ExpectedOld...)},
		{tag: tagNew, val: append([]byte(nil), p.New...)},
		{tag: tagScope, val: []byte(p.Scope)},
		{tag: tagSigner, val: append([]byte(nil), p.SignerKey...)},
		{tag: tagNonce, val: append([]byte(nil), p.Nonce...)},
		{tag: tagNotAfter, val: appendUvarint(nil, uint64(p.NotAfter))},
	}
	if len(p.Evidence) > 0 {
		fields = append(fields, field{tag: tagEvidence, val: encodeEvidence(p.Evidence)})
	}
	sort.Slice(fields, func(a, b int) bool { return fields[a].tag < fields[b].tag })
	return &Payload{fields: fields}, nil
}

func (p *Payload) get(tag uint64) ([]byte, bool) {
	for i := range p.fields {
		if p.fields[i].tag == tag {
			return p.fields[i].val, true
		}
	}
	return nil, false
}

// Ref returns the target ref name.
func (p *Payload) Ref() string { v, _ := p.get(tagRef); return string(v) }

// Scope returns the scope this update claims.
func (p *Payload) Scope() string { v, _ := p.get(tagScope); return string(v) }

// ExpectedOld returns the CAS precondition, or nil for "expect absent".
func (p *Payload) ExpectedOld() multihash.Multihash {
	v, _ := p.get(tagExpectedOld)
	if len(v) == 0 {
		return nil
	}
	return multihash.Multihash(append([]byte(nil), v...))
}

// New returns the new value, or nil for a deletion.
func (p *Payload) New() multihash.Multihash {
	v, _ := p.get(tagNew)
	if len(v) == 0 {
		return nil
	}
	return multihash.Multihash(append([]byte(nil), v...))
}

// SignerKey returns the signer's Ed25519 public key.
func (p *Payload) SignerKey() ed25519.PublicKey {
	v, _ := p.get(tagSigner)
	return ed25519.PublicKey(append([]byte(nil), v...))
}

// Fingerprint returns the signer's SSH SHA256 fingerprint — the identifier the
// trust store is keyed by (auth design §2.2). It is derived from the public key
// in the payload, so no separate signer field can disagree with the key that
// actually verifies the signature.
func (p *Payload) Fingerprint() string {
	return sshkey.PublicKey{Key: p.SignerKey()}.Fingerprint()
}

// Nonce returns the anti-replay nonce.
func (p *Payload) Nonce() []byte { v, _ := p.get(tagNonce); return append([]byte(nil), v...) }

// NotAfter returns the expiry in unix seconds.
func (p *Payload) NotAfter() int64 {
	v, ok := p.get(tagNotAfter)
	if !ok {
		return 0
	}
	n, _, err := readUvarint(v)
	if err != nil {
		return 0
	}
	return int64(n)
}

// Evidence returns the supporting object ids referenced by this update.
func (p *Payload) Evidence() ([]multihash.Multihash, error) {
	v, ok := p.get(tagEvidence)
	if !ok {
		return nil, nil
	}
	return decodeEvidence(v)
}

// CanonicalBytes returns the exact bytes that are signed and verified. Every
// field — including unknown non-critical ones — is emitted in tag order, so the
// signed byte string is reconstructible by any implementation (§5.1).
func (p *Payload) CanonicalBytes() []byte {
	var buf []byte
	buf = append(buf, magic[:]...)
	buf = appendUvarint(buf, uint64(len(p.fields)))
	for _, f := range p.fields {
		buf = appendUvarint(buf, f.tag)
		buf = appendUvarint(buf, uint64(len(f.val)))
		buf = append(buf, f.val...)
	}
	return buf
}

// Decode parses canonical payload bytes, enforcing canonical framing and the
// critical-field rule. It does not verify the signature or the request; that is
// the Verifier's job.
func Decode(b []byte) (*Payload, error) {
	c := &cursor{b: b}
	m, err := c.take(4)
	if err != nil || !bytes.Equal(m, magic[:]) {
		return nil, fmt.Errorf("%w: bad magic", ErrMalformed)
	}
	n, err := c.uvarint()
	if err != nil {
		return nil, err
	}
	fields := make([]field, 0, n)
	var prev uint64
	for i := uint64(0); i < n; i++ {
		tag, err := c.uvarint()
		if err != nil {
			return nil, err
		}
		if i > 0 && tag <= prev {
			return nil, fmt.Errorf("%w: fields not sorted/unique", ErrMalformed)
		}
		length, err := c.uvarint()
		if err != nil {
			return nil, err
		}
		val, err := c.take(length)
		if err != nil {
			return nil, err
		}
		if tag < CriticalMax && !known(tag) {
			return nil, fmt.Errorf("%w: tag %d", ErrUnknownCritical, tag)
		}
		fields = append(fields, field{tag: tag, val: append([]byte(nil), val...)})
		prev = tag
	}
	if !c.empty() {
		return nil, fmt.Errorf("%w: trailing bytes", ErrMalformed)
	}
	p := &Payload{fields: fields}
	if err := p.validate(); err != nil {
		return nil, err
	}
	return p, nil
}

// validate checks that all required critical fields are present and well-sized.
func (p *Payload) validate() error {
	for _, tag := range []uint64{tagRef, tagExpectedOld, tagNew, tagScope, tagSigner, tagNonce, tagNotAfter} {
		if _, ok := p.get(tag); !ok {
			return fmt.Errorf("%w: missing required field %d", ErrMalformed, tag)
		}
	}
	if len(p.SignerKey()) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: bad signer key length", ErrMalformed)
	}
	if len(p.Nonce()) != NonceLen {
		return fmt.Errorf("%w: bad nonce length", ErrMalformed)
	}
	if _, err := p.Evidence(); err != nil {
		return err
	}
	return nil
}

func known(tag uint64) bool {
	switch tag {
	case tagRef, tagExpectedOld, tagNew, tagScope, tagSigner, tagNonce, tagNotAfter, tagEvidence:
		return true
	default:
		return false
	}
}

// --- evidence list: count uvarint, then sorted-unique length-prefixed ids ---

func encodeEvidence(ids []multihash.Multihash) []byte {
	sorted := append([]multihash.Multihash(nil), ids...)
	sort.Slice(sorted, func(a, b int) bool { return bytes.Compare(sorted[a], sorted[b]) < 0 })
	uniq := sorted[:0]
	for i, id := range sorted {
		if i > 0 && bytes.Equal(id, sorted[i-1]) {
			continue
		}
		uniq = append(uniq, id)
	}
	var b []byte
	b = appendUvarint(b, uint64(len(uniq)))
	for _, id := range uniq {
		b = appendUvarint(b, uint64(len(id)))
		b = append(b, id...)
	}
	return b
}

func decodeEvidence(v []byte) ([]multihash.Multihash, error) {
	c := &cursor{b: v}
	n, err := c.uvarint()
	if err != nil {
		return nil, err
	}
	var out []multihash.Multihash
	var prev []byte
	for i := uint64(0); i < n; i++ {
		l, err := c.uvarint()
		if err != nil {
			return nil, err
		}
		id, err := c.take(l)
		if err != nil {
			return nil, err
		}
		if i > 0 && bytes.Compare(id, prev) <= 0 {
			return nil, fmt.Errorf("%w: evidence not sorted/unique", ErrMalformed)
		}
		out = append(out, multihash.Multihash(append([]byte(nil), id...)))
		prev = id
	}
	if !c.empty() {
		return nil, fmt.Errorf("%w: trailing bytes in evidence", ErrMalformed)
	}
	return out, nil
}
