// Package multihash implements a small subset of the multiformats multihash
// specification: a self-describing digest of the form
//
//	<uvarint hash-code> <uvarint digest-length> <digest bytes>
//
// Loom uses self-describing digests from day one (design §4.5, "hash agility
// is the one thing you cannot retrofit"). The object format never names a
// hash algorithm; every object identity carries its own algorithm code, so a
// future dual-hash transition needs no format change — only a new code in the
// registry below and a translation table.
package multihash

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/zeebo/blake3"

	"crypto/sha256"
)

// Code identifies a hash algorithm. Values match the multiformats registry.
type Code uint64

const (
	// SHA2_256 is provided for interoperability and as the second leg of any
	// future dual-hash transition.
	SHA2_256 Code = 0x12
	// BLAKE3 is Loom's default digest.
	BLAKE3 Code = 0x1e
)

// Default is the algorithm new objects are written with. Because identities
// are self-describing, this is a policy choice — not part of the frozen
// format — and may change without breaking readers of existing objects.
const Default = BLAKE3

type algo struct {
	name string
	size int
	sum  func([]byte) []byte
}

var registry = map[Code]algo{
	SHA2_256: {name: "sha2-256", size: 32, sum: func(b []byte) []byte { s := sha256.Sum256(b); return s[:] }},
	BLAKE3:   {name: "blake3", size: 32, sum: func(b []byte) []byte { s := blake3.Sum256(b); return s[:] }},
}

// Registered reports whether c is a known algorithm in this build.
func Registered(c Code) bool { _, ok := registry[c]; return ok }

// Name returns the human-readable name of a code, or "unknown-0x..".
func Name(c Code) string {
	if a, ok := registry[c]; ok {
		return a.name
	}
	return fmt.Sprintf("unknown-0x%x", uint64(c))
}

var (
	// ErrUnknownCode is returned when an algorithm code is not registered.
	ErrUnknownCode = errors.New("multihash: unknown hash code")
	// ErrMalformed is returned when a byte slice is not a valid multihash.
	ErrMalformed = errors.New("multihash: malformed digest")
	// ErrWrongLength is returned when the declared length does not match the
	// registered digest size for a known algorithm.
	ErrWrongLength = errors.New("multihash: digest length mismatch")
)

// Multihash is the self-describing digest: code || length || digest.
type Multihash []byte

// Sum hashes data with algorithm c and returns the encoded multihash.
func Sum(c Code, data []byte) (Multihash, error) {
	a, ok := registry[c]
	if !ok {
		return nil, ErrUnknownCode
	}
	return encode(c, a.sum(data)), nil
}

func encode(c Code, digest []byte) Multihash {
	buf := make([]byte, 0, binary.MaxVarintLen64*2+len(digest))
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], uint64(c))
	buf = append(buf, tmp[:n]...)
	n = binary.PutUvarint(tmp[:], uint64(len(digest)))
	buf = append(buf, tmp[:n]...)
	buf = append(buf, digest...)
	return buf
}

// Decode validates m and returns its algorithm code and raw digest. It refuses
// non-minimal varints and trailing bytes so that a multihash has exactly one
// valid byte encoding (required for stable identities).
func Decode(m Multihash) (Code, []byte, error) {
	code, n1, err := readUvarint(m)
	if err != nil {
		return 0, nil, ErrMalformed
	}
	length, n2, err := readUvarint(m[n1:])
	if err != nil {
		return 0, nil, ErrMalformed
	}
	rest := m[n1+n2:]
	if uint64(len(rest)) != length {
		return 0, nil, ErrMalformed
	}
	return Code(code), rest, nil
}

// Verify reports whether data hashes, under m's own algorithm, to m.
func Verify(m Multihash, data []byte) (bool, error) {
	code, digest, err := Decode(m)
	if err != nil {
		return false, err
	}
	a, ok := registry[Code(code)]
	if !ok {
		return false, ErrUnknownCode
	}
	got := a.sum(data)
	if len(got) != len(digest) {
		return false, nil
	}
	for i := range got {
		if got[i] != digest[i] {
			return false, nil
		}
	}
	return true, nil
}

// Code returns the algorithm code of a well-formed multihash, or false.
func (m Multihash) Code() (Code, bool) {
	c, _, err := Decode(m)
	if err != nil {
		return 0, false
	}
	return c, true
}

// Digest returns the raw digest bytes of a well-formed multihash.
func (m Multihash) Digest() []byte {
	_, d, err := Decode(m)
	if err != nil {
		return nil
	}
	return d
}

// Hex returns the lowercase hex encoding of the whole multihash. This is the
// canonical textual form used for filenames and display.
func (m Multihash) Hex() string { return hex.EncodeToString(m) }

// String implements fmt.Stringer.
func (m Multihash) String() string {
	c, d, err := Decode(m)
	if err != nil {
		return "<invalid-multihash>"
	}
	return fmt.Sprintf("%s:%s", Name(Code(c)), hex.EncodeToString(d))
}

// Equal reports whether two multihashes are byte-identical.
func (m Multihash) Equal(o Multihash) bool {
	if len(m) != len(o) {
		return false
	}
	for i := range m {
		if m[i] != o[i] {
			return false
		}
	}
	return true
}

// ParseHex decodes a multihash from its Hex() form and validates its framing.
func ParseHex(s string) (Multihash, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, ErrMalformed
	}
	if _, _, err := Decode(Multihash(b)); err != nil {
		return nil, err
	}
	return Multihash(b), nil
}

// readUvarint decodes a base-128 varint, rejecting non-minimal encodings and
// overlong sequences. A minimal encoding is required so that every value has
// exactly one byte representation.
func readUvarint(b []byte) (val uint64, n int, err error) {
	var x uint64
	var s uint
	for i := 0; i < len(b); i++ {
		c := b[i]
		if i == 10 {
			return 0, 0, ErrMalformed
		}
		if c < 0x80 {
			if i == 9 && c > 1 {
				return 0, 0, ErrMalformed
			}
			if c == 0 && i != 0 {
				// A trailing 0x00 continuation byte means a non-minimal encoding.
				return 0, 0, ErrMalformed
			}
			return x | uint64(c)<<s, i + 1, nil
		}
		x |= uint64(c&0x7f) << s
		s += 7
	}
	return 0, 0, ErrMalformed
}
