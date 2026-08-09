// Package sshkey implements just enough of the SSH key formats to reuse a
// user's existing Ed25519 identity (auth design §2.1, "reuse SSH keys — users
// already have ~/.ssh/id_ed25519"). It is deliberately hand-rolled and cgo-free
// so the one portable binary (design §3) gains no dependency and no toolchain
// requirement.
//
// Scope is narrow on purpose: Ed25519 only (auth design §2.1, "no negotiation,
// no alternatives at v1"). It parses SSH public keys (the authorized_keys /
// *.pub line), renders the standard SHA256 fingerprint, reads unencrypted
// OpenSSH private keys, and speaks the ssh-agent protocol for listing and
// signing. Encrypted private keys are not decrypted here — the user is directed
// to ssh-agent, which is the intended path for a key protected at rest.
//
// # SSH wire encoding
//
// Every structure below uses the SSH convention: a "string" is a big-endian
// uint32 length followed by that many bytes. This is distinct from Varvig's own
// object encoding (minimal varints; see package object) and is confined to this
// package.
package sshkey

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// KeyTypeEd25519 is the only SSH key algorithm Varvig accepts (auth design §2.1).
const KeyTypeEd25519 = "ssh-ed25519"

var (
	// ErrUnsupportedType is returned for any SSH key type other than Ed25519.
	ErrUnsupportedType = errors.New("sshkey: unsupported key type (only ssh-ed25519)")
	// ErrMalformed marks input that is not valid SSH wire framing.
	ErrMalformed = errors.New("sshkey: malformed encoding")
	// ErrEncrypted is returned when a private key is passphrase-protected; use
	// ssh-agent instead of decrypting at rest.
	ErrEncrypted = errors.New("sshkey: private key is encrypted (use ssh-agent)")
)

// PublicKey is a parsed Ed25519 SSH public key together with its optional
// comment (the trailing free-text field of an authorized_keys line).
type PublicKey struct {
	Key     ed25519.PublicKey
	Comment string
}

// Blob returns the canonical SSH wire encoding of the public key:
//
//	string  "ssh-ed25519"
//	string  32-byte public key
//
// This blob is what the fingerprint hashes and what an authorized_keys line
// base64-encodes, so it is the stable identity of the key.
func (p PublicKey) Blob() []byte {
	var b []byte
	b = appendString(b, []byte(KeyTypeEd25519))
	b = appendString(b, p.Key)
	return b
}

// Fingerprint returns the standard OpenSSH SHA256 fingerprint of the key, e.g.
// "SHA256:aXk9Lm4Qr…" — base64 (no padding) of the SHA-256 of the wire blob.
// This is the form users paste from `ssh-keygen -lf` and `ssh-add -l`, so
// Varvig accepts and displays it directly (auth design §2.2).
func (p PublicKey) Fingerprint() string {
	return FingerprintBlob(p.Blob())
}

