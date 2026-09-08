package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/refs"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// Remote-tracking refs are per peer, because the lease they carry is about one
// peer and means nothing about another.
//
// A push sends the tracking value as its compare-and-swap lease: "advance the
// branch only if it is still where I last saw it" (force-with-lease, §2). With a
// single refs/remotes/origin/<branch> that value was whichever peer was fetched
// from last, so in a mesh a push to any *other* peer carried a lease describing
// somebody else's repository. At most one peer could accept a head push, and
// which one depended on fetch order.
//
// It failed safely — a refused compare-and-swap, never an overwrite — but safe
// is not the same as working: a factory whose members legitimately differ on the
// branch could not converge except by relay through whichever peer happened to
// be last.
//
// One ref per peer makes the lease mean what the push says it means.

const legacyTrackingPrefix = "refs/remotes/origin/"

// trackingRef names the local record of where a peer's branch was last seen.
func trackingRef(addr, branch string) string {
	return "refs/remotes/" + peerSegment(addr) + "/" + branch
}

// peerSegment turns a peer address into exactly one ref path segment.
//
// The encoding is injective, so two addresses never share a record, and it
// leaves the common shape readable: 127.0.0.1:9418 becomes 127.0.0.1%3A9418,
// which is still recognisably that peer in `varvig show-ref`.
//
// Everything outside [A-Za-z0-9._-] is percent-encoded, including ':' — a ref
// name becomes a file path, and ':' is illegal in a filename on Windows, which
// this binary cross-compiles for. '%' is encoded too, which is what keeps the
// mapping injective rather than merely tidy.
func peerSegment(addr string) string {
	var b strings.Builder
	for i := 0; i < len(addr); i++ {
		c := addr[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '.', c == '_', c == '-':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	s := b.String()
	switch s {
	case "":
		// An empty address cannot name a peer, but it must not produce an empty
		// segment either — that is a ref name the store rejects, and the refusal
		// would name the ref rather than the real mistake.
		return "%00"
	case ".", "..":
		// Both are rejected as path segments, and both are reachable from an
		// address of "." or "..". Encode the leading dot to keep the mapping
		// total.
		return "%2E" + s[1:]
	}
	return s
}

// trackedTip reads what this repository last saw of a peer's branch.
//
// A repository cloned before tracking refs were per peer has only the legacy
// refs/remotes/origin/<branch>, so that is consulted when the per-peer record is
// absent. It is a migration path and nothing more: the per-peer ref is written
// from then on, and after one fetch the legacy value is never read again.
//
// Falling back is safe even when the legacy value describes a different peer.
// The worst case is a lease that peer refuses, which is the same refusal the
// single-ref scheme produced every time — and one fetch replaces it with a real
// observation.
func trackedTip(r *repo.Repo, addr, branch string) multihash.Multihash {
	if id, err := r.Refs.Resolve(trackingRef(addr, branch)); err == nil {
		return id
	} else if !errors.Is(err, refs.ErrNotExist) {
		return nil
	}
	if id, err := r.Refs.Resolve(legacyTrackingPrefix + branch); err == nil {
		return id
	}
	return nil
}

// recordTip advances this repository's record of where a peer's branch is.
func recordTip(r *repo.Repo, addr, branch string, tip multihash.Multihash, actor, msg string) error {
	name := trackingRef(addr, branch)
	cur, err := r.Refs.Resolve(name)
	if err != nil && !errors.Is(err, refs.ErrNotExist) {
		return err
	}
	if cur != nil && cur.Equal(tip) {
		return nil
	}
	return r.Refs.CompareAndSwap(name, cur, tip, actor, msg)
}
