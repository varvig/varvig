package ticket

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/notes"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/reserved"
)

// Artifacts are external build outputs attached to a ticket as *evidence* — a
// container image, an SBOM, a release archive (federation §1). Attaching one is
// deliberately ungoverned: it records a fact about the work, not a change of
// intent, so it goes through notes (like discussion), never a new intent
// revision. This matters — a new revision would move the ticket head, and
// approvals bind to the head hash (§2.2), so a revision-based attach would silently
// un-approve an approved ticket just for recording a build. Notes leave the intent
// chain and its attestations untouched.

// artifactIndexNote is the payload of a per-ticket index note (NoteArtifacts):
// it names the artifact-ref object attached to the ticket.
type artifactIndexNote struct {
	Ref string `json:"ref"` // artifact-ref object id, hex
}

// artifactPinNote is the payload of a reachability pin note (NoteArtifactRef),
// keyed by the artifact-ref id so GC marks the artifact-ref reachable-through.
type artifactPinNote struct {
	Ticket string `json:"ticket"` // ticket id, hex
}

// AttachArtifact records an external artifact against a ticket and returns the
// stored artifact-ref's object id. It writes two ungoverned notes: a per-ticket
// index note (so Artifacts can list a ticket's attachments cheaply) and a pin
// note keyed by the artifact-ref id (so the artifact-ref object — and through it
// the external bytes — stays reachable to GC, exactly as Change.Artifacts would
// make it). The ticket's intent chain, head, and approvals are untouched.
func AttachArtifact(r *repo.Repo, id multihash.Multihash, ref object.ArtifactRef, author string, now int64) (multihash.Multihash, error) {
	if len(ref.ContentHash) == 0 {
		return nil, fmt.Errorf("ticket: artifact needs a content hash")
	}
	if _, err := Head(r, id); err != nil {
		return nil, err // the ticket must exist
	}
	artID, err := r.Objects.Put(object.NewArtifactRef(ref))
	if err != nil {
		return nil, err
	}
	index, err := json.Marshal(artifactIndexNote{Ref: artID.Hex()})
	if err != nil {
		return nil, err
	}
	pin, err := json.Marshal(artifactPinNote{Ticket: id.Hex()})
	if err != nil {
		return nil, err
	}
	if author == "" {
		author = "anon"
	}
	n := notes.New(r)
	if _, err := n.Add(reserved.NoteArtifacts, id, index, author, now); err != nil {
		return nil, err
	}
	if _, err := n.Add(reserved.NoteArtifactRef, artID, pin, author, now); err != nil {
		return nil, err
	}
	return artID, nil
}

// Artifacts returns the external artifacts attached to a ticket (federation §1),
// resolved to their ArtifactRef objects. It unions two sources and dedupes by
// artifact-ref id, ordered by id for determinism:
//
//   - the per-ticket index notes written by AttachArtifact (the usual path), and
//   - the head revision's Change.Artifacts, for the day a materialization producer
//     names on the change the artifacts it built.
//
// The refs are reachability handles — identity, media type, size and locators for
// bytes that live outside the object store — never the bytes themselves. A named
// artifact-ref object that is missing is a loud error, not a silent drop.
func Artifacts(r *repo.Repo, id multihash.Multihash) ([]object.ArtifactRef, error) {
	ids, err := attachedRefIDs(r, id)
	if err != nil {
		return nil, err
	}
	changeArts, err := headChangeArtifacts(r, id)
	if err != nil {
		return nil, err
	}
	ids = append(ids, changeArts...)

	seen := map[string]bool{}
	uniq := make([]multihash.Multihash, 0, len(ids))
	for _, aid := range ids {
		h := aid.Hex()
		if seen[h] {
			continue
		}
		seen[h] = true
		uniq = append(uniq, aid)
	}
	sort.Slice(uniq, func(i, j int) bool { return uniq[i].Hex() < uniq[j].Hex() })

	out := make([]object.ArtifactRef, 0, len(uniq))
	for _, aid := range uniq {
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

// attachedRefIDs reads the per-ticket index notes and returns the artifact-ref
// ids they name.
func attachedRefIDs(r *repo.Repo, id multihash.Multihash) ([]multihash.Multihash, error) {
	chain, err := notes.New(r).List(reserved.NoteArtifacts, id)
	if err != nil {
		return nil, err
	}
	out := make([]multihash.Multihash, 0, len(chain))
	for _, e := range chain {
		var p artifactIndexNote
		if err := json.Unmarshal(e.Note.Payload, &p); err != nil {
			return nil, fmt.Errorf("ticket: bad artifact note %s: %w", e.ID.Hex(), err)
		}
		aid, err := multihash.ParseHex(p.Ref)
		if err != nil {
			return nil, fmt.Errorf("ticket: artifact note %s names a bad ref %q: %w", e.ID.Hex(), p.Ref, err)
		}
		out = append(out, aid)
	}
	return out, nil
}

// headChangeArtifacts reads the artifact-refs the ticket's head change names, if
// any (the materialized-change path).
func headChangeArtifacts(r *repo.Repo, id multihash.Multihash) ([]multihash.Multihash, error) {
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
	return c.Artifacts, nil
}