// FingerprintBlob computes the SSH SHA256 fingerprint of an already-encoded
// public-key blob.
func FingerprintBlob(blob []byte) string {
	sum := sha256.Sum256(blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// ParseAuthorizedKey parses one authorized_keys / *.pub line:
//
//	ssh-ed25519 <base64-blob> [comment...]
//
// Leading options are not supported (they do not appear in a *.pub file and are
// out of scope for a trust store keyed by fingerprint). A blank or comment line
// yields ErrMalformed; callers that tolerate those should check first.
func ParseAuthorizedKey(line string) (PublicKey, error) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 2 {
		return PublicKey{}, fmt.Errorf("%w: expected 'ssh-ed25519 <base64> [comment]'", ErrMalformed)
	}
	if fields[0] != KeyTypeEd25519 {
		return PublicKey{}, ErrUnsupportedType
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return PublicKey{}, fmt.Errorf("%w: bad base64", ErrMalformed)
	}
	pk, err := ParsePublicBlob(blob)
	if err != nil {
		return PublicKey{}, err
	}
	// The comment is everything after the base64 field, space-joined verbatim.
	if len(fields) > 2 {
		pk.Comment = strings.Join(fields[2:], " ")
	}
	return pk, nil
}

// ParsePublicBlob decodes an SSH wire public-key blob into a PublicKey. The blob
// must name ssh-ed25519 and carry a 32-byte key with no trailing bytes.
func ParsePublicBlob(blob []byte) (PublicKey, error) {
	r := reader{b: blob}
	typ, err := r.string()
	if err != nil {
		return PublicKey{}, err
	}
	if string(typ) != KeyTypeEd25519 {
		return PublicKey{}, ErrUnsupportedType
	}
	key, err := r.string()
	if err != nil {
		return PublicKey{}, err
	}
	if len(key) != ed25519.PublicKeySize {
		return PublicKey{}, fmt.Errorf("%w: ed25519 public key must be %d bytes", ErrMalformed, ed25519.PublicKeySize)
	}
	if !r.empty() {
		return PublicKey{}, fmt.Errorf("%w: trailing bytes after public key", ErrMalformed)
	}
	return PublicKey{Key: ed25519.PublicKey(append([]byte(nil), key...))}, nil
}

// AuthorizedLine renders a PublicKey back to a single authorized_keys line
// (without a trailing newline). The comment is included when non-empty.
func (p PublicKey) AuthorizedLine() string {
	line := KeyTypeEd25519 + " " + base64.StdEncoding.EncodeToString(p.Blob())
	if p.Comment != "" {
		line += " " + p.Comment
	}
	return line
}

// --- OpenSSH private key ("-----BEGIN OPENSSH PRIVATE KEY-----") ---

const opensshMagic = "openssh-key-v1\x00"

// ParseOpenSSHPrivateKey decodes an unencrypted OpenSSH-format Ed25519 private
// key (the modern default written by `ssh-keygen -t ed25519`). Encrypted keys
// return ErrEncrypted; the caller should fall back to ssh-agent.
func ParseOpenSSHPrivateKey(pem []byte) (ed25519.PrivateKey, PublicKey, error) {
	body, err := decodePEM(pem, "OPENSSH PRIVATE KEY")
	if err != nil {
		return nil, PublicKey{}, err
	}
	if len(body) < len(opensshMagic) || string(body[:len(opensshMagic)]) != opensshMagic {
		return nil, PublicKey{}, fmt.Errorf("%w: not an openssh-key-v1 blob", ErrMalformed)
	}
	r := reader{b: body[len(opensshMagic):]}
	cipher, err := r.string()
	if err != nil {
		return nil, PublicKey{}, err
	}
	kdf, err := r.string()
	if err != nil {
		return nil, PublicKey{}, err
	}
	if _, err := r.string(); err != nil { // kdfoptions (empty for cipher "none")
		return nil, PublicKey{}, err
	}
	if string(cipher) != "none" || string(kdf) != "none" {
		return nil, PublicKey{}, ErrEncrypted
	}
	nkeys, err := r.uint32()
	if err != nil {
		return nil, PublicKey{}, err
	}
	if nkeys != 1 {
		return nil, PublicKey{}, fmt.Errorf("%w: expected exactly one key, got %d", ErrMalformed, nkeys)
	}
	pubBlob, err := r.string()
	if err != nil {
		return nil, PublicKey{}, err
	}
	pub, err := ParsePublicBlob(pubBlob)
	if err != nil {
		return nil, PublicKey{}, err
	}
	priv, err := r.string()
	if err != nil {
		return nil, PublicKey{}, err
	}
	if !r.empty() {
		return nil, PublicKey{}, fmt.Errorf("%w: trailing bytes after private section", ErrMalformed)
	}

	// Private section (unencrypted): two check ints, then per-key fields.
	pr := reader{b: priv}
	c1, err := pr.uint32()
	if err != nil {
		return nil, PublicKey{}, err
	}
	c2, err := pr.uint32()
	if err != nil {
		return nil, PublicKey{}, err
	}
	if c1 != c2 {
		// A mismatch is the canonical signal of a wrong passphrase, but with
		// cipher "none" it just means corruption.
		return nil, PublicKey{}, fmt.Errorf("%w: private-key check bytes differ", ErrMalformed)
	}
	ktype, err := pr.string()
	if err != nil {
		return nil, PublicKey{}, err
	}
	if string(ktype) != KeyTypeEd25519 {
		return nil, PublicKey{}, ErrUnsupportedType
	}
	if _, err := pr.string(); err != nil { // public key, repeated
		return nil, PublicKey{}, err
	}
	privKey, err := pr.string()
	if err != nil {
		return nil, PublicKey{}, err
	}
	if len(privKey) != ed25519.PrivateKeySize {
		return nil, PublicKey{}, fmt.Errorf("%w: ed25519 private key must be %d bytes", ErrMalformed, ed25519.PrivateKeySize)
	}
	comment, err := pr.string()
	if err != nil {
		return nil, PublicKey{}, err
	}
	pub.Comment = string(comment)

	sk := ed25519.PrivateKey(append([]byte(nil), privKey...))
	// The embedded public key must match the one derived from the private key,
	// or the file is inconsistent and signatures would not verify.
	if derived, ok := sk.Public().(ed25519.PublicKey); !ok || !derived.Equal(pub.Key) {
		return nil, PublicKey{}, fmt.Errorf("%w: private key does not match its public key", ErrMalformed)
	}
	return sk, pub, nil
}

// decodePEM extracts the base64 body of a single PEM block with the given type,
// tolerating CRLF and surrounding whitespace. It is a minimal reader — no
// headers, one block — sufficient for OpenSSH private keys.
func decodePEM(pem []byte, typ string) ([]byte, error) {
	begin := "-----BEGIN " + typ + "-----"
	end := "-----END " + typ + "-----"
	s := string(pem)
	i := strings.Index(s, begin)
	if i < 0 {
		return nil, fmt.Errorf("%w: missing %q", ErrMalformed, begin)
	}
	s = s[i+len(begin):]
	j := strings.Index(s, end)
	if j < 0 {
		return nil, fmt.Errorf("%w: missing %q", ErrMalformed, end)
	}
	var body strings.Builder
	for _, line := range strings.Split(s[:j], "\n") {
		body.WriteString(strings.TrimSpace(line))
	}
	raw, err := base64.StdEncoding.DecodeString(body.String())
	if err != nil {
		return nil, fmt.Errorf("%w: bad base64 in PEM body", ErrMalformed)
	}
	return raw, nil
}

// --- SSH wire helpers (big-endian uint32 length-prefixed strings) ---

func appendString(b, s []byte) []byte {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(s)))
	b = append(b, n[:]...)
	return append(b, s...)
}

type reader struct {
	b []byte
	i int
}

func (r *reader) uint32() (uint32, error) {
	if r.i+4 > len(r.b) {
		return 0, fmt.Errorf("%w: truncated uint32", ErrMalformed)
	}
	v := binary.BigEndian.Uint32(r.b[r.i:])
	r.i += 4
	return v, nil
}

func (r *reader) string() ([]byte, error) {
	n, err := r.uint32()
	if err != nil {
		return nil, err
	}
	if r.i+int(n) > len(r.b) || int(n) < 0 {
		return nil, fmt.Errorf("%w: truncated string", ErrMalformed)
	}
	s := r.b[r.i : r.i+int(n)]
	r.i += int(n)
	return s, nil
}

func (r *reader) empty() bool { return r.i == len(r.b) }
