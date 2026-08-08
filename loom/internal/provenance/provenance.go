// Package provenance signs change objects and builds their provenance records
// (design §2.1, promoted from Git's bolted-on signing to a load-bearing part of
// the model). When most changes come from non-humans, a verifiable audit chain
// — model, version, sampling, tool permissions, the acting authority, and the
// hash of the tool binary — is the only basis on which a deploy can be trusted.
//
// Signing uses Ed25519 (pure Go, no cgo). A signature covers the change's
// SignableBytes: everything except the signature field, which includes the
// provenance object's id and therefore transitively commits to the provenance
// content.
package provenance

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dividebyzero/claude-experiments/loom/internal/multihash"
	"github.com/dividebyzero/claude-experiments/loom/internal/object"
)

// Signature scheme identifiers, stored as the first field of the signature blob.
const schemeEd25519 uint64 = 1

var (
	// ErrUnsigned is returned when a change carries no signature.
	ErrUnsigned = errors.New("provenance: change is not signed")
	// ErrBadSignature is returned when a signature does not verify.
	ErrBadSignature = errors.New("provenance: signature does not verify")
	// ErrUnknownScheme is returned for an unrecognized signature scheme.
	ErrUnknownScheme = errors.New("provenance: unknown signature scheme")
	// ErrNoProvenance is returned when a change references no provenance object.
	ErrNoProvenance = errors.New("provenance: change has no provenance record")
)

// Sign computes an Ed25519 signature over the change's SignableBytes and
// attaches it. The change must already carry its provenance reference, since
// SignableBytes covers it.
func Sign(change *object.Object, priv ed25519.PrivateKey) error {
	if change.Type() != object.TypeChange {
		return fmt.Errorf("provenance: cannot sign a %s", change.Type())
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return errors.New("provenance: bad private key")
	}
	sig := ed25519.Sign(priv, change.SignableBytes())
	change.SetSignature(encodeSig(schemeEd25519, pub, sig))
	return nil
}

// Verify checks a change's signature and returns the signing public key.
func Verify(change *object.Object) (ed25519.PublicKey, error) {
	blob, ok := change.RawSignature()
	if !ok {
		return nil, ErrUnsigned
	}
	scheme, pub, sig, err := decodeSig(blob)
	if err != nil {
		return nil, err
	}
	if scheme != schemeEd25519 {
		return nil, ErrUnknownScheme
	}
	if len(pub) != ed25519.PublicKeySize {
		return nil, ErrBadSignature
	}
	if !ed25519.Verify(pub, change.SignableBytes(), sig) {
		return nil, ErrBadSignature
	}
	return pub, nil
}

// signature blob = uvarint(scheme) bytes(pubkey) bytes(sig)

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

// --- identity ---

// LoadOrCreateIdentity returns the repository's Ed25519 signing key, generating
// and persisting one (0600) on first use. The key lives under the metadata
// directory, outside the object store and never synced.
func LoadOrCreateIdentity(gitDir string) (ed25519.PrivateKey, error) {
	dir := filepath.Join(gitDir, "identity")
	seedPath := filepath.Join(dir, "ed25519.seed")
	if seed, err := os.ReadFile(seedPath); err == nil {
		if len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("provenance: corrupt identity seed (%d bytes)", len(seed))
		}
		return ed25519.NewKeyFromSeed(seed), nil
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(seedPath, seed, 0o600); err != nil {
		return nil, err
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// Build assembles a provenance record from the acting authority, the ambient
// environment (LOOM_MODEL, LOOM_MODEL_VERSION, LOOM_SAMPLING, LOOM_TOOL_PERMS,
// LOOM_TASK, LOOM_CONTEXT, LOOM_REASONING), and the tool binary's own hash.
func Build(authority string) object.Provenance {
	return object.Provenance{
		Authority:       authority,
		Model:           os.Getenv("LOOM_MODEL"),
		ModelVersion:    os.Getenv("LOOM_MODEL_VERSION"),
		Sampling:        os.Getenv("LOOM_SAMPLING"),
		ToolPermissions: splitList(os.Getenv("LOOM_TOOL_PERMS")),
		ToolHash:        hashSelf(),
		TaskSpec:        os.Getenv("LOOM_TASK"),
		ContextRead:     os.Getenv("LOOM_CONTEXT"),
		Reasoning:       os.Getenv("LOOM_REASONING"),
	}
}

// hashSelf returns the multihash of the running binary, best-effort.
func hashSelf() multihash.Multihash {
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		return nil
	}
	mh, err := multihash.Sum(multihash.Default, data)
	if err != nil {
		return nil
	}
	return mh
}

func splitList(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' || r == ' ' || r == '\n' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// --- minimal varint / length-prefix helpers (shared discipline) ---

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
			return 0, 0, errors.New("provenance: varint overflow")
		}
		if ch < 0x80 {
			if i == 9 && ch > 1 {
				return 0, 0, errors.New("provenance: varint overflow")
			}
			if ch == 0 && i != 0 {
				return 0, 0, errors.New("provenance: non-minimal varint")
			}
			return x | uint64(ch)<<s, i + 1, nil
		}
		x |= uint64(ch&0x7f) << s
		s += 7
	}
	return 0, 0, errors.New("provenance: truncated varint")
}

func takeBytes(b []byte) (val, rest []byte, err error) {
	n, k, err := readUvarint(b)
	if err != nil {
		return nil, nil, err
	}
	b = b[k:]
	if n > uint64(len(b)) {
		return nil, nil, errors.New("provenance: truncated field")
	}
	return b[:n], b[n:], nil
}
