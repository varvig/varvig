package hook

import (
	"context"
	"errors"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/refs"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// Ref is where the hook manifest lives. It points at a HookConfig object, whose
// entries reference wasm module blobs — so the whole trigger configuration is
// content-addressed and versioned.
const Ref = "refs/hooks"

// Well-known event names fired by built-in trigger points.
const (
	EventPreCommit  = "pre-commit"  // vetoes the commit on nonzero exit
	EventPostCommit = "post-commit" // informational; failures do not block
)

// LoadManifest returns the repository's hook manifest, or an empty one.
func LoadManifest(r *repo.Repo) (object.HookConfig, error) {
	id, err := r.Refs.Resolve(Ref)
	if err != nil {
		if errors.Is(err, refs.ErrNotExist) {
			return object.HookConfig{}, nil
		}
		return object.HookConfig{}, err
	}
	obj, err := r.Objects.Get(id)
	if err != nil {
		return object.HookConfig{}, err
	}
	return obj.AsHookConfig()
}

// SetHook stores a wasm module and binds it to event, replacing any existing
// binding for that event. Returns the module's id.
func SetHook(r *repo.Repo, event string, wasmModule []byte, author string) (multihash.Multihash, error) {
	moduleID, err := r.Objects.Put(object.NewBlob(wasmModule))
	if err != nil {
		return nil, err
	}
	cfg, err := LoadManifest(r)
	if err != nil {
		return nil, err
	}
	next := object.HookConfig{}
	for _, e := range cfg.Entries {
		if e.Event != event {
			next.Entries = append(next.Entries, e)
		}
	}
	next.Entries = append(next.Entries, object.HookEntry{Event: event, Module: moduleID})

	newID, err := r.Objects.Put(object.NewHookConfig(next))
	if err != nil {
		return nil, err
	}
	old, _ := r.Refs.Resolve(Ref)
	if err := r.Refs.CompareAndSwap(Ref, old, newID, author, "hook:set "+event); err != nil {
		return nil, err
	}
	return moduleID, nil
}

// Fire runs every hook bound to event with the given input, in manifest order.
func Fire(ctx context.Context, r *repo.Repo, event string, input []byte) ([]Result, error) {
	cfg, err := LoadManifest(r)
	if err != nil {
		return nil, err
	}
	var results []Result
	for _, e := range cfg.Entries {
		if e.Event != event {
			continue
		}
		obj, err := r.Objects.Get(e.Module)
		if err != nil {
			return results, err
		}
		wasmModule, ok := obj.BlobContent()
		if !ok {
			return results, errors.New("hook: module is not a blob")
		}
		res, err := Run(ctx, wasmModule, input)
		if err != nil {
			return results, err
		}
		results = append(results, res)
	}
	return results, nil
}
