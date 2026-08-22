// Package pin encodes cross-peer retention pins as ordinary refs (federation
// §3). A pin is a ref under refs/pins/<peer>/<not_after>/<hash> whose value is
// the pinned object, so it is a GC root with no new primitive — the only added
// semantics are per-peer quota and expiry, both handled by the peers that read
// these names (the p2p pin handlers and GC's root walk).
//
// The peer id is hex-encoded into its path segment so an arbitrary identity
// string (which may contain '/') is always a single, safe segment. not_after is
// a fixed-width hex of unix seconds, so a pin is self-describing: its expiry is
// recoverable from the ref name alone, with no dependence on reflog messages
// that retention may compact away.
package pin

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
)

// Prefix is the ref namespace all pins live under.
const Prefix = "refs/pins/"

// MaxPerPeer bounds how many live pins one peer may hold, so a peer can never
// exhaust another's disk by pinning (§3: quota'd, refusal is a normal response).
// A var, not a const, so a deployment (or a test) can tune the quota.
var MaxPerPeer = 4096

// RefName builds the ref name for a pin.
func RefName(peerID string, notAfter int64, hash multihash.Multihash) string {
	return fmt.Sprintf("%s%s/%016x/%s", Prefix, encodePeer(peerID), notAfter, hash.Hex())
}

// PeerPrefix is the ref-name prefix enumerating one peer's pins.
func PeerPrefix(peerID string) string {
	return Prefix + encodePeer(peerID) + "/"
}

// IsPinRef reports whether name is a pin ref.
func IsPinRef(name string) bool { return strings.HasPrefix(name, Prefix) }

// Parse decodes a pin ref name into its parts. ok is false for any name that is
// not a well-formed pin ref.
func Parse(name string) (peerID string, notAfter int64, hash multihash.Multihash, ok bool) {
	rest, found := strings.CutPrefix(name, Prefix)
	if !found {
		return "", 0, nil, false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return "", 0, nil, false
	}
	peer, err := decodePeer(parts[0])
	if err != nil {
		return "", 0, nil, false
	}
	na, err := strconv.ParseInt(parts[1], 16, 64)
	if err != nil {
		return "", 0, nil, false
	}
	h, err := multihash.ParseHex(parts[2])
	if err != nil {
		return "", 0, nil, false
	}
	return peer, na, h, true
}

func encodePeer(peerID string) string { return hex.EncodeToString([]byte(peerID)) }

func decodePeer(s string) (string, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
