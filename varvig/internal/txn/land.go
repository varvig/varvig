package txn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/varvig/varvig/varvig/internal/multihash"
	"github.com/varvig/varvig/varvig/internal/object"
	"github.com/varvig/varvig/varvig/internal/repo"
)

// The landing surface: the second half of how work reaches a ref.
//
// An executor runs first, outside any transaction, and emits a *proposal* — a
// set of objects and a change that records what it produced against the base it
// saw (design §4.4). Landing is the half that admits that proposal: it takes
// the change's id and drives the scheduler to apply the proposal's delta onto
// the ref's *current* tip, which may have moved since the executor ran.
//
// # Why the input is an object id and not a callback
//
// The unit of work the scheduler admits is a proposal, not a job (§4.4). A job
// would be a callback the transaction runs — and a transaction re-runs its
// Apply up to maxAttempts times when it loses the ref CAS. A callback that
// reached a model inside that loop would be re-sampled on every retry: wrong
// for a pure generator and a money bug for an effectful one. So the model call
// already happened, once, before landing began; what the transaction replays is
// the stored delta, which is a deterministic function of three immutable trees
// and nothing else. Landing never runs an executor, and could not if it tried:
// its only input is an id.
//
// # What a conflict does here, and what it will do later
//
// This surface *refuses* a genuine conflict: a path the proposal changed, that
// the current tip also changed away from the proposal's base to something else.
// Refusal is the conservative floor — it never silently drops the proposal's
// work or the concurrent change's. It is also pure (three immutable trees
// compared byte for byte, no model, no subprocess), which is exactly why it is
// safe inside the re-running Apply. Turning that refusal into a merge — or a
// regeneration from the proposal's provenance — is the next step (§10b item 3),
// and it must happen *outside* this loop, because a regenerator is an executor
// and §4.4 forbids one here. Landing is built refuse-first so that the unsafe
// case is a loud error today, not a wrong answer that a later PR has to notice.

// ErrNotAProposal is returned when the id handed to landing is not a change
// object. Landing operates on a proposal, which is a change; anything else is a
// caller mistake worth naming rather than a tree to guess at.
var ErrNotAProposal = errors.New("land: object is not a change")

// ErrLandConflict is returned when the current tip changed a path the proposal
// also changed, away from the proposal's base and to something other than what
// the proposal wants. It wraps the refusal §10b item 3 will later replace with a
// merge, so a caller distinguishes "the proposal cannot be fast-forwarded" from
// a scheduler CAS loss (refs.ErrConflict) or any other failure.
var ErrLandConflict = errors.New("land: path changed under the proposal's base")

// LandProposal applies the proposal change onto ref, driven by the scheduler so
// that a tip moving underneath the landing re-derives and re-applies rather than
// clobbering (design §1.4). It returns the transaction's Result; a convergent
// proposal — one already present at the tip — lands as a no-op, not an error.
//
// The proposal is produced beforehand and outside any transaction (§4.4); this
// is only the admission half. See the package comment on land for why the input
// is an object id.
func LandProposal(ctx context.Context, r *repo.Repo, ref string, proposal multihash.Multihash) (Result, error) {
	t, err := landTxn(r.Objects, proposal)
	if err != nil {
		return Result{}, err
	}
	res := NewScheduler(r, ref).Run(ctx, []*Txn{t})
	return res[0], res[0].Err
}

// delta is one path the proposal changed, resolved to the concrete content on
// each side so Apply can compare without re-reading blobs on every retry.
type delta struct {
	basePresent   bool
	baseContent   []byte
	targetPresent bool
	targetContent []byte
}

// landTxn builds the transaction that lands a proposal. It reads the proposal's
// tree and the base it was generated against, resolves the delta between them to
// concrete content, and returns a Txn whose declared write set is exactly the
// changed paths and whose Apply replays the delta onto whatever base the
// scheduler hands it.
//
// Scope is the proposal's own delta, not a computed guess: the paths the change
// actually touched are the paths it may touch. A proposal whose declared
// provenance scope is narrower than its delta is a malformed proposal, and the
// workspace's own out-of-scope guard is where that surfaces — landing does not
// re-derive a scope the proposal failed to keep.
func landTxn(objs ObjectStore, proposal multihash.Multihash) (*Txn, error) {
	obj, err := objs.Get(proposal)
	if err != nil {
		return nil, fmt.Errorf("land: reading proposal %s: %w", proposal, err)
	}
	if obj.Type() != object.TypeChange {
		return nil, fmt.Errorf("%w: %s", ErrNotAProposal, proposal)
	}
	ch, err := obj.AsChange()
	if err != nil {
		return nil, fmt.Errorf("land: decoding proposal %s: %w", proposal, err)
	}

	// PT is what the proposal wants; PB is the base it was generated from. A
	// root proposal (no parents) has an empty base, so every path it carries is
	// an addition — the three-way check below degenerates cleanly.
	pt, err := flatten(objs, ch.Tree)
	if err != nil {
		return nil, fmt.Errorf("land: flattening proposal tree: %w", err)
	}
	var baseTree multihash.Multihash
	if len(ch.Parents) > 0 {
		baseTree, err = changeTree(objs, ch.Parents[0])
		if err != nil {
			return nil, fmt.Errorf("land: resolving proposal base: %w", err)
		}
	}
	pb, err := flatten(objs, baseTree)
	if err != nil {
		return nil, fmt.Errorf("land: flattening proposal base tree: %w", err)
	}

	deltas, err := resolveDelta(objs, pb, pt)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(deltas))
	for p := range deltas {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	name := ch.Message
	if name == "" {
		name = "land " + proposal.String()
	}

	apply := func(ws *Workspace) error {
		for _, p := range paths {
			if err := applyDelta(ws, p, deltas[p]); err != nil {
				return err
			}
		}
		return nil
	}
	return &Txn{Name: name, Reads: paths, Writes: paths, Apply: apply}, nil
}

