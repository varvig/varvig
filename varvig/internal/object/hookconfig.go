package object

import (
	"fmt"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
)

// HookEntry binds an event name to a wasm module (a blob object). The module is
// content-addressed, so triggers are versioned alongside the code they guard
// (design §3.2).
type HookEntry struct {
	Event  string
	Module multihash.Multihash
}

// HookConfig is the repository's hook manifest: which wasm module runs on which
// event. It is a content-addressed object referenced by refs/hooks.
type HookConfig struct {
	Entries []HookEntry
}

// The entries are serialized into one field value:
//
//	count    uvarint
//	entries  count records:
//	           eventLen  uvarint
//	           event     eventLen bytes
//	           moduleLen uvarint
//	           module    moduleLen bytes

// NewHookConfig builds a hook manifest object.
func NewHookConfig(c HookConfig) *Object {
	var val []byte
	val = appendUvarint(val, uint64(len(c.Entries)))
	for _, e := range c.Entries {
		val = appendUvarint(val, uint64(len(e.Event)))
		val = append(val, e.Event...)
		val = appendUvarint(val, uint64(len(e.Module)))
		val = append(val, e.Module...)
	}
	return newObject(TypeHookConfig, []field{{tag: tagHookEntries, val: val}})
}

// AsHookConfig decodes the typed view of a hook manifest.
func (o *Object) AsHookConfig() (HookConfig, error) {
	if o.typ != TypeHookConfig {
		return HookConfig{}, fmt.Errorf("object: not a hook config (%s)", o.typ)
	}
	val, ok := o.Field(tagHookEntries)
	if !ok {
		return HookConfig{}, nil
	}
	c := &cursor{b: val}
	n, err := c.uvarint()
	if err != nil {
		return HookConfig{}, err
	}
	var cfg HookConfig
	for i := uint64(0); i < n; i++ {
		el, err := c.uvarint()
		if err != nil {
			return HookConfig{}, err
		}
		event, err := c.take(el)
		if err != nil {
			return HookConfig{}, err
		}
		ml, err := c.uvarint()
		if err != nil {
			return HookConfig{}, err
		}
		mod, err := c.take(ml)
		if err != nil {
			return HookConfig{}, err
		}
		cfg.Entries = append(cfg.Entries, HookEntry{
			Event:  string(event),
			Module: multihash.Multihash(append([]byte(nil), mod...)),
		})
	}
	if !c.empty() {
		return HookConfig{}, fmt.Errorf("%w: trailing bytes in hook config", ErrMalformed)
	}
	return cfg, nil
}
