package p2p

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/refs"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/reserved"
)

// Reserved ref namespaces replicate by default, exactly as notes do (federation
// §4). The reasoning for which namespaces, and why not the others, lives with
// the catalogue in internal/reserved; this file is only the mechanism.
//
// # This needs no new wire verb, and no new capability
//
// LISTREFS already advertises every ref a peer holds, and servePush already
// accepts any ref name (behind the ref-update hook gate). So the transport could
// always carry these refs; the client simply never asked for them. That makes
// this a client-side omission rather than a protocol gap, and it is why there is
// no capability bit here: gating the fix on a negotiated token would stop it
// working against every peer already deployed, for no gain. An unmodified peer
// advertises its ticket and Factory refs on LISTREFS and accepts a push to them
// today.
//
// # The conflict rule, and why it is this weak
//
// A head is a change with parents and a note is a chain with a Parent, so both
// have an ancestry to fast-forward along. The refs here mostly do not: a lease,
// an envelope and a reservation are canonical blobs with no links at all. There
// is no ordering available to the core, and inventing one would mean the core
// learning what a lease means — the thing reserving a namespace is supposed to
// avoid.
//
// So the rule is structural only:
//
//   - the peer has it and we do not          -> take it
//   - both hold the same id                  -> nothing to do
//   - one id reaches the other in the object
//     graph                                 -> the reaching side supersedes
//   - anything else                          -> diverged: touch neither, report
//
// Reachability generalises the notes Parent walk to the links every object
// already exposes, so a ticket's intent chain still fast-forwards while a
// Factory blob, having no links, falls through to the last case. Where the core
// cannot tell, it refuses and says so.
//
// # Divergence is a report, not a failure
//
// Notes abort the whole sync on a divergent chain, because two peers writing the
// same note chain differently is a fault. Here it is ordinary: two cells racing
// the same claim ref is the mechanism working. Aborting would mean one contested
// claim stopping every lease and ticket from replicating. So divergence is
// collected and returned — visible, per-ref, and resolved by the layer that
// knows what the ref means — while a *transfer* failure between two peers stays
// a loud error, never a silent partial.
//
// # A ref name from the network is a path
//
// These names arrive over the wire and are written into the ref store, so they
// are validated at this boundary rather than left to fail deeper down. A
// well-behaved peer cannot advertise an invalid one — its own store validates on
// write — so a refusal is a report about the peer.
//
// # Deletion never propagates
//
// A ref we hold and the peer does not is left alone in both directions. The
// protocol cannot distinguish "deleted there" from "never existed there", and
// guessing deletion would drop a lease or a reservation record — the two things
// whose absence reads as "nothing was ordered".

// RefReport records what a reserved-ref replication pass did. Both slices are
// sorted, so a caller printing them gets stable output.
type RefReport struct {
	// Updated names the refs this pass moved: created or advanced locally on a
	// fetch, pushed to the peer on a push.
	Updated []string
	// Diverged names the refs both sides hold at values with no ancestry
	// between them. Neither side was touched.
	Diverged []string
	// Refused names refs the peer advertised under a replicating namespace
	// whose names we will not write. A well-behaved peer cannot produce one —
	// its own ref store validates on write — so this is a report about the
	// peer, not about our repository.
	Refused []string
}

func (rep *RefReport) sort() {
	sort.Strings(rep.Updated)
	sort.Strings(rep.Diverged)
	sort.Strings(rep.Refused)
}

