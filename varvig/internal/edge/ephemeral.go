package edge

// Collectable edges — those touching an ephemeral endpoint — are not notes.
//
// This is the one place edge storage forks, and it is worth saying why, because
// the obvious design does not work. A note pins its target (object.Links
// returns a note's Target), and a note ref is a GC root, and every ref move is
// reflogged while every reflog id is *also* a GC root. So an edge note anchored
// on a speculation state would keep that state alive through three separate
// mechanisms. Making it collectable would mean teaching GC to exclude a
// namespace from the ref walk *and* from the reflog walk — weakening universal
// undo — and then reaping refs and logs afterward. That is exactly "a separate
// retention rule with its own sweep", which GRAPH.md §11.5 says not to build.
//
// What §11.5 asks for instead is that these edges "share their endpoint's GC
// root" and are "collected with the speculation state they attach to, by
// construction". So they live beside the speculation pool, keyed by the state
// they describe: discarding the state deletes them, in the same operation, with
// nothing to sweep and nothing to remember. Retention is not enforced here; it
// is unrepresentable to get wrong.
//
// Two consequences worth being explicit about:
//
//   - They do not replicate. A discarded attempt's edges are of no use to a
//     peer, and speculation states are local to begin with (the pool is not
//     synced), so there is nothing to send and no silent loss.
//   - They do not survive promotion. A promoted candidate is an ObjectNode with
//     the same bytes as the EphemeralNode that preceded it — deliberately a
//     different node with the opposite retention (§3.1) — so an edge about the
//     attempt is not an edge about the change. Re-assert it against the object
//     node if it still holds. That is the class distinction doing its job, not a
//     gap in it.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// MaxPerState bounds how many collectable edges one speculation state may carry.
//
// §1.5 expects thousands of speculation states, and §8 names edge retention as
// the way that buries the store. The bound makes excess a loud failure at the
// moment it happens rather than a store that degrades invisibly over weeks —
// A3's preference everywhere else. A var, not a const, so a deployment or a test
// can tune it, exactly as pin.MaxPerPeer is.
var MaxPerState = 256

// ErrStateEdgeBudget reports that a speculation state has as many edges as it
// may carry. It names the count, so the failure says what happened.
var ErrStateEdgeBudget = errors.New("edge: per-state edge budget exceeded")

// ephemeralRoot is where collectable edges live: beside the speculation pool
// (<gitdir>/spec), not inside the object store. Nothing here is
// content-addressed identity — it is local, disposable, per-attempt data.
func ephemeralRoot(r *repo.Repo) string { return filepath.Join(r.GitDir(), "spec-edges") }

// stateDir is one state's edges. The directory *is* the retention unit.
func stateDir(r *repo.Repo, state multihash.Multihash) string {
	return filepath.Join(ephemeralRoot(r), state.Hex())
}

// putEphemeral writes a collectable edge under its anchor's directory, named by
// the content hash of its encoded form. Naming by content makes the write
// idempotent for free: re-asserting the same belief overwrites one file rather
// than accreting.
func putEphemeral(r *repo.Repo, e StoredEdge, anchor multihash.Multihash) (multihash.Multihash, bool, error) {
	payload, err := Encode(e)
	if err != nil {
		return nil, false, err
	}
	id, err := multihash.Sum(multihash.BLAKE3, payload)
	if err != nil {
		return nil, false, err
	}
	dir := stateDir(r, anchor)
	path := filepath.Join(dir, id.Hex())
	if _, err := os.Stat(path); err == nil {
		return id, false, nil // already recorded; not a new edge
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, false, err
	}
	// Check the budget against what is already there, so the bound holds across
	// process runs rather than only within one.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, false, err
	}
	if len(entries) >= MaxPerState {
		return nil, false, fmt.Errorf("%w: state %s already carries %d edges (max %d)",
			ErrStateEdgeBudget, anchor.Hex(), len(entries), MaxPerState)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		return nil, false, err
	}
	return id, true, nil
}

// listEphemeral reads a state's collectable edges. A file it cannot decode is
// reported rather than skipped, for the same reason a note is: silently dropping
// an edge makes a gap indistinguishable from an absence.
func listEphemeral(r *repo.Repo, state multihash.Multihash) ([]Entry, error) {
	dir := stateDir(r, state)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Entry, 0, len(entries))
	for _, f := range entries {
		if f.IsDir() {
			continue
		}
		payload, err := os.ReadFile(filepath.Join(dir, f.Name()))
		if err != nil {
			return nil, err
		}
		e, err := Decode(payload)
		if err != nil {
			return nil, fmt.Errorf("edge: collectable edge %s on state %s: %w",
				f.Name(), state.Hex(), err)
		}
		id, err := multihash.ParseHex(f.Name())
		if err != nil {
			return nil, fmt.Errorf("edge: collectable edge file %q is not a content id", f.Name())
		}
		out = append(out, Entry{Note: id, Edge: e})
	}
	return out, nil
}

// ForgetState deletes every collectable edge attached to a speculation state.
//
// This is what makes retention true by construction: the caller that discards a
// state calls this in the same breath, and there is no window in which the edges
// outlive it and no sweep that has to remember them. It is idempotent, so
// discarding a state twice is not an error.
func ForgetState(r *repo.Repo, state multihash.Multihash) error {
	if state == nil {
		return nil
	}
	return os.RemoveAll(stateDir(r, state))
}

// CountEphemeral is the number of collectable edges a state carries. It exists
// so a test can assert the count returns to baseline exactly after a discard —
// the standing invariant of §11.5.
func CountEphemeral(r *repo.Repo, state multihash.Multihash) (int, error) {
	entries, err := os.ReadDir(stateDir(r, state))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return len(entries), nil
}

// CountAllEphemeral is the total across every state, for the same invariant at
// repository scope.
func CountAllEphemeral(r *repo.Repo) (int, error) {
	states, err := os.ReadDir(ephemeralRoot(r))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	total := 0
	for _, s := range states {
		if !s.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(ephemeralRoot(r), s.Name()))
		if err != nil {
			return 0, err
		}
		total += len(files)
	}
	return total, nil
}
