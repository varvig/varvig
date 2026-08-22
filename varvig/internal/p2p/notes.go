package p2p

import (
	"errors"
	"fmt"
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/refs"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/reserved"
	"github.com/dividebyzero/claude-experiments/varvig/internal/wire"
)

// Notes replicate by default (federation §4): cross-peer evidence sharing
// depends on it, and the worst failure shape is a federation silently losing the
// data selection exists to compare. So sync drives these after the head, and any
// failure to move a note between two notes-sync peers is an error, never a silent
// omission.

const notesPrefix = "refs/notes/"

// noteNamespaceOf recovers the namespace from a refs/notes/<ns>/<target> name.
// The namespace may itself contain slashes (e.g. varvig/attest); the target hex
// is the final segment.
func noteNamespaceOf(ref string) (string, bool) {
	rest, ok := strings.CutPrefix(ref, notesPrefix)
	if !ok {
		return "", false
	}
	i := strings.LastIndexByte(rest, '/')
	if i <= 0 {
		return "", false
	}
	return rest[:i], true
}

// skipNamespace applies a per-namespace opt-out (federation §4). Reserved
// governance namespaces (evidence, attestation, scope, score) are never opted
// out — they must always sync; only a peer's own non-reserved namespaces may.
func skipNamespace(ns string, optOut func(string) bool) bool {
	if optOut == nil || reserved.IsReservedNoteNamespace(ns) {
		return false
	}
	return optOut(ns)
}

// ReplicateNotesFetch pulls every notes ref the peer advertises into r,
// fast-forwarding local notes refs. It requires the notes-sync capability and
// fails loudly rather than silently skipping a note it cannot transfer.
func ReplicateNotesFetch(c *Client, r *repo.Repo, optOut func(ns string) bool) error {
	if !c.caps[wire.CapNotesSync] {
		return fmt.Errorf("p2p: peer does not advertise %q; refusing to pretend notes replicated", wire.CapNotesSync)
	}
	remote, err := c.ListRefs()
	if err != nil {
		return err
	}
	for _, ref := range remote {
		ns, ok := noteNamespaceOf(ref.Name)
		if !ok || skipNamespace(ns, optOut) {
			continue
		}
		tip := multihash.Multihash(ref.ID)
		localOld := resolveOrNil(r, ref.Name)
		var have []multihash.Multihash
		if localOld != nil {
			have = append(have, localOld)
		}
		if err := c.Fetch(r.Objects, []multihash.Multihash{tip}, have); err != nil {
			return fmt.Errorf("notes: fetching %s: %w", ref.Name, err)
		}
		if err := fastForwardNoteRef(r, ref.Name, localOld, tip); err != nil {
			return err
		}
	}
	return nil
}

// ReplicateNotesPush pushes every local notes ref to the peer, fast-forward
// only. Same capability requirement and loud-failure discipline as the fetch
// direction.
func ReplicateNotesPush(c *Client, r *repo.Repo, optOut func(ns string) bool) error {
	if !c.caps[wire.CapNotesSync] {
		return fmt.Errorf("p2p: peer does not advertise %q; refusing to pretend notes replicated", wire.CapNotesSync)
	}
	remote, err := c.ListRefs()
	if err != nil {
		return err
	}
	remoteTip := map[string]multihash.Multihash{}
	for _, ref := range remote {
		remoteTip[ref.Name] = multihash.Multihash(ref.ID)
	}
	local, err := r.Refs.List()
	if err != nil {
		return err
	}
	for _, name := range local {
		ns, ok := noteNamespaceOf(name)
		if !ok || skipNamespace(ns, optOut) {
			continue
		}
		localTip, err := r.Refs.Resolve(name)
		if err != nil {
			return fmt.Errorf("notes: resolving %s: %w", name, err)
		}
		old := remoteTip[name]
		if old != nil && old.Equal(localTip) {
			continue // remote already has this tip
		}
		// Fast-forward only: never clobber notes the remote has that we lack.
		if old != nil && !noteChainContains(r, localTip, old) {
			return fmt.Errorf("notes: %s diverged (remote %s not an ancestor of local %s); refusing to clobber", name, shortHash(old), shortHash(localTip))
		}
		if err := c.Push(r.Objects, name, old, localTip); err != nil {
			return fmt.Errorf("notes: pushing %s: %w", name, err)
		}
	}
	return nil
}

// fastForwardNoteRef advances a local notes ref to remoteTip, refusing to
// overwrite a local chain that the remote tip does not descend from.
func fastForwardNoteRef(r *repo.Repo, name string, localOld, remoteTip multihash.Multihash) error {
	if localOld != nil && localOld.Equal(remoteTip) {
		return nil
	}
	if localOld == nil {
		return r.Refs.CompareAndSwap(name, nil, remoteTip, "notes-sync", "fetch "+name)
	}
	if !noteChainContains(r, remoteTip, localOld) {
		return fmt.Errorf("notes: %s diverged (local %s not an ancestor of remote %s); refusing to clobber", name, shortHash(localOld), shortHash(remoteTip))
	}
	return r.Refs.CompareAndSwap(name, localOld, remoteTip, "notes-sync", "fetch "+name)
}

// noteChainContains reports whether target appears in the note Parent chain
// rooted at tip. A note namespace/target is an append-only chain linked by
// Parent (notes design), so ancestry is a linear walk.
func noteChainContains(r *repo.Repo, tip, target multihash.Multihash) bool {
	cur := tip
	seen := map[string]bool{}
	for cur != nil {
		if cur.Equal(target) {
			return true
		}
		key := cur.Hex()
		if seen[key] {
			return false
		}
		seen[key] = true
		obj, err := r.Objects.Get(cur)
		if err != nil {
			return false
		}
		n, err := obj.AsNote()
		if err != nil {
			return false
		}
		cur = n.Parent
	}
	return false
}

func resolveOrNil(r *repo.Repo, name string) multihash.Multihash {
	id, err := r.Refs.Resolve(name)
	if err != nil {
		if errors.Is(err, refs.ErrNotExist) {
			return nil
		}
		return nil
	}
	return id
}

func shortHash(m multihash.Multihash) string {
	h := m.Hex()
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