// ReplicateRefsFetch pulls every reserved replicating ref the peer advertises,
// creating refs we lack and advancing refs the peer's value supersedes.
//
// Objects travel in one batched transfer: the protocol takes a want set pruned
// by a have set, so N tickets cost one round trip rather than N.
func ReplicateRefsFetch(c *Client, r *repo.Repo) (RefReport, error) {
	var rep RefReport
	remote, err := c.ListRefs()
	if err != nil {
		return rep, err
	}

	type candidate struct {
		name         string
		local, their multihash.Multihash
	}
	var cands []candidate
	var names []string
	var want, have []multihash.Multihash
	for _, ref := range remote {
		if !reserved.IsReplicatedRef(ref.Name) {
			continue
		}
		// The name arrives over the network and becomes a path, so it is
		// checked here, at the boundary, rather than left to fail three layers
		// down inside the ref store: one malformed advertisement must not be
		// able to abort replication of everything else.
		if err := refs.ValidName(ref.Name); err != nil {
			rep.Refused = append(rep.Refused, ref.Name)
			continue
		}
		their := multihash.Multihash(ref.ID)
		// Same reasoning for the id: an undecodable multihash is a malformed
		// advertisement, and asking for it would abort a pass that should
		// instead deliver every well-formed ref beside it. A bad-but-decodable
		// id still fails loudly later, at PutVerified.
		if _, _, err := multihash.Decode(their); err != nil {
			rep.Refused = append(rep.Refused, ref.Name)
			continue
		}
		local := resolveOrNil(r, ref.Name)
		if local != nil && local.Equal(their) {
			continue
		}
		cands = append(cands, candidate{ref.Name, local, their})
		names = append(names, ref.Name)
		want = append(want, their)
		if local != nil {
			have = append(have, local)
		}
	}
	if len(cands) == 0 {
		rep.sort()
		return rep, nil
	}

	if err := c.Fetch(r.Objects, want, have); err != nil {
		return rep, fmt.Errorf("refs: transferring %s: %w", describe(names), err)
	}

	for _, cd := range cands {
		if cd.local == nil {
			if err := r.Refs.CompareAndSwap(cd.name, nil, cd.their, "ref-sync", "fetch "+cd.name); err != nil {
				return rep, fmt.Errorf("refs: creating %s: %w", cd.name, err)
			}
			rep.Updated = append(rep.Updated, cd.name)
			continue
		}
		if !reaches(r.Objects, cd.their, cd.local) {
			rep.Diverged = append(rep.Diverged, cd.name)
			continue
		}
		if err := r.Refs.CompareAndSwap(cd.name, cd.local, cd.their, "ref-sync", "fetch "+cd.name); err != nil {
			return rep, fmt.Errorf("refs: advancing %s: %w", cd.name, err)
		}
		rep.Updated = append(rep.Updated, cd.name)
	}
	rep.sort()
	return rep, nil
}

// ReplicateRefsPush pushes every local reserved replicating ref the peer lacks
// or whose local value supersedes the peer's.
//
// The compare-and-swap value is what the peer advertised on this connection, so
// a change between our LISTREFS and our PUSH is refused server-side rather than
// overwritten. That is a stronger guard than the head path's tracking-ref lease,
// not a weaker one: the check happens at the peer, on the value it holds now.
func ReplicateRefsPush(c *Client, r *repo.Repo) (RefReport, error) {
	var rep RefReport
	remote, err := c.ListRefs()
	if err != nil {
		return rep, err
	}
	theirs := map[string]multihash.Multihash{}
	for _, ref := range remote {
		theirs[ref.Name] = multihash.Multihash(ref.ID)
	}
	local, err := r.Refs.List()
	if err != nil {
		return rep, err
	}
	sort.Strings(local)
	for _, name := range local {
		if !reserved.IsReplicatedRef(name) {
			continue
		}
		// Refs.List walks a directory, so a file planted there yields a name
		// that is not a ref. That is a local artifact to report, not a fault to
		// abort a push over; a name that is valid and still will not resolve
		// is a real fault and stays loud.
		if err := refs.ValidName(name); err != nil {
			rep.Refused = append(rep.Refused, name)
			continue
		}
		tip, err := r.Refs.Resolve(name)
		if err != nil {
			return rep, fmt.Errorf("refs: resolving %s: %w", name, err)
		}
		old := theirs[name]
		if old != nil && old.Equal(tip) {
			continue
		}
		if old != nil && !reaches(r.Objects, tip, old) {
			rep.Diverged = append(rep.Diverged, name)
			continue
		}
		if err := c.Push(r.Objects, name, old, tip); err != nil {
			return rep, fmt.Errorf("refs: pushing %s: %w", name, err)
		}
		rep.Updated = append(rep.Updated, name)
	}
	rep.sort()
	return rep, nil
}

// reaches reports whether target is in from's reachable closure, so that from
// strictly extends what target names.
//
// It walks the generic Links graph and stops at the first hit, which keeps a
// long intent or change chain from being fully traversed to answer "does this
// supersede the one ref we hold". An object we do not have is not walked
// through: absence answers "cannot show it reaches", which is the conservative
// direction — the caller then reports divergence instead of moving a ref.
func reaches(objs ObjectStore, from, target multihash.Multihash) bool {
	seen := map[string]bool{}
	queue := []multihash.Multihash{from}
	for len(queue) > 0 {
		id := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if id.Equal(target) {
			return true
		}
		key := id.Hex()
		if seen[key] {
			continue
		}
		seen[key] = true
		obj, err := objs.Get(id)
		if err != nil {
			continue
		}
		links, err := obj.Links()
		if err != nil {
			continue
		}
		queue = append(queue, links...)
	}
	return false
}

// describe names a bounded sample of a batch, so a transfer failure over two
// hundred tickets reports which refs were in flight without printing all of
// them.
func describe(names []string) string {
	const sample = 3
	shown := names
	if len(shown) > sample {
		shown = shown[:sample]
	}
	out := fmt.Sprintf("%d ref(s)", len(names))
	if len(shown) == 0 {
		return out
	}
	out += " (" + strings.Join(shown, ", ")
	if len(names) > len(shown) {
		out += fmt.Sprintf(", and %d more", len(names)-len(shown))
	}
	return out + ")"
}
