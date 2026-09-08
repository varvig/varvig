package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/p2p"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// syncReservedRefs replicates the reserved ref namespaces after a head sync, in
// the given direction (federation §4, tickets D3).
//
// There is no capability check and no opt-out file, and both omissions are
// deliberate:
//
//   - No capability, because none is needed. LISTREFS already advertises these
//     refs and a push to them is already accepted, so this works against peers
//     built before it existed; a negotiated token would only break that.
//   - No opt-out, because everything replicated here is reserved, and notes
//     already established that a reserved namespace may not be opted out of
//     replication. A knob the code then refuses to honour is worse than no
//     knob. Choosing not to hold a namespace is done by not writing to it.
//
// Divergence prints to stderr and is not an error: two peers holding the same
// claim ref at different values is the claim mechanism working, and the layer
// that knows what the ref means resolves it. A failure to *transfer* a ref
// between two peers is still an error, never a silent partial.
func syncReservedRefs(client *p2p.Client, r *repo.Repo, push bool) error {
	replicate, direction := p2p.ReplicateRefsFetch, "from peer"
	if push {
		replicate, direction = p2p.ReplicateRefsPush, "to peer"
	}
	rep, err := replicate(client, r)
	if err != nil {
		return err
	}
	if len(rep.Updated) > 0 {
		fmt.Printf("refs: replicated %d reserved ref(s) %s\n", len(rep.Updated), direction)
	}
	if len(rep.Diverged) > 0 {
		fmt.Fprintf(os.Stderr, "refs: %d ref(s) diverged and were left untouched on both sides: %s\n",
			len(rep.Diverged), strings.Join(rep.Diverged, ", "))
	}
	if len(rep.Refused) > 0 {
		// A varvig peer cannot advertise one of these — its own ref store
		// validates on write — so this line says something about the peer, and
		// is worth surfacing rather than counting silently.
		fmt.Fprintf(os.Stderr, "refs: the peer advertised %d ref name(s) we will not write: %s\n",
			len(rep.Refused), strings.Join(rep.Refused, ", "))
	}
	return nil
}
