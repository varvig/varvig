package main

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/bridge"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/provenance"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/ticket"
)

// cmdBridge is the vendor-neutral surface an external-tracker peer drives
// (tickets §5). The core names no tracker: `system` is an opaque tag the peer
// chooses, and every operation is capped by the core's governance (a bridge key
// can only ever produce weak attestations, §2.4). The peer that talks a real
// tracker's API lives outside this repo and shells out to these verbs.
//
//	varvig bridge link <ticket> [--system S --foreign-id ID]   set/show the external link
//	varvig bridge needs-push <ticket>                          is there local work to push?
//	varvig bridge mark-pushed <ticket>                         record the head as pushed
//	varvig bridge apply-inbound <ticket> -m <spec> [--author A] apply a tracker edit
//	varvig bridge transition <ticket> <approve|veto|request-change> [-m msg]
//
// The signing key is the repository's active identity, which a bridge peer
// registers as `kind: bridge` via `varvig principal add`.
func cmdBridge(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: varvig bridge <link|needs-push|mark-pushed|apply-inbound|transition> ...")
	}
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	switch args[0] {
	case "link":
		return bridgeLink(r, args[1:])
	case "needs-push":
		return bridgeNeedsPush(r, args[1:])
	case "mark-pushed":
		return bridgeMarkPushed(r, args[1:])
	case "apply-inbound":
		return bridgeApplyInbound(r, args[1:])
	case "transition":
		return bridgeTransition(r, args[1:])
	default:
		return fmt.Errorf("bridge: unknown subcommand %q", args[0])
	}
}

func bridgeLink(r *repo.Repo, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: varvig bridge link <ticket> [--system S --foreign-id ID]")
	}
	id, err := ticketID(r, args[0])
	if err != nil {
		return err
	}
	var system, foreignID string
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--system":
			if i+1 >= len(args) {
				return errors.New("bridge: --system requires a value")
			}
			system = args[i+1]
			i++
		case "--foreign-id":
			if i+1 >= len(args) {
				return errors.New("bridge: --foreign-id requires a value")
			}
			foreignID = args[i+1]
			i++
		default:
			return fmt.Errorf("bridge: unknown argument %q", args[i])
		}
	}

	if system == "" && foreignID == "" {
		link, ok, err := bridge.GetLink(r, id)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("(no external link)")
			return nil
		}
		fmt.Printf("system     %s\nforeign-id %s\nlast-pushed %s\nlast-pulled %s\n",
			link.System, link.ForeignID, dashIfEmpty(link.LastPushed), dashIfEmpty(link.LastPulled))
		return nil
	}
	if system == "" || foreignID == "" {
		return errors.New("bridge: setting a link needs both --system and --foreign-id")
	}
	// Preserve existing watermarks when updating identity fields.
	link, _, err := bridge.GetLink(r, id)
	if err != nil {
		return err
	}
	link.System, link.ForeignID = system, foreignID
	if err := bridge.SetLink(r, id, link, author(), time.Now().Unix()); err != nil {
		return err
	}
	fmt.Printf("linked %s to %s:%s\n", id.Hex()[4:16], system, foreignID)
	return nil
}

func bridgeNeedsPush(r *repo.Repo, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: varvig bridge needs-push <ticket>")
	}
	id, err := ticketID(r, args[0])
	if err != nil {
		return err
	}
	need, err := bridge.NeedsPush(r, id)
	if err != nil {
		return err
	}
	if need {
		fmt.Println("yes")
	} else {
		fmt.Println("no")
	}
	return nil
}

func bridgeMarkPushed(r *repo.Repo, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: varvig bridge mark-pushed <ticket>")
	}
	id, err := ticketID(r, args[0])
	if err != nil {
		return err
	}
	if err := bridge.MarkPushed(r, id, author(), time.Now().Unix()); err != nil {
		return err
	}
	fmt.Printf("marked %s pushed\n", id.Hex()[4:16])
	return nil
}

func bridgeApplyInbound(r *repo.Repo, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: varvig bridge apply-inbound <ticket> -m <spec> [--author A]")
	}
	id, err := ticketID(r, args[0])
	if err != nil {
		return err
	}
	spec := dashM(args[1:])
	authorLabel := author()
	for i := 1; i < len(args); i++ {
		if args[i] == "--author" && i+1 < len(args) {
			authorLabel = args[i+1]
			i++
		}
	}
	if spec == "" {
		return errors.New("bridge: apply-inbound needs -m <spec>")
	}
	priv, err := bridgeKey(r)
	if err != nil {
		return err
	}
	rev, changed, err := bridge.ApplyInbound(r, id, spec, priv, authorLabel, time.Now().Unix())
	if err != nil {
		return err
	}
	if !changed {
		fmt.Printf("no change (%s)\n", rev.Hex()[4:16])
		return nil
	}
	fmt.Printf("applied inbound edit: %s\n", rev.Hex())
	return nil
}

func bridgeTransition(r *repo.Repo, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: varvig bridge transition <ticket> <approve|veto|request-change> [-m msg]")
	}
	id, err := ticketID(r, args[0])
	if err != nil {
		return err
	}
	decision, err := parseDecision(args[1])
	if err != nil {
		return err
	}
	rationale := dashM(args[2:])
	head, err := ticket.Head(r, id)
	if err != nil {
		return err
	}
	priv, err := bridgeKey(r)
	if err != nil {
		return err
	}
	if _, err := bridge.RecordTransition(r, priv, head, decision, rationale, time.Now().Unix()); err != nil {
		return err
	}
	fmt.Printf("recorded weak %s on %s\n", decision, head.Hex()[4:16])
	return nil
}

// bridgeKey returns the repository's Ed25519 identity, the key a bridge peer
// signs with (registered as kind: bridge via `varvig principal add`).
func bridgeKey(r *repo.Repo) (ed25519.PrivateKey, error) {
	return provenance.LoadOrCreateIdentity(r.GitDir())
}

func parseDecision(s string) (object.Decision, error) {
	switch s {
	case "approve":
		return object.DecisionApprove, nil
	case "veto":
		return object.DecisionVeto, nil
	case "request-change":
		return object.DecisionRequestChange, nil
	default:
		return object.DecisionUnknown, fmt.Errorf("bridge: unknown transition %q (want approve|veto|request-change)", s)
	}
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