// resolveDelta returns the paths that differ between the proposal's base (pb)
// and its tree (pt), each resolved to the concrete content on both sides. A path
// present on both sides with the same blob id is unchanged and omitted; a
// mode-only change is likewise omitted, because the workspace normalizes mode on
// write and so cannot carry one, and recording it would produce a delta Apply
// could never satisfy.
func resolveDelta(objs ObjectStore, pb, pt map[string]fileEnt) (map[string]delta, error) {
	out := map[string]delta{}
	seen := map[string]bool{}
	for p, tfe := range pt {
		seen[p] = true
		bfe, inBase := pb[p]
		if inBase && bfe.ID.Equal(tfe.ID) {
			continue // unchanged content
		}
		d := delta{targetPresent: true}
		tc, err := blobContent(objs, tfe.ID)
		if err != nil {
			return nil, fmt.Errorf("land: reading proposal content for %q: %w", p, err)
		}
		d.targetContent = tc
		if inBase {
			bc, err := blobContent(objs, bfe.ID)
			if err != nil {
				return nil, fmt.Errorf("land: reading base content for %q: %w", p, err)
			}
			d.basePresent, d.baseContent = true, bc
		}
		out[p] = d
	}
	for p, bfe := range pb {
		if seen[p] {
			continue
		}
		// Present at base, absent in the proposal: the proposal removed it.
		bc, err := blobContent(objs, bfe.ID)
		if err != nil {
			return nil, fmt.Errorf("land: reading base content for %q: %w", p, err)
		}
		out[p] = delta{basePresent: true, baseContent: bc}
	}
	return out, nil
}

// applyDelta replays one path's delta onto the workspace with a three-way
// decision against the current tip:
//
//   - current == base  → the tip has not touched this path since the proposal's
//     base, so the proposal fast-forwards: write (or remove) its target.
//   - current == target → the tip already holds what the proposal wants
//     (someone applied an equal change concurrently); nothing to do.
//   - otherwise         → the tip changed this path to a third thing; refuse,
//     because fast-forwarding would discard that concurrent work. §10b item 3
//     replaces this refusal with a merge performed outside the retry loop.
func applyDelta(ws *Workspace, p string, d delta) error {
	curPresent := ws.Exists(p)
	var curContent []byte
	if curPresent {
		c, err := ws.Read(p)
		if err != nil {
			return err
		}
		curContent = c
	}

	if sameState(curPresent, curContent, d.basePresent, d.baseContent) {
		if d.targetPresent {
			return ws.Write(p, d.targetContent)
		}
		if curPresent {
			return ws.Remove(p)
		}
		return nil
	}
	if sameState(curPresent, curContent, d.targetPresent, d.targetContent) {
		return nil // convergent: already at the target
	}
	return fmt.Errorf("%w: %q", ErrLandConflict, p)
}

func sameState(aPresent bool, aContent []byte, bPresent bool, bContent []byte) bool {
	if aPresent != bPresent {
		return false
	}
	if !aPresent {
		return true
	}
	return bytes.Equal(aContent, bContent)
}

// changeTree resolves a change id to its tree id. A nil id is the empty tree. An
// id that is already a tree (not a change) is returned as-is, matching how the
// scheduler reads a base that may be either.
func changeTree(objs ObjectStore, id multihash.Multihash) (multihash.Multihash, error) {
	if id == nil {
		return nil, nil
	}
	obj, err := objs.Get(id)
	if err != nil {
		return nil, err
	}
	if obj.Type() != object.TypeChange {
		return id, nil
	}
	c, err := obj.AsChange()
	if err != nil {
		return nil, err
	}
	return c.Tree, nil
}

func blobContent(objs ObjectStore, id multihash.Multihash) ([]byte, error) {
	obj, err := objs.Get(id)
	if err != nil {
		return nil, err
	}
	content, ok := obj.BlobContent()
	if !ok {
		return nil, fmt.Errorf("land: object %s is not a blob", id)
	}
	return content, nil
}
