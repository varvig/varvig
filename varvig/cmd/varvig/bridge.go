package main

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/attest"
	"github.com/dividebyzero/claude-experiments/varvig/internal/bridge"
	"github.com/dividebyzero/claude-experiments/varvig/internal/provenance"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// cmdBridge is the vendor-neutral surface an external-tracker peer drives
// (tickets §5). The core names no tracker: `system` is an opaque tag the peer
// chooses, and every operation is capped by the core's governance (a bridge key
// can only ever produce weak attestations, §2.4). The peer that talks a real
// tracker's API lives outside this repo and shells out to these verbs.
//
//	varvig bridge link <ticket> [--system S --foreign-id ID]   set/show the external link
//	varvig bridge list [--system S]                            list linked tickets (id, system, foreign-id)
//	varvig bridge needs-push <ticket>                          is there local work to push?
//	varvig bridge mark-pushed <ticket>                         record the head as pushed
//	varvig bridge apply-inbound <ticket> -m <spec> [--author A] apply a tracker edit
//	varvig bridge transition <ticket> <approve|veto|request-change> [-m msg]
//	varvig bridge nudge <ticket> <0..1>                        set a priority nudge (scoring input)
//	varvig bridge assignee <ticket> <name>                     mirror the tracker assignee (informational)
//
// The signing key is the repository's active identity, which a bridge peer
// registers as `kind: bridge` via `varvig principal add`.
func cmdBridge(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: varvig bridge <link|list|needs-push|mark-pushed|apply-inbound|transition|nudge|assignee> ...")
	}
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	switch args[0] {
	case "link":
		return bridgeLink(r, args[1:])
	case "list":
		return bridgeList(r, args[1:])
	case "needs-push":
		return bridgeNeedsPush(r, args[1:])
	case "mark-pushed":
		return bridgeMarkPushed(r, args[1:])
	case "apply-inbound":
		return bridgeApplyInbound(r, args[1:])
	case "transition":
		return bridgeTransition(r, args[1:])
	case "nudge":
		return bridgeNudge(r, args[1:])
	case "assignee":
		return bridgeAssignee(r, args[1:])
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
		fmt.Printf("system     %s\nforeign-id %s\nlast-pushed %s\nlast-pulled %s\nnudge      %s\nassignee   %s\n",
			link.System, link.ForeignID, dashIfEmpty(link.LastPushed), dashIfEmpty(link.LastPulled),
			nudgeStr(link.PriorityNudge), dashIfEmpty(link.Assignee))
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

// bridgeList prints every linked ticket, one per line, as
// "<ticketid-hex>\t<system>\t<foreign-id>" — the machine-readable index a
// connector reads to sync all the tickets it mirrors in one pass. --system
// filters to a single opaque system tag.
func bridgeList(r *repo.Repo, args []string) error {
	var system string
	for i := 0; i < len(args); i++ {
		if args[i] == "--system" && i+1 < len(args) {
			system = args[i+1]
			i++
		} else {
			return fmt.Errorf("bridge: unknown argument %q", args[i])
		}
	}
	links, err := bridge.ListLinks(r, system)
	if err != nil {
		return err
	}
	for _, l := range links {
		fmt.Printf("%s\t%s\t%s\n", l.TicketID.Hex(), l.Link.System, l.Link.ForeignID)
	}
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
	priv, err := bridgeKey(r)
	if err != nil {
		return err
	}
	// Idempotent: a tracker reports the same transition on every poll, so this
	// records a weak attestation only when the transition is new for the head.
	recorded, err := bridge.RecordTransitionOnce(r, priv, id, decision, rationale, time.Now().Unix())
	if err != nil {
		return err
	}
	if !recorded {
		fmt.Printf("no change (%s already recorded)\n", decision)
		return nil
	}
	fmt.Printf("recorded weak %s\n", decision)
	return nil
}

// bridgeNudge sets a ticket's external priority nudge (§5.2): a value in [0,1]
// the connector projects from the tracker. It is only ever a scoring input —
// the scorer learns how much to trust it — never an authoritative priority.
func bridgeNudge(r *repo.Repo, args []string) error {
	if len(args) != 2 {
		return errors.New("usage: varvig bridge nudge <ticket> <0..1>")
	}
	id, err := ticketID(r, args[0])
	if err != nil {
		return err
	}
	nudge, err := strconv.ParseFloat(args[1], 64)
	if err != nil {
		return fmt.Errorf("bridge: nudge must be a number in [0,1]: %w", err)
	}
	if nudge < 0 || nudge > 1 {
		return fmt.Errorf("bridge: nudge %v is out of range [0,1]", nudge)
	}
	if err := bridge.SetNudge(r, id, nudge, author(), time.Now().Unix()); err != nil {
		return err
	}
	fmt.Printf("nudge %s = %s\n", id.Hex()[4:16], nudgeStr(nudge))
	return nil
}

// bridgeAssignee mirrors the tracker's assignee onto the ticket (§5.2),
// informational only. An empty name clears it.
func bridgeAssignee(r *repo.Repo, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: varvig bridge assignee <ticket> [name]")
	}
	id, err := ticketID(r, args[0])
	if err != nil {
		return err
	}
	var name string
	if len(args) > 1 {
		name = args[1]
	}
	if err := bridge.SetAssignee(r, id, name, author(), time.Now().Unix()); err != nil {
		return err
	}
	fmt.Printf("assignee %s = %s\n", id.Hex()[4:16], dashIfEmpty(name))
	return nil
}

// nudgeStr renders a priority nudge, showing "-" when unset (zero).
func nudgeStr(n float64) string {
	if n == 0 {
		return "-"
	}
	return strconv.FormatFloat(n, 'g', -1, 64)
}

// bridgeKey returns the repository's Ed25519 identity, the key a bridge peer
// signs with (registered as kind: bridge via `varvig principal add`).
func bridgeKey(r *repo.Repo) (ed25519.PrivateKey, error) {
	return provenance.LoadOrCreateIdentity(r.GitDir())
}

func parseDecision(s string) (attest.Decision, error) {
	switch s {
	case "approve":
		return attest.Approve, nil
	case "veto":
		return attest.Veto, nil
	case "request-change":
		return attest.RequestChange, nil
	default:
		return attest.DecisionUnknown, fmt.Errorf("bridge: unknown transition %q (want approve|veto|request-change)", s)
	}
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
