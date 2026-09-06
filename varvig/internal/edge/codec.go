package edge

// The note payload, and the rule that a field this binary does not model must
// survive a read-and-rewrite untouched (§4.4, and A3's general form:
// preserve or refuse, never silently drop).
//
// That rule is why decoding keeps every unrecognized key verbatim as raw JSON
// rather than discarding it. A newer producer's edge, rewritten by an older
// binary, comes back byte-identical — including an edge type no binary in the
// network has ever seen, since the core never looks at the value.

import (
	"encoding/json"
	"fmt"

	"github.com/dividebyzero/claude-experiments/varvig/internal/graphnode"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
)

// rawField is an undecoded JSON value held for round-tripping.
type rawField = json.RawMessage

// Wire field names. These are format: changing one orphans every stored edge.
const (
	fSource        = "source"
	fTarget        = "target"
	fType          = "type"
	fObservedUnder = "observed_under"
	fProvenance    = "provenance"
	fValidFrom     = "valid_from"
	fValidTo       = "valid_to"

	fClass        = "class"
	fPrincipal    = "principal"
	fModule       = "module"
	fStrength     = "strength"
	fModel        = "model"
	fModelVersion = "model_version"
	fSampling     = "sampling"
)

// knownTop and knownProv are the keys this binary models. Everything else in a
// payload is unknown and preserved.
var (
	knownTop = map[string]bool{
		fSource: true, fTarget: true, fType: true,
		fObservedUnder: true, fProvenance: true,
		fValidFrom: true, fValidTo: true,
	}
	knownProv = map[string]bool{
		fClass: true, fPrincipal: true, fModule: true, fStrength: true,
		fModel: true, fModelVersion: true, fSampling: true,
	}
)

// Encode renders a stored edge as its note payload. Keys are emitted in sorted
// order (encoding/json sorts map keys), so the same edge always encodes to the
// same bytes and a round trip is comparable byte for byte.
func Encode(e StoredEdge) ([]byte, error) {
	prov := map[string]rawField{}
	for k, v := range e.unknownProv {
		prov[k] = v
	}
	put := func(m map[string]rawField, key string, val any) error {
		b, err := json.Marshal(val)
		if err != nil {
			return err
		}
		m[key] = b
		return nil
	}
	if err := put(prov, fClass, e.provenance.Class.String()); err != nil {
		return nil, err
	}
	if err := put(prov, fPrincipal, e.provenance.Principal); err != nil {
		return nil, err
	}
	if err := put(prov, fStrength, e.provenance.Strength.String()); err != nil {
		return nil, err
	}
	for key, val := range map[string]string{
		fModel: e.provenance.Model, fModelVersion: e.provenance.ModelVersion,
		fSampling: e.provenance.Sampling,
	} {
		if val != "" {
			if err := put(prov, key, val); err != nil {
				return nil, err
			}
		}
	}
	if e.provenance.Module != nil {
		if err := put(prov, fModule, e.provenance.Module.Hex()); err != nil {
			return nil, err
		}
	}

	top := map[string]rawField{}
	for k, v := range e.unknown {
		top[k] = v
	}
	if err := put(top, fSource, e.source.Key()); err != nil {
		return nil, err
	}
	if err := put(top, fTarget, e.target.Key()); err != nil {
		return nil, err
	}
	if err := put(top, fType, e.edgeType); err != nil {
		return nil, err
	}
	if err := put(top, fObservedUnder, e.observedUnder.Hex()); err != nil {
		return nil, err
	}
	provBytes, err := json.Marshal(prov)
	if err != nil {
		return nil, err
	}
	top[fProvenance] = provBytes
	if e.validity.From != nil {
		if err := put(top, fValidFrom, e.validity.From.Hex()); err != nil {
			return nil, err
		}
	}
	if e.validity.To != nil {
		if err := put(top, fValidTo, e.validity.To.Hex()); err != nil {
			return nil, err
		}
	}
	return json.Marshal(top)
}

