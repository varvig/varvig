package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/blocked"
	"github.com/dividebyzero/claude-experiments/varvig/internal/identity"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// cmdBlocked is the scope-authority side of the blocked-on-scope outcome (build
// spec P1.2). A task that hits a boundary it cannot cross records a signed
// blocked-on-scope report through the gate; this is where whoever holds scope
// authority reads those requests and records a widening decision. Widening never
// happens automatically — it is a decision with an author, so `widen` signs it
// and stores it beside the request; applying it is the next, separate step of
// minting a fresh task with the wider scope.
//
//	varvig blocked list <intent>                       # requests + widenings for an intent revision
//	varvig blocked widen <intent> --to <scope> [--reason R]
func cmdBlocked(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: varvig blocked <list|widen> ...")
	}
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		if len(args) != 2 {
			return errors.New("usage: varvig blocked list <intent>")
		}
		return blockedList(r, args[1])
	case "widen":
		return blockedWiden(r, args[1:])
	default:
		return fmt.Errorf("blocked: unknown subcommand %q (want: list, widen)", args[0])
	}
}

func blockedList(r *repo.Repo, ref string) error {
	intent, err := resolve(r, ref)
	if err != nil {
		return fmt.Errorf("blocked list: %w", err)
	}
	trace, err := blocked.Provenance(r, intent)
	if err != nil {
		return err
	}
	if len(trace.Requests) == 0 && len(trace.Widenings) == 0 {
		fmt.Println("(no blocked-on-scope activity for this intent)")
		return nil
	}
	for _, req := range trace.Requests {
		fmt.Printf("blocked  scope %s  needs %s\n", req.Scope, req.Need)
		if req.Why != "" {
			fmt.Printf("         why    %s\n", req.Why)
		}
		if req.Unmet != "" {
			fmt.Printf("         unmet  %s\n", req.Unmet)
		}
		fmt.Printf("         author %s  hits %d\n", req.Author, len(req.Hits))
		for _, h := range req.Hits {
			fmt.Printf("           - %s (%s)\n", h.Path, h.Reason)
		}
	}
	for _, w := range trace.Widenings {
		fmt.Printf("widened  %s -> %s  by %s", w.FromScope, w.ToScope, w.Decider)
		if w.Reason != "" {
			fmt.Printf("  (%s)", w.Reason)
		}
		fmt.Println()
	}
	return nil
}

func blockedWiden(r *repo.Repo, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: varvig blocked widen <intent> --to <scope> [--reason R]")
	}
	ref := args[0]
	to := ""
	reason := ""
	from := ""
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--to":
			if i+1 >= len(args) {
				return errors.New("blocked widen: --to requires a scope")
			}
			to, i = args[i+1], i+1
		case "--from":
			if i+1 >= len(args) {
				return errors.New("blocked widen: --from requires a scope")
			}
			from, i = args[i+1], i+1
		case "--reason":
			if i+1 >= len(args) {
				return errors.New("blocked widen: --reason requires a value")
			}
			reason, i = args[i+1], i+1
		default:
			return fmt.Errorf("blocked widen: unknown argument %q", args[i])
		}
	}
	if to == "" {
		return errors.New("blocked widen: --to <scope> is required (widening is a decision with a destination)")
	}
	intent, err := resolve(r, ref)
	if err != nil {
		return fmt.Errorf("blocked widen: %w", err)
	}
	// Default the recorded original scope to the most recent open request's scope,
	// so the widening trace pairs with what the task actually declared.
	if from == "" {
		if reqs, err := blocked.List(r, intent); err == nil && len(reqs) > 0 {
			from = reqs[0].Scope
		}
	}

	id, err := identity.Resolve("", os.Getenv)
	if err != nil {
		return err
	}
	defer id.Close()
	if !id.CanSign() {
		return errors.New("blocked widen: active identity cannot sign (encrypted key and no ssh-agent); " +
			"start ssh-agent and `ssh-add` your key")
	}
	noteID, err := blocked.Widen(r, id.Signer, blocked.Widening{
		Intent:    intent.Hex(),
		FromScope: from,
		ToScope:   to,
		Decider:   id.Fingerprint(),
		Reason:    reason,
		Timestamp: time.Now().Unix(),
	})
	if err != nil {
		return err
	}
	fmt.Printf("widened %s -> %s (intent %s) recorded %s\n", from, to, intent.Hex(), noteID.Hex())
	fmt.Fprintln(os.Stderr, "note: this records the decision only — start a fresh task with the wider --scope to act on it")
	return nil
}
