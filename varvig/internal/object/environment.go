package object

import (
	"fmt"
	"sort"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
)

// Environment describes the environment a piece of evidence was produced in
// (federation §2): the platform, the toolchain versions, the flags that affect
// outcome, an optional container image, and — only for evidence produced by
// inference — the model. Evidence records what was checked and with what
// result; the environment records against what, so cross-peer selection can
// compare like with like instead of comparing "tests passed" on one toolchain
// against "tests passed" on another while appearing rigorous.
//
// It is a first-class object, referenced by a Provenance (the evidence object),
// so thousands of evidence records that share an environment deduplicate onto
// one descriptor. Its encoding is canonical and deterministic — maps are
// emitted in sorted key order — so identical environments hash identically
// across peers; that identity is what makes both deduplication and comparison
// work.
type Environment struct {
	Platform   string            // os/arch
	Toolchains map[string]string // compiler, runtime, test runner, linker…
	Flags      map[string]string // build/test flags that affect the outcome
	Container  multihash.Multihash
	Model      *EnvModel // set only for evidence produced by inference
}

// EnvModel identifies the inference model behind a piece of evidence, so a
// Factory can decide which peer can faithfully *regenerate* an attempt versus
// merely produce a new one (federation §2.3, design §4b.3).
type EnvModel struct {
	ID      string
	Version string
	Params  string
}

// NewEnvironment builds an environment object. Maps are emitted in sorted key
// order and empty components are omitted, so equal environments always encode
// to identical bytes regardless of map iteration order.
func NewEnvironment(e Environment) *Object {
	var fields []field
	if e.Platform != "" {
		fields = append(fields, field{tag: tagEnvPlatform, val: []byte(e.Platform)})
	}
	if len(e.Toolchains) > 0 {
		fields = append(fields, field{tag: tagEnvToolchains, val: encodeStringMap(e.Toolchains)})
	}
	if len(e.Flags) > 0 {
		fields = append(fields, field{tag: tagEnvFlags, val: encodeStringMap(e.Flags)})
	}
	if e.Container != nil {
		fields = append(fields, field{tag: tagEnvContainer, val: append([]byte(nil), e.Container...)})
	}
	if e.Model != nil {
		if e.Model.ID != "" {
			fields = append(fields, field{tag: tagEnvModelID, val: []byte(e.Model.ID)})
		}
		if e.Model.Version != "" {
			fields = append(fields, field{tag: tagEnvModelVersion, val: []byte(e.Model.Version)})
		}
		if e.Model.Params != "" {
			fields = append(fields, field{tag: tagEnvModelParams, val: []byte(e.Model.Params)})
		}
	}
	return newObject(TypeEnvironment, fields)
}

// AsEnvironment decodes the typed view of an environment object.
func (o *Object) AsEnvironment() (Environment, error) {
	if o.typ != TypeEnvironment {
		return Environment{}, fmt.Errorf("object: not an environment (%s)", o.typ)
	}
	var e Environment
	if v, ok := o.Field(tagEnvPlatform); ok {
		e.Platform = string(v)
	}
	if v, ok := o.Field(tagEnvToolchains); ok {
		m, err := decodeStringMap(v)
		if err != nil {
			return Environment{}, err
		}
		e.Toolchains = m
	}
	if v, ok := o.Field(tagEnvFlags); ok {
		m, err := decodeStringMap(v)
		if err != nil {
			return Environment{}, err
		}
		e.Flags = m
	}
	if v, ok := o.Field(tagEnvContainer); ok {
		e.Container = multihash.Multihash(append([]byte(nil), v...))
	}
	var m EnvModel
	if v, ok := o.Field(tagEnvModelID); ok {
		m.ID = string(v)
	}
	if v, ok := o.Field(tagEnvModelVersion); ok {
		m.Version = string(v)
	}
	if v, ok := o.Field(tagEnvModelParams); ok {
		m.Params = string(v)
	}
	if m != (EnvModel{}) {
		e.Model = &m
	}
	return e, nil
}

// encodeStringMap serializes a map as count + (keyLen,key,valLen,val)*, with
// keys in ascending order — a canonical, deterministic encoding independent of
// Go's map iteration order.
func encodeStringMap(m map[string]string) []byte {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b []byte
	b = appendUvarint(b, uint64(len(keys)))
	for _, k := range keys {
		b = appendUvarint(b, uint64(len(k)))
		b = append(b, k...)
		v := m[k]
		b = appendUvarint(b, uint64(len(v)))
		b = append(b, v...)
	}
	return b
}

func decodeStringMap(b []byte) (map[string]string, error) {
	c := &cursor{b: b}
	n, err := c.uvarint()
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, n)
	var prev string
	for i := uint64(0); i < n; i++ {
		kl, err := c.uvarint()
		if err != nil {
			return nil, err
		}
		kb, err := c.take(kl)
		if err != nil {
			return nil, err
		}
		vl, err := c.uvarint()
		if err != nil {
			return nil, err
		}
		vb, err := c.take(vl)
		if err != nil {
			return nil, err
		}
		k := string(kb)
		if i > 0 && k <= prev {
			return nil, fmt.Errorf("%w: environment map keys not sorted/unique", ErrMalformed)
		}
		out[k] = string(vb)
		prev = k
	}
	if !c.empty() {
		return nil, fmt.Errorf("%w: trailing bytes in environment map", ErrMalformed)
	}
	return out, nil
}
