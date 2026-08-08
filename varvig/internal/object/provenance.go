package object

import (
	"fmt"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
)

// Provenance records who or what produced a change (design §1.1 intent, §2.1
// signed provenance). It is a first-class content-addressed object referenced
// by a change, so the audit data dedups across changes and is fetched only
// when auditing rather than weighing down every history walk.
//
// All fields are optional at the codec level; the commit layer populates them
// and the verify layer requires a change to reference a provenance object.
type Provenance struct {
	// Authority is the human, organization, or agent on whose authority the
	// change was made — the identity the signature is expected to bind to.
	Authority string
	// Model, ModelVersion, and Sampling pin the generator so that "intent is
	// primary" has a chance of faithful regeneration (design §1.1, risk §5).
	Model        string
	ModelVersion string
	Sampling     string
	// ToolPermissions are the capabilities in effect when the change was made.
	ToolPermissions []string
	// ToolHash is the multihash of the tool binary itself, so reproducibility
	// covers the tooling and not only the code (design §2.1, §3).
	ToolHash multihash.Multihash
	// TaskSpec, ContextRead, and Reasoning capture the intent: what was asked,
	// what the agent actually read, and the plan it produced (design §1.1).
	TaskSpec    string
	ContextRead string
	Reasoning   string
}

// NewProvenance builds a provenance object, emitting only the fields that are
// set so the encoding stays canonical and compact.
func NewProvenance(p Provenance) *Object {
	var fields []field
	add := func(tag uint64, s string) {
		if s != "" {
			fields = append(fields, field{tag: tag, val: []byte(s)})
		}
	}
	add(tagProvAuthority, p.Authority)
	add(tagProvModel, p.Model)
	add(tagProvModelVersion, p.ModelVersion)
	add(tagProvSampling, p.Sampling)
	if len(p.ToolPermissions) > 0 {
		fields = append(fields, field{tag: tagProvToolPerms, val: encodeStringList(p.ToolPermissions)})
	}
	if p.ToolHash != nil {
		fields = append(fields, field{tag: tagProvToolHash, val: append([]byte(nil), p.ToolHash...)})
	}
	add(tagProvTaskSpec, p.TaskSpec)
	add(tagProvContextRead, p.ContextRead)
	add(tagProvReasoning, p.Reasoning)
	return newObject(TypeProvenance, fields)
}

// AsProvenance decodes the typed view of a provenance object.
func (o *Object) AsProvenance() (Provenance, error) {
	if o.typ != TypeProvenance {
		return Provenance{}, fmt.Errorf("object: not provenance (%s)", o.typ)
	}
	var p Provenance
	str := func(tag uint64) string {
		if v, ok := o.Field(tag); ok {
			return string(v)
		}
		return ""
	}
	p.Authority = str(tagProvAuthority)
	p.Model = str(tagProvModel)
	p.ModelVersion = str(tagProvModelVersion)
	p.Sampling = str(tagProvSampling)
	if v, ok := o.Field(tagProvToolPerms); ok {
		perms, err := decodeStringList(v)
		if err != nil {
			return Provenance{}, err
		}
		p.ToolPermissions = perms
	}
	if v, ok := o.Field(tagProvToolHash); ok {
		p.ToolHash = multihash.Multihash(append([]byte(nil), v...))
	}
	p.TaskSpec = str(tagProvTaskSpec)
	p.ContextRead = str(tagProvContextRead)
	p.Reasoning = str(tagProvReasoning)
	return p, nil
}

// encodeStringList serializes a []string as count + length-prefixed strings.
func encodeStringList(ss []string) []byte {
	var b []byte
	b = appendUvarint(b, uint64(len(ss)))
	for _, s := range ss {
		b = appendUvarint(b, uint64(len(s)))
		b = append(b, s...)
	}
	return b
}

func decodeStringList(b []byte) ([]string, error) {
	c := &cursor{b: b}
	n, err := c.uvarint()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, n)
	for i := uint64(0); i < n; i++ {
		l, err := c.uvarint()
		if err != nil {
			return nil, err
		}
		v, err := c.take(l)
		if err != nil {
			return nil, err
		}
		out = append(out, string(v))
	}
	if !c.empty() {
		return nil, fmt.Errorf("%w: trailing bytes in string list", ErrMalformed)
	}
	return out, nil
}
