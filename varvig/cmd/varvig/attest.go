package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/attest"
	"github.com/dividebyzero/claude-experiments/varvig/internal/identity"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/reserved"
)

// cmdAttest records and inspects governance attestations (tickets §2): signed
// approve / veto / request-change decisions bound to a specific intent revision
// hash. Status is derived from attestations, never stored, so these commands
// only ever write signed decisions and compute the rest.
//
//	varvig attest approve <ref|id> [--strength strong|delegated] [-m rationale]
//	varvig attest veto <ref|id> [-m rationale]
//	varvig attest request-change <ref|id> [-m rationale]
//	varvig attest list <ref|id>
//	varvig attest status <ref|id> [--require weak|delegated|strong]
func cmdAttest(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: varvig attest <approve|veto|request-change|list|status|policy> ...")
	}
	sub := args[0]
	r, err := repo.Open(".")
	if err != nil {
		return err
	}

	// `policy` manages the repository's promotion-policy module and takes no
	// intent target, so it is handled before the target is resolved.
	if sub == "policy" {
		return attestPolicy(r, args[1:])
	}

	if len(args) < 2 {
		return errors.New("usage: varvig attest <approve|veto|request-change|list|status> <ref|id> [opts]")
	}
	target, err := resolve(r, args[1])
	if err != nil {
		return fmt.Errorf("attest: cannot resolve %q: %w", args[1], err)
	}
	rest := args[2:]

	switch sub {
	case "approve":
		strength := object.StrengthStrong
		rationale := ""
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--strength":
				if i+1 >= len(rest) {
					return errors.New("attest: --strength requires a value")
				}
				s, err := parseStrength(rest[i+1])
				if err != nil {
					return err
				}
				if s == object.StrengthWeak {
					return errors.New("attest: weak is reserved for bridges (a keyholder signs strong or delegated)")
				}
				strength = s
				i++
			case "-m":
				if i+1 >= len(rest) {
					return errors.New("attest: -m requires a value")
				}
				rationale = rest[i+1]
				i++
			default:
				return fmt.Errorf("attest: unknown argument %q", rest[i])
			}
		}
		return signAndAttach(r, target, object.DecisionApprove, strength, rationale)
	case "veto", "request-change":
		rationale := ""
		for i := 0; i < len(rest); i++ {
			if rest[i] == "-m" && i+1 < len(rest) {
				rationale = rest[i+1]
				i++
				continue
			}
			return fmt.Errorf("attest: unknown argument %q", rest[i])
		}
		decision := object.DecisionVeto
		if sub == "request-change" {
			decision = object.DecisionRequestChange
		}
		return signAndAttach(r, target, decision, object.StrengthStrong, rationale)
	case "list":
		return attestList(r, target)
	case "status":
		required := object.StrengthStrong
		for i := 0; i < len(rest); i++ {
			if rest[i] == "--require" && i+1 < len(rest) {
				s, err := parseStrength(rest[i+1])
				if err != nil {
					return err
				}
				required = s
				i++
				continue
			}
			return fmt.Errorf("attest: unknown argument %q", rest[i])
		}
		return attestStatus(r, target, required)
	default:
		return fmt.Errorf("attest: unknown subcommand %q", sub)
	}
}

// signAndAttach signs an attestation with the active identity and attaches it.
func signAndAttach(r *repo.Repo, target multihash.Multihash, decision object.Decision, strength object.Strength, rationale string) error {
	id, err := identity.Resolve("", os.Getenv)
	if err != nil {
		return err
	}
	defer id.Close()
	if !id.CanSign() {
		return errors.New("attest: active identity cannot sign (encrypted key and no ssh-agent); " +
			"start ssh-agent and `ssh-add` your key")
	}
	obj, err := attest.Sign(id.Signer, object.Attestation{
		Target:    target,
		Decision:  decision,
		Strength:  strength,
		Timestamp: time.Now().Unix(),
		Rationale: rationale,
	})
	if err != nil {
		return err
	}
	if _, err := attest.Attach(r, obj, id.Fingerprint(), time.Now().Unix()); err != nil {
		return err
	}
	fmt.Printf("%s %s (%s) signed by %s\n", decision, target.Hex(), strength, id.Fingerprint())
	return nil
}

func attestList(r *repo.Repo, target multihash.Multihash) error {
	entries, err := attest.List(r, target)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Println("(no attestations)")
		return nil
	}
	for _, e := range entries {
		a := e.Attestation
		line := fmt.Sprintf("%-14s %-9s %s", a.Decision, a.Strength, attest.Fingerprint(e.SignerKey))
		if a.Rationale != "" {
			line += "  " + a.Rationale
		}
		fmt.Println(line)
	}
	return nil
}

func attestStatus(r *repo.Repo, target multihash.Multihash, required object.Strength) error {
	atts, err := attest.Attestations(r, target)
	if err != nil {
		return err
	}
	fmt.Printf("%s (require %s)\n", attest.Derive(atts, required), required)
	return nil
}

// attestPolicy manages the repository's promotion-policy wasm module
// (refs/varvig/policy, tickets §2.5):
//
//	varvig attest policy set <module.wasm>   store the module and point the ref at it
//	varvig attest policy show                print the module's id, or "(none)"
//	varvig attest policy clear               remove the policy
func attestPolicy(r *repo.Repo, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: varvig attest policy <set <module.wasm>|show|clear>")
	}
	switch args[0] {
	case "set":
		if len(args) != 2 {
			return errors.New("usage: varvig attest policy set <module.wasm>")
		}
		mod, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		id, err := r.Objects.Put(object.NewBlob(mod))
		if err != nil {
			return err
		}
		cur, _ := r.Refs.Resolve(reserved.PolicyRef)
		if err := r.Refs.CompareAndSwap(reserved.PolicyRef, cur, id, author(), "attest policy set"); err != nil {
			return err
		}
		fmt.Printf("policy set to %s\n", id.Hex())
		return nil
	case "show":
		id, err := r.Refs.Resolve(reserved.PolicyRef)
		if err != nil {
			fmt.Println("(none)")
			return nil
		}
		fmt.Println(id.Hex())
		return nil
	case "clear":
		cur, err := r.Refs.Resolve(reserved.PolicyRef)
		if err != nil {
			fmt.Println("(none)")
			return nil
		}
		if err := r.Refs.Delete(reserved.PolicyRef, cur, author(), "attest policy clear"); err != nil {
			return err
		}
		fmt.Println("policy cleared")
		return nil
	default:
		return fmt.Errorf("attest: unknown policy subcommand %q", args[0])
	}
}

func parseStrength(s string) (object.Strength, error) {
	switch s {
	case "weak":
		return object.StrengthWeak, nil
	case "delegated":
		return object.StrengthDelegated, nil
	case "strong":
		return object.StrengthStrong, nil
	default:
		return object.StrengthUnknown, fmt.Errorf("attest: unknown strength %q (want weak|delegated|strong)", s)
	}
}
