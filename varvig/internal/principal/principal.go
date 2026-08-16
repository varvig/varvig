// Package principal is the repository's org chart (tickets §1.4): the set of
// keyholders and, for each, the kind (human / agent / bridge) that governs what
// its signatures are worth. The chart is a tree pointed at by
// refs/varvig/principals and moved by compare-and-swap, so it is versioned,
// hash-pinned, diffable, and auditable through the reflog — "who was allowed to
// approve billing changes in March" is answered from ref history, not an
// interview.
//
// Each principal is a content-addressed TypePrincipal object stored as a blob
// (its encoded bytes), and the tree maps fingerprint → that blob. Reusing tree
// and blob means the object store's existing garbage collection pins every
// principal through the tree's links, with no new object type and no change to
// the frozen format.
package principal

import (
	"fmt"
	"sort"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/reserved"
	"github.com/dividebyzero/claude-experiments/varvig/internal/sshkey"
)

// Registry is the org chart backed by a repository ref. It implements
// attest.KindResolver, so it can be handed straight to
// attest.VerifyWithPrincipal.
type Registry struct{ r *repo.Repo }

// Open returns the registry for a repository.
func Open(r *repo.Repo) *Registry { return &Registry{r: r} }

// Fingerprint returns the SSH SHA256 fingerprint of a principal's key — the
// name it is filed under in the chart.
func Fingerprint(p object.Principal) string {
	return sshkey.PublicKey{Key: p.Key}.Fingerprint()
}

// Add records a principal, replacing any existing entry for the same
// fingerprint. The chart advances by compare-and-swap, so a concurrent writer
// retries against the new head and the move is recorded in the reflog.
func (reg *Registry) Add(p object.Principal, author string, now int64) error {
	if len(p.Key) == 0 {
		return fmt.Errorf("principal: a public key is required")
	}
	fp := Fingerprint(p)
	blobID, err := reg.r.Objects.Put(object.NewBlob(object.NewPrincipal(p).Encode()))
	if err != nil {
		return err
	}
	return reg.update(author, "principal add "+fp, func(m map[string]object.Entry) {
		m[fp] = object.Entry{Name: fp, Mode: 0o100644, Kind: object.TypeBlob, ID: blobID}
	})
}

// Remove drops a principal by fingerprint. Removing an unknown fingerprint is a
// no-op that still records a chart version, so the audit trail shows the intent.
func (reg *Registry) Remove(fingerprint, author string) error {
	return reg.update(author, "principal remove "+fingerprint, func(m map[string]object.Entry) {
		delete(m, fingerprint)
	})
}

// Get returns the principal filed under fingerprint, if present.
func (reg *Registry) Get(fingerprint string) (object.Principal, bool, error) {
	entries, err := reg.entries()
	if err != nil {
		return object.Principal{}, false, err
	}
	e, ok := entries[fingerprint]
	if !ok {
		return object.Principal{}, false, nil
	}
	p, err := reg.decode(e.ID)
	if err != nil {
		return object.Principal{}, false, err
	}
	return p, true, nil
}

// KindOf implements attest.KindResolver: the kind of the principal filed under
// fingerprint, or false if unknown.
func (reg *Registry) KindOf(fingerprint string) (object.Kind, bool) {
	p, ok, err := reg.Get(fingerprint)
	if err != nil || !ok {
		return object.KindUnknown, false
	}
	return p.Kind, true
}

// List returns every principal, ordered by fingerprint for determinism.
func (reg *Registry) List() ([]object.Principal, error) {
	entries, err := reg.entries()
	if err != nil {
		return nil, err
	}
	fps := make([]string, 0, len(entries))
	for fp := range entries {
		fps = append(fps, fp)
	}
	sort.Strings(fps)
	out := make([]object.Principal, 0, len(fps))
	for _, fp := range fps {
		p, err := reg.decode(entries[fp].ID)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// entries reads the current chart tree into a fingerprint→entry map.
func (reg *Registry) entries() (map[string]object.Entry, error) {
	out := map[string]object.Entry{}
	treeID, err := reg.r.Refs.Resolve(reserved.PrincipalsRef)
	if err != nil {
		return out, nil // no chart yet
	}
	obj, err := reg.r.Objects.Get(treeID)
	if err != nil {
		return nil, err
	}
	list, err := obj.TreeEntries()
	if err != nil {
		return nil, err
	}
	for _, e := range list {
		out[e.Name] = e
	}
	return out, nil
}

func (reg *Registry) decode(blobID multihash.Multihash) (object.Principal, error) {
	obj, err := reg.r.Objects.Get(blobID)
	if err != nil {
		return object.Principal{}, err
	}
	content, ok := obj.BlobContent()
	if !ok {
		return object.Principal{}, fmt.Errorf("principal: entry %s is not a blob", blobID.Hex())
	}
	pObj, err := object.Decode(content)
	if err != nil {
		return object.Principal{}, err
	}
	return pObj.AsPrincipal()
}

// update rebuilds the chart tree by applying mutate to the current entry map
// and moves the ref by compare-and-swap.
func (reg *Registry) update(author, msg string, mutate func(map[string]object.Entry)) error {
	old, err := reg.r.Refs.Resolve(reserved.PrincipalsRef)
	if err != nil {
		old = nil
	}
	entries, err := reg.entries()
	if err != nil {
		return err
	}
	mutate(entries)

	list := make([]object.Entry, 0, len(entries))
	for _, e := range entries {
		list = append(list, e)
	}
	treeID, err := reg.r.Objects.Put(object.NewTree(list))
	if err != nil {
		return err
	}
	return reg.r.Refs.CompareAndSwap(reserved.PrincipalsRef, old, treeID, author, msg)
}
