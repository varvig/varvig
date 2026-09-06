package graphnode

// A node's Key is its canonical wire spelling: the form an edge record stores
// and a query returns. It is deliberately textual and self-describing, so an
// older binary that has never heard of a node class still round-trips one
// untouched (§4.4, A3's general form) rather than dropping or rewriting it.
//
// The class prefix is what makes the encoding unambiguous. Without it an object
// node and an ephemeral node — both a bare hash — would be indistinguishable,
// and their retention rules are opposite.

import (
	"fmt"
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
)

// Class prefixes. These are format, so they are fixed: changing one would
// invalidate every stored edge. They are short because every edge stores two.
const (
	prefixObject    = "obj"
	prefixIdentity  = "id"
	prefixExternal  = "ext"
	prefixEphemeral = "eph"
)

// keySeparator divides an identity node's ref from the revision it resolved to.
// It is "@" because a ref name cannot contain one, so the split is unambiguous
// even for a ref with slashes in it.
const keySeparator = "@"

// ParseKey decodes a canonical key back into a node. It is the inverse of
// Key() for every node this package can build.
//
// An unrecognized class prefix is an error rather than a silently ignored node:
// a reader that cannot understand an endpoint must say so, because treating it
// as absent is how a coverage gap becomes a wrong answer (design §5). Preserving
// an unknown node for round-trip is the storage layer's job, not the decoder's.
func ParseKey(key string) (Node, error) {
	prefix, rest, ok := strings.Cut(key, ":")
	if !ok {
		return nil, fmt.Errorf("graphnode: %q is not a node key", key)
	}
	switch prefix {
	case prefixObject:
		id, err := multihash.ParseHex(rest)
		if err != nil {
			return nil, fmt.Errorf("graphnode: object key %q: %w", key, err)
		}
		return Object(id)
	case prefixEphemeral:
		id, err := multihash.ParseHex(rest)
		if err != nil {
			return nil, fmt.Errorf("graphnode: ephemeral key %q: %w", key, err)
		}
		return Ephemeral(id)
	case prefixIdentity:
		ref, hex, ok := strings.Cut(rest, keySeparator)
		if !ok {
			return nil, fmt.Errorf("graphnode: identity key %q has no revision", key)
		}
		rev, err := multihash.ParseHex(hex)
		if err != nil {
			return nil, fmt.Errorf("graphnode: identity key %q: %w", key, err)
		}
		return Identity(ref, rev)
	case prefixExternal:
		system, foreign, ok := strings.Cut(rest, ":")
		if !ok {
			return nil, fmt.Errorf("graphnode: external key %q has no foreign id", key)
		}
		return External(system, foreign)
	}
	return nil, fmt.Errorf("graphnode: unknown node class %q in key %q", prefix, key)
}
