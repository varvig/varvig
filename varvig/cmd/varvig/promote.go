package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/attest"
	"github.com/dividebyzero/claude-experiments/varvig/internal/check"
	"github.com/dividebyzero/claude-experiments/varvig/internal/identity"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/refs"
	"github.com/dividebyzero/claude-experiments/varvig/internal/refupdate"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// checkPromotionNotStale refuses to promote a commit that implements an intent
// revision which is not approved — the staleness guard of the ticket→commit link
// (tickets, "The Ticket → Commit Link", §2). Because Change.Fulfills names the
// exact revision the work was written against, this catches the common race where
// a spec is approved, then edited, and the work is implemented against the
// edited-but-unapproved revision: the fulfilled revision has no approval, so the
// promotion is refused. A commit that fulfills nothing, or a non-change ref, is
// unaffected (fulfilling nothing is legal; policy decides where it is required).
func checkPromotionNotStale(r *repo.Repo, newID multihash.Multihash) error {
	obj, err := r.Objects.Get(newID)
	if err != nil {
		return nil // not a readable object here; presence was already checked
	}
	c, err := obj.AsChange()
	if err != nil || c.Fulfills == nil {
		return nil // not a change, or fulfills nothing — nothing to guard
	}
	atts, err := attest.Attestations(r, c.Fulfills)
	if err != nil {
		return err
	}
	if status := attest.Derive(atts, object.StrengthStrong); status != attest.StatusApproved {
		return fmt.Errorf("promote: %s implements intent revision %s, which is not approved (%s); "+
			"the approved spec has moved on — re-approve the current revision, or pass --allow-stale to override",
			newID.Hex(), c.Fulfills.Hex(), status)
	}
	return nil
}

// checkPromotionEvidenceNotStale enforces §1.3's evidence invariant at promotion
// (build spec P1.3): verification evidence binds to the tree it was produced
// against, so an edit after checking is detectable. A proposal that has been
// checked must promote with *fresh, passing* evidence — evidence produced against
// a different tree is stale and is treated as absent, never as a pass. A proposal
// that was never checked is unaffected (evidence is opt-in per proposal); the
// guard exists so a passing check cannot be silently invalidated by a later edit
// and still wave the change through. --allow-stale overrides it deliberately.
func checkPromotionEvidenceNotStale(r *repo.Repo, newID multihash.Multihash) error {
	obj, err := r.Objects.Get(newID)
	if err != nil {
		return nil
	}
	c, err := obj.AsChange()
	if err != nil {
		return nil // not a change — no tree, no evidence to judge
	}
	state, staleTree, err := check.Promotion(r, newID, c.Tree)
	if err != nil {
		return err
	}
	switch state {
	case check.FreshFail:
		return fmt.Errorf("promote: %s failed its checks on the current tree %s; "+
			"fix the cause and re-run `varvig check`, or pass --allow-stale to override",
			newID.Hex(), c.Tree.Hex())
	case check.Stale:
		return fmt.Errorf("promote: %s has only stale check evidence — it was checked against tree %s "+
			"but the current tree is %s; the proposal was edited after checking, so the evidence does not count. "+
			"Re-run `varvig check`, or pass --allow-stale to override",
			newID.Hex(), staleTree, c.Tree.Hex())
	default: // NoEvidence (opt-in) or FreshPass
		return nil
	}
}

// cmdPromote moves a ref by way of a signed ref update (auth design §5, §10.7).
// The signature travels with the change: the same bytes are signed here and
// verified by the pipeline, so a promotion is authorized by *who signed it*,
// not by the channel it arrived over.
//
//	varvig promote <ref> <new> [--scope S] [--ttl SECONDS] [--allow-stale]
//
// If <new> is a commit that fulfills a ticket intent revision (Change.Fulfills),
// the promotion is refused unless that revision is approved — the staleness guard
// of the ticket→commit link. --allow-stale overrides it deliberately.
//
// The lease (expected_old) is the ref's current value, so a concurrent move is
// rejected as a conflict and the caller is told the new head to rebase onto —
// the embryonic regeneration-merge retry of §10.6.
func cmdPromote(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: varvig promote <ref> <new> [--scope S] [--ttl SECONDS] [--allow-stale]")
	}
	refName, newArg := args[0], args[1]
	scope := "/"
	ttl := int64(3600)
	allowStale := false
	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "--scope":
			if i+1 >= len(args) {
				return errors.New("promote: --scope requires a value")
			}
			scope = args[i+1]
			i++
		case "--ttl":
			if i+1 >= len(args) {
				return errors.New("promote: --ttl requires a value")
			}
			n, err := strconv.ParseInt(args[i+1], 10, 64)
			if err != nil {
				return fmt.Errorf("promote: bad --ttl: %w", err)
			}
			ttl = n
			i++
		case "--allow-stale":
			allowStale = true
		default:
			return fmt.Errorf("promote: unknown argument %q", args[i])
		}
	}

	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	newID, err := resolve(r, newArg)
	if err != nil {
		return fmt.Errorf("promote: cannot resolve %q: %w", newArg, err)
	}
	if !r.Objects.Has(newID) {
		return fmt.Errorf("promote: object %s is not present", newID.Hex())
	}
	if !allowStale {
		if err := checkPromotionNotStale(r, newID); err != nil {
			return err
		}
		if err := checkPromotionEvidenceNotStale(r, newID); err != nil {
			return err
		}
	}

	id, err := identity.Resolve("", os.Getenv)
	if err != nil {
		return err
	}
	defer id.Close()
	if !id.CanSign() {
		return errors.New("promote: active identity cannot sign (encrypted key and no ssh-agent); " +
			"start ssh-agent and `ssh-add` your key")
	}

	// The lease is the ref's current value (nil if it does not yet exist).
	expectedOld, err := r.Refs.Resolve(refName)
	if err != nil {
		if !errors.Is(err, refs.ErrNotExist) {
			return err
		}
		expectedOld = nil
	}

	nonce, err := refupdate.NewNonce()
	if err != nil {
		return err
	}
	su, err := refupdate.Sign(id.Signer, refupdate.Params{
		Ref:         refName,
		ExpectedOld: expectedOld,
		New:         newID,
		Scope:       scope,
		Nonce:       nonce,
		NotAfter:    time.Now().Unix() + ttl,
	})
	if err != nil {
		return err
	}

	tf, err := loadTrustFile(r)
	if err != nil {
		return err
	}
	guard, err := refupdate.NewFileGuard(filepath.Join(r.GitDir(), "refupdate"))
	if err != nil {
		return err
	}
	v := &refupdate.Verifier{
		Trust:   tf,
		Objects: r.Objects,
		Refs:    r.Refs,
		Replay:  guard,
	}
	res, err := v.Verify(su)
	if err != nil {
		return err
	}
	if !res.Accepted {
		if res.Current != nil {
			return fmt.Errorf("promote rejected: %s (current head is %s — rebase and retry)",
				res.Reason, res.Current.Hex())
		}
		return fmt.Errorf("promote rejected: %s", res.Reason)
	}
	fmt.Printf("promoted %s to %s (signed by %s)\n", refName, newID.Hex(), id.Fingerprint())
	return nil
}
