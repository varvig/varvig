package principal

import (
	"crypto/ed25519"

	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
)

// Kind is the principal-kind vocabulary, re-exported so a caller names the domain
// (`principal.KindHuman`) rather than the wire-format package (design addendum,
// U1). It is a type alias, so Registry methods that take an object.Kind accept
// these unchanged.
type Kind = object.Kind

const (
	KindUnknown = object.KindUnknown
	KindHuman   = object.KindHuman
	KindAgent   = object.KindAgent
	KindBridge  = object.KindBridge
)

// NewPrincipal builds a principal record from a public key, display name, and
// kind, so a caller can register one without naming the underlying object type.
func NewPrincipal(key ed25519.PublicKey, name string, kind Kind) object.Principal {
	return object.Principal{Key: key, Name: name, Kind: kind}
}
