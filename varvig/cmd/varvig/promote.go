package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/identity"
	"github.com/dividebyzero/claude-experiments/varvig/internal/refs"
	"github.com/dividebyzero/claude-experiments/varvig/internal/refupdate"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// cmdPromote moves a ref by way of a signed ref update (auth design §5, §10.7).
// The signature travels with the change: the same bytes are signed here and
// verified by the pipeline, so a promotion is authorized by *who signed it*,
// not by the channel it arrived over.
//
//	varvig promote <ref> <new> [--scope S] [--ttl SECONDS]
//
// The lease (expected_old) is the ref's current value, so a concurrent move is
// rejected as a conflict and the caller is told the new head to rebase onto —
// the embryonic regeneration-merge retry of §10.6.
func cmdPromote(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: varvig promote <ref> <new> [--scope S] [--ttl SECONDS]")
	}
	refName, newArg := args[0], args[1]
	scope := "/"
	ttl := int64(3600)
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
