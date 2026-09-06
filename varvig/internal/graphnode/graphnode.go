// Package graphnode is the identity of a context-graph endpoint (GRAPH.md §3.1).
//
// The rule the whole package exists to enforce: **no node is a bare string.**
// `src/auth.ts` is not a node. It is one *resolution* of a file identity, true
// under one tree and false under the next, and an edge bound to the string
// silently points at a different thing after a rename while continuing to read
// as valid. That is A4 for the fourth time — an approval bound to a ticket id, a
// commit bound to a ticket id, evidence bound to a proposal id — and it is fixed
// the same way each time: bind to something immutable.
//
// So there are four classes, and each binds to something that cannot change
// underneath an edge:
//
//	Object     a content hash                      commit, tree, blob, revision, note, module
//	Identity   a ref plus the revision it resolved to    ticket, symbol, file across time, principal
//	External   <system>:<foreign-id>                a row in a foreign system
//	Ephemeral  a content hash, retention-governed   speculation state, task execution
//
// Node is a sealed interface: the only implementations are the four below, each
// reachable only through its own validating constructor. A caller therefore
// cannot invent a fifth class, cannot build a node whose class disagrees with its
// contents, and cannot set a class at all — Class() is computed from the concrete
// type. GRAPH.md §11.5 requires exactly that ("endpoint class is computed, not
// declared"), and in Go a sealed interface plus unexported fields is how far the
// type system goes: it makes the bad state unconstructible, though it cannot make
// a missing switch case a compile error the way a sum type would.
package graphnode

import (
	"fmt"
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
)

// Class is a node's kind. It is computed from the concrete type and never stored
// on the node, so it cannot drift from what the node actually is.
type Class uint8

// The four node classes (GRAPH.md §3.1).
const (
	ClassObject Class = iota + 1
	ClassIdentity
	ClassExternal
	ClassEphemeral
)

func (c Class) String() string {
	switch c {
	case ClassObject:
		return "object"
	case ClassIdentity:
		return "identity"
	case ClassExternal:
		return "external"
	case ClassEphemeral:
		return "ephemeral"
	}
	return "unknown"
}

// Retention is how an edge touching a node treats that node's lifetime.
type Retention uint8

const (
	// Durable: an edge to this node pins it, exactly as an approval pins the
	// intent revision it approves (tickets D4).
	Durable Retention = iota + 1
	// Collectable: an edge to this node is collected with it. A speculation state
	// produces thousands of these (design §1.5), and an edge that outlived its
	// endpoint would bury the store one attempt at a time (GRAPH.md §11.5).
	Collectable
)

func (r Retention) String() string {
	if r == Collectable {
		return "collectable"
	}
	return "durable"
}

// Retention is derived from the class, which is derived from the concrete type.
// A writer never supplies it, so an ephemeral endpoint cannot be marked durable
// — correctly or otherwise.
func (c Class) Retention() Retention {
	if c == ClassEphemeral {
		return Collectable
	}
	return Durable
}

// Node is a graph endpoint. The interface is sealed: node() is unexported, so
// only this package can implement it.
type Node interface {
	// Class reports the node's kind, computed from its concrete type.
	Class() Class
	// Key is the node's canonical encoding — stable, unambiguous across classes,
	// and parseable back by ParseKey.
	Key() string
	node()
}

// Retention of a node, derived from its class. Provided as a function so callers
// need not reach through Class themselves.
func RetentionOf(n Node) Retention { return n.Class().Retention() }

// --- Object ---

// ObjectNode is a content-addressed object. Its identity is its content, so
// nothing about it can change while an edge points at it.
type ObjectNode struct{ id multihash.Multihash }

// Object builds an object node from a content id.
func Object(id multihash.Multihash) (ObjectNode, error) {
	if len(id) == 0 {
		return ObjectNode{}, fmt.Errorf("graphnode: object node needs a content id")
	}
	return ObjectNode{id: id}, nil
}

func (n ObjectNode) Class() Class            { return ClassObject }
func (n ObjectNode) ID() multihash.Multihash { return n.id }
func (n ObjectNode) Key() string             { return prefixObject + ":" + n.id.Hex() }
func (n ObjectNode) node()                   {}

