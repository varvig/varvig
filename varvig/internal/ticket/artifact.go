package ticket

import (
	"fmt"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// Artifacts returns the external artifacts the ticket's head revision names
// (federation §1). A change records the artifact-refs it produced, and a ticket's
// head is a change, so this reads that change's Artifacts and resolves each id to
// its ArtifactRef. The refs are reachability handles — identity, media type, size
// and locators for bytes that live outside the object store (a container image,
// an SBOM, a release archive) — never the bytes themselves. A head that names
// none returns an empty slice.
//
// Like the rest of the ticket read surface it reflects the *head* revision only,
// not the whole intent chain: it is the evidence the current intent points at.
// The order is the change's canonical artifact order (sorted by object id).
func Artifacts(r *repo.Repo, id multihash.Multihash) ([]object.ArtifactRef, error) {
	head, err := Head(r, id)
	if err != nil {
		return nil, err
	}
	obj, err := r.Objects.Get(head)
	if err != nil {
		return nil, err
	}
	c, err := obj.AsChange()
	if err != nil {
		return nil, fmt.Errorf("ticket: head %s is not a change: %w", head.Hex(), err)
	}
	out := make([]object.ArtifactRef, 0, len(c.Artifacts))
	for _, aid := range c.Artifacts {
		ao, err := r.Objects.Get(aid)
		if err != nil {
			return nil, fmt.Errorf("ticket: artifact-ref %s: %w", aid.Hex(), err)
		}
		a, err := ao.AsArtifactRef()
		if err != nil {
			return nil, fmt.Errorf("ticket: %s: %w", aid.Hex(), err)
		}
		out = append(out, a)
	}
	return out, nil
}
