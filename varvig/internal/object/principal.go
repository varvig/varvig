package object

import (
	"crypto/ed25519"
	"fmt"
)

// Kind classifies a principal (tickets §1.4). The object format does not
// distinguish human from agent anywhere else — a director may be either, and
// switching one for the other is a personnel change, not a schema change — but
// a bridge is different in kind: it holds a key and asserts on behalf of a
// keyless principal, so its attestations can only ever be weak (tickets §2.4).
type Kind uint64

const (
	KindUnknown Kind = 0
	KindHuman   Kind = 1
	KindAgent   Kind = 2
	KindBridge  Kind = 3
)

func (k Kind) String() string {
	switch k {
	case KindHuman:
		return "human"
	case KindAgent:
		return "agent"
	case KindBridge:
		return "bridge"
	default:
		return fmt.Sprintf("kind-%d", uint64(k))
	}
}

// Principal is a keyholder: a public key, a display name, and a kind
// (tickets §1.4). Principal records are content-addressed objects, so the org
// chart is versioned, hash-pinned, diffable, and auditable — including
// retroactively. Agent-specific provenance fields (model, version, delegated
// authority) are additive future tags that older builds preserve untouched.
type Principal struct {
	Key  ed25519.PublicKey
	Name string
	Kind Kind
}

// NewPrincipal builds a principal object.
func NewPrincipal(p Principal) *Object {
	fields := []field{
		{tag: tagPrincipalKey, val: append([]byte(nil), p.Key...)},
		{tag: tagPrincipalName, val: []byte(p.Name)},
		{tag: tagPrincipalKind, val: appendUvarint(nil, uint64(p.Kind))},
	}
	return newObject(TypePrincipal, fields)
}

// AsPrincipal decodes the typed view of a principal object.
func (o *Object) AsPrincipal() (Principal, error) {
	if o.typ != TypePrincipal {
		return Principal{}, fmt.Errorf("object: not a principal (%s)", o.typ)
	}
	var p Principal
	if v, ok := o.Field(tagPrincipalKey); ok {
		p.Key = ed25519.PublicKey(append([]byte(nil), v...))
	}
	if v, ok := o.Field(tagPrincipalName); ok {
		p.Name = string(v)
	}
	k, err := readUvarintField(o, tagPrincipalKind)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: bad principal kind", ErrMalformed)
	}
	p.Kind = Kind(k)
	return p, nil
}