// --- Identity ---

// IdentityNode is a thing with a life: a ticket, a symbol, a file across time, a
// principal. It carries both halves and needs both: Ref is who it is, Revision is
// what it meant when the edge was written.
//
// Recording only the ref would be the rename bug — the edge would follow the
// identity to a name it never described. Recording only the revision would lose
// the thread that connects a symbol's history together. An edge that stores both
// can answer "what did this mean then" and "what is it now" as the separate
// questions they are.
type IdentityNode struct {
	ref      string
	revision multihash.Multihash
}

// Identity builds an identity node from its ref and the revision it resolved to.
func Identity(ref string, revision multihash.Multihash) (IdentityNode, error) {
	if !strings.HasPrefix(ref, "refs/") {
		return IdentityNode{}, fmt.Errorf("graphnode: identity ref %q must be a ref name", ref)
	}
	if strings.ContainsAny(ref, keySeparator+" \t\n\x00") {
		return IdentityNode{}, fmt.Errorf("graphnode: identity ref %q contains an illegal character", ref)
	}
	if len(revision) == 0 {
		return IdentityNode{}, fmt.Errorf("graphnode: identity node needs the revision it resolved to")
	}
	return IdentityNode{ref: ref, revision: revision}, nil
}

func (n IdentityNode) Class() Class                  { return ClassIdentity }
func (n IdentityNode) Ref() string                   { return n.ref }
func (n IdentityNode) Revision() multihash.Multihash { return n.revision }
func (n IdentityNode) Key() string {
	return prefixIdentity + ":" + n.ref + keySeparator + n.revision.Hex()
}
func (n IdentityNode) node() {}

// --- External ---

// ExternalNode is a row in a foreign system. System is an opaque tag the
// connector chooses; the core neither enumerates nor branches on it, and no
// vendor name belongs in core source (GRAPH.md §3.1).
type ExternalNode struct{ system, foreignID string }

// External builds an external node. The system tag may not contain a colon,
// because the colon is what separates it from a foreign id that often does.
func External(system, foreignID string) (ExternalNode, error) {
	if system == "" || foreignID == "" {
		return ExternalNode{}, fmt.Errorf("graphnode: external node needs a system and a foreign id")
	}
	if strings.Contains(system, ":") {
		return ExternalNode{}, fmt.Errorf("graphnode: system tag %q may not contain a colon", system)
	}
	if strings.ContainsAny(system, " \t\n\x00") || strings.ContainsAny(foreignID, "\n\x00") {
		return ExternalNode{}, fmt.Errorf("graphnode: external identifier contains an illegal character")
	}
	return ExternalNode{system: system, foreignID: foreignID}, nil
}

func (n ExternalNode) Class() Class      { return ClassExternal }
func (n ExternalNode) System() string    { return n.system }
func (n ExternalNode) ForeignID() string { return n.foreignID }
func (n ExternalNode) Key() string {
	return prefixExternal + ":" + n.system + ":" + n.foreignID
}
func (n ExternalNode) node() {}

// --- Ephemeral ---

// EphemeralNode is content-addressed like an object, but its retention is
// governed: an edge to it is collected with it rather than pinning it. The
// distinction is not a property of the bytes, it is a statement about lifetime,
// which is why it is a separate class rather than a flag on ObjectNode — a flag
// is something a writer could set wrongly.
type EphemeralNode struct{ id multihash.Multihash }

// Ephemeral builds an ephemeral node from a content id.
func Ephemeral(id multihash.Multihash) (EphemeralNode, error) {
	if len(id) == 0 {
		return EphemeralNode{}, fmt.Errorf("graphnode: ephemeral node needs a content id")
	}
	return EphemeralNode{id: id}, nil
}

func (n EphemeralNode) Class() Class            { return ClassEphemeral }
func (n EphemeralNode) ID() multihash.Multihash { return n.id }
func (n EphemeralNode) Key() string             { return prefixEphemeral + ":" + n.id.Hex() }
func (n EphemeralNode) node()                   {}