// Decode parses a note payload back into a stored edge, keeping every field it
// does not model so a rewrite loses nothing.
func Decode(payload []byte) (StoredEdge, error) {
	var top map[string]rawField
	if err := json.Unmarshal(payload, &top); err != nil {
		return StoredEdge{}, fmt.Errorf("edge: payload is not an edge record: %w", err)
	}
	var s Spec
	var err error
	if s.Source, err = decodeNode(top, fSource); err != nil {
		return StoredEdge{}, err
	}
	if s.Target, err = decodeNode(top, fTarget); err != nil {
		return StoredEdge{}, err
	}
	if s.Type, err = decodeString(top, fType); err != nil {
		return StoredEdge{}, err
	}
	if s.ObservedUnder, err = decodeHash(top, fObservedUnder); err != nil {
		return StoredEdge{}, err
	}
	if s.Validity.From, err = decodeOptionalHash(top, fValidFrom); err != nil {
		return StoredEdge{}, err
	}
	if s.Validity.To, err = decodeOptionalHash(top, fValidTo); err != nil {
		return StoredEdge{}, err
	}

	provRaw, ok := top[fProvenance]
	if !ok {
		return StoredEdge{}, fmt.Errorf("edge: record has no provenance")
	}
	var prov map[string]rawField
	if err := json.Unmarshal(provRaw, &prov); err != nil {
		return StoredEdge{}, fmt.Errorf("edge: provenance is not an object: %w", err)
	}
	if s.Provenance, err = decodeProvenance(prov); err != nil {
		return StoredEdge{}, err
	}

	e, err := New(s)
	if err != nil {
		return StoredEdge{}, err
	}
	e.unknown = unknownOf(top, knownTop)
	e.unknownProv = unknownOf(prov, knownProv)
	return e, nil
}

func decodeProvenance(prov map[string]rawField) (Provenance, error) {
	var p Provenance
	class, err := decodeString(prov, fClass)
	if err != nil {
		return p, err
	}
	switch class {
	case Imported.String():
		p.Class = Imported
	case Asserted.String():
		p.Class = Asserted
	default:
		// A class this binary does not know is refused, not guessed. Guessing
		// would let an unreadable record be treated as one of the two kinds whose
		// authority rules differ.
		return p, fmt.Errorf("edge: unknown provenance class %q", class)
	}
	if p.Principal, err = decodeString(prov, fPrincipal); err != nil {
		return p, err
	}
	strength, err := decodeString(prov, fStrength)
	if err != nil {
		return p, err
	}
	switch strength {
	case object.StrengthWeak.String():
		p.Strength = object.StrengthWeak
	case object.StrengthDelegated.String():
		p.Strength = object.StrengthDelegated
	case object.StrengthStrong.String():
		p.Strength = object.StrengthStrong
	default:
		return p, fmt.Errorf("edge: unknown strength %q", strength)
	}
	if p.Module, err = decodeOptionalHash(prov, fModule); err != nil {
		return p, err
	}
	for key, dst := range map[string]*string{
		fModel: &p.Model, fModelVersion: &p.ModelVersion, fSampling: &p.Sampling,
	} {
		if _, ok := prov[key]; ok {
			if *dst, err = decodeString(prov, key); err != nil {
				return p, err
			}
		}
	}
	return p, nil
}

func unknownOf(m map[string]rawField, known map[string]bool) map[string]rawField {
	var out map[string]rawField
	for k, v := range m {
		if known[k] {
			continue
		}
		if out == nil {
			out = map[string]rawField{}
		}
		out[k] = v
	}
	return out
}

func decodeNode(m map[string]rawField, key string) (graphnode.Node, error) {
	s, err := decodeString(m, key)
	if err != nil {
		return nil, err
	}
	n, err := graphnode.ParseKey(s)
	if err != nil {
		return nil, fmt.Errorf("edge: %s: %w", key, err)
	}
	return n, nil
}

func decodeString(m map[string]rawField, key string) (string, error) {
	raw, ok := m[key]
	if !ok {
		return "", fmt.Errorf("edge: record has no %s", key)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("edge: %s is not a string: %w", key, err)
	}
	return s, nil
}

func decodeHash(m map[string]rawField, key string) (multihash.Multihash, error) {
	s, err := decodeString(m, key)
	if err != nil {
		return nil, err
	}
	h, err := multihash.ParseHex(s)
	if err != nil {
		return nil, fmt.Errorf("edge: %s: %w", key, err)
	}
	return h, nil
}

func decodeOptionalHash(m map[string]rawField, key string) (multihash.Multihash, error) {
	if _, ok := m[key]; !ok {
		return nil, nil
	}
	return decodeHash(m, key)
}
