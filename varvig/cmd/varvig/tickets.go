package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/attest"
	"github.com/dividebyzero/claude-experiments/varvig/internal/deps"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/provenance"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/reserved"
	"github.com/dividebyzero/claude-experiments/varvig/internal/score"
	"github.com/dividebyzero/claude-experiments/varvig/internal/ticket"
)

// cmdTickets is the ticket lifecycle surface (tickets §1.2, §3, §4):
//
//	varvig tickets new -m <spec>                                 mint a ticket (a ref + genesis revision)
//	varvig tickets revise <ticket> -m <spec>                     append an intent revision, move the ref
//	varvig tickets list                                          list tickets and their state
//	varvig tickets show <ticket>                                 spec, scope, status, blockers, score
//	varvig tickets scope <ticket> [--reads a,b] [--writes c,d]   declare/show a scope
//	varvig tickets blockers <ticket>                             tickets blocking this one
//	varvig tickets graph                                         the derived blocking graph
//	varvig tickets rank [--weights f.json]                       rank scoped tickets by score
//
// A ticket's identity is a ref (§1.2); mutation appends an immutable revision and
// moves the ref, so undo is the reflog. Blocking is never hand-declared: it is
// derived from declared read/write sets (§3.2). `rank` is the throughput half
// (§3.3): a score reorders, it never gates.
func cmdTickets(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: varvig tickets <new|revise|list|show|scope|blockers|graph|rank|backtest> ...")
	}
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	switch args[0] {
	case "new":
		return ticketsNew(r, args[1:])
	case "revise":
		return ticketsRevise(r, args[1:])
	case "list":
		return ticketsList(r)
	case "show":
		return ticketsShow(r, args[1:])
	case "spec":
		return ticketsSpec(r, args[1:])
	case "status":
		return ticketsStatus(r, args[1:])
	case "scope":
		return ticketsScope(r, args[1:])
	case "blockers":
		return ticketsBlockers(r, args[1:])
	case "graph":
		return ticketsGraph(r)
	case "rank":
		return ticketsRank(r, args[1:])
	case "backtest":
		return ticketsBacktest(r, args[1:])
	default:
		return fmt.Errorf("tickets: unknown subcommand %q", args[0])
	}
}

func dashM(args []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "-m" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func ticketsNew(r *repo.Repo, args []string) error {
	spec := dashM(args)
	if spec == "" {
		return errors.New("usage: varvig tickets new -m <spec>")
	}
	priv, err := provenance.LoadOrCreateIdentity(r.GitDir())
	if err != nil {
		return err
	}
	id, err := ticket.New(r, spec, priv, author(), time.Now().Unix())
	if err != nil {
		return err
	}
	fmt.Printf("ticket %s\n", id.Hex())
	return nil
}

func ticketsRevise(r *repo.Repo, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: varvig tickets revise <ticket> -m <spec>")
	}
	id, err := ticketID(r, args[0])
	if err != nil {
		return err
	}
	spec := dashM(args[1:])
	if spec == "" {
		return errors.New("usage: varvig tickets revise <ticket> -m <spec>")
	}
	priv, err := provenance.LoadOrCreateIdentity(r.GitDir())
	if err != nil {
		return err
	}
	rev, err := ticket.Revise(r, id, spec, priv, author(), time.Now().Unix())
	if err != nil {
		return err
	}
	fmt.Printf("revised %s -> %s\n", id.Hex()[4:16], rev.Hex())
	return nil
}

func ticketsList(r *repo.Repo) error {
	list, err := ticket.List(r)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("(no tickets)")
		return nil
	}
	for _, info := range list {
		atts, _ := attest.Attestations(r, info.Head)
		fmt.Printf("%s  %-9s  %s\n", info.ID.Hex()[4:16], attest.Derive(atts, object.StrengthStrong), info.Spec)
	}
	return nil
}

func ticketsShow(r *repo.Repo, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: varvig tickets show <ticket>")
	}
	id, err := ticketID(r, args[0])
	if err != nil {
		return err
	}
	info, err := ticket.Get(r, id)
	if err != nil {
		return err
	}
	fmt.Printf("ticket   %s\n", info.ID.Hex())
	fmt.Printf("head     %s\n", info.Head.Hex())
	fmt.Printf("spec     %s\n", info.Spec)

	atts, err := attest.Attestations(r, info.Head)
	if err != nil {
		return err
	}
	fmt.Printf("status   %s (require strong)\n", attest.Derive(atts, object.StrengthStrong))

	s, hasScope, err := deps.GetScope(r, info.Head)
	if err != nil {
		return err
	}
	if !hasScope {
		fmt.Println("scope    (none — unschedulable)")
		return nil
	}
	fmt.Printf("scope    reads=[%s] writes=[%s]\n", strings.Join(s.Reads, " "), strings.Join(s.Writes, " "))

	all, err := deps.ScopedTickets(r)
	if err != nil {
		return err
	}
	blockers := deps.Blockers(deps.Ticket{ID: info.Head, Scope: s}, all)
	if len(blockers) == 0 {
		fmt.Println("blockers (none — ready)")
	} else {
		var bs []string
		for _, b := range blockers {
			bs = append(bs, b.Hex()[4:16])
		}
		fmt.Printf("blockers %s\n", strings.Join(bs, " "))
	}
	return nil
}

// ticketsSpec prints a ticket's current spec verbatim (no label), so a tool —
// a bridge peer projecting the spec to a tracker — reads it losslessly,
// including a multi-line title+body, which the human-oriented `show` cannot.
func ticketsSpec(r *repo.Repo, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: varvig tickets spec <ticket>")
	}
	id, err := ticketID(r, args[0])
	if err != nil {
		return err
	}
	info, err := ticket.Get(r, id)
	if err != nil {
		return err
	}
	fmt.Print(info.Spec)
	return nil
}

// ticketsStatus prints a ticket's derived status word (pending|approved|vetoed)
// and nothing else, so a tool — a bridge peer projecting status onto a tracker —
// reads it unambiguously. The required strength defaults to strong.
func ticketsStatus(r *repo.Repo, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: varvig tickets status <ticket>")
	}
	head, err := resolveTicketHead(r, args[0])
	if err != nil {
		return err
	}
	atts, err := attest.Attestations(r, head)
	if err != nil {
		return err
	}
	fmt.Print(attest.Derive(atts, object.StrengthStrong).String())
	return nil
}

// ticketID resolves a ticket argument (a bare ticket id or a full ticket ref)
// to its stable ticket id.
func ticketID(r *repo.Repo, arg string) (multihash.Multihash, error) {
	suffix := strings.TrimPrefix(arg, reserved.TicketsPrefix)
	id, err := multihash.ParseHex(suffix)
	if err != nil {
		return nil, fmt.Errorf("tickets: %q is not a ticket id", arg)
	}
	if _, err := r.Refs.Resolve(reserved.TicketsPrefix + id.Hex()); err != nil {
		return nil, fmt.Errorf("tickets: no ticket %s", id.Hex())
	}
	return id, nil
}

// resolveTicketHead resolves a ticket argument to the revision to act on: a
// ticket's current head, or — when arg is not a ticket — whatever resolve()
// makes of it (a raw revision hash or another ref).
func resolveTicketHead(r *repo.Repo, arg string) (multihash.Multihash, error) {
	if strings.HasPrefix(arg, reserved.TicketsPrefix) {
		return r.Refs.Resolve(arg)
	}
	if id, err := multihash.ParseHex(arg); err == nil {
		if head, err := r.Refs.Resolve(reserved.TicketsPrefix + id.Hex()); err == nil {
			return head, nil
		}
	}
	return resolve(r, arg)
}

func ticketsScope(r *repo.Repo, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: varvig tickets scope <ref|id> [--reads a,b] [--writes c,d]")
	}
	head, err := resolveTicketHead(r, args[0])
	if err != nil {
		return fmt.Errorf("tickets: cannot resolve %q: %w", args[0], err)
	}
	var reads, writes []string
	setting := false
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--reads":
			if i+1 >= len(args) {
				return errors.New("tickets: --reads requires a value")
			}
			reads = splitCSV(args[i+1])
			setting = true
			i++
		case "--writes":
			if i+1 >= len(args) {
				return errors.New("tickets: --writes requires a value")
			}
			writes = splitCSV(args[i+1])
			setting = true
			i++
		default:
			return fmt.Errorf("tickets: unknown argument %q", args[i])
		}
	}

	if !setting {
		s, ok, err := deps.GetScope(r, head)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("(no scope declared)")
			return nil
		}
		fmt.Printf("reads:  %s\nwrites: %s\n", strings.Join(s.Reads, " "), strings.Join(s.Writes, " "))
		return nil
	}
	if _, err := deps.SetScope(r, head, deps.Scope{Reads: reads, Writes: writes}, author(), time.Now().Unix()); err != nil {
		return err
	}
	fmt.Printf("scope set for %s\n", head.Hex())
	return nil
}

func ticketsBlockers(r *repo.Repo, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: varvig tickets blockers <ref|id>")
	}
	head, err := resolveTicketHead(r, args[0])
	if err != nil {
		return fmt.Errorf("tickets: cannot resolve %q: %w", args[0], err)
	}
	s, ok, err := deps.GetScope(r, head)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("tickets: %s has no declared scope (unschedulable)", head.Hex())
	}
	all, err := deps.ScopedTickets(r)
	if err != nil {
		return err
	}
	blockers := deps.Blockers(deps.Ticket{ID: head, Scope: s}, all)
	if len(blockers) == 0 {
		fmt.Println("(no blockers)")
		return nil
	}
	for _, b := range blockers {
		fmt.Println(b.Hex())
	}
	return nil
}

func ticketsGraph(r *repo.Repo) error {
	all, err := deps.ScopedTickets(r)
	if err != nil {
		return err
	}
	if len(all) == 0 {
		fmt.Println("(no scoped tickets)")
		return nil
	}
	for _, t := range all {
		blockers := deps.Blockers(t, all)
		short := t.ID.Hex()[4:16]
		if len(blockers) == 0 {
			fmt.Printf("%s  (ready)\n", short)
			continue
		}
		var bs []string
		for _, b := range blockers {
			bs = append(bs, b.Hex()[4:16])
		}
		fmt.Printf("%s  blocked-by %s\n", short, strings.Join(bs, " "))
	}
	return nil
}

// defaultWeights is a documented heuristic starting point when no learned
// scorer is supplied: prefer work that frees the most conflicting tickets, then
// older work, and mildly discount a large blast radius. Replace it with weights
// learned from real decisions (score.Fit) once a corpus exists.
var defaultWeights = score.Weights{Unblocks: 1.0, AgeSeconds: 1e-6, BlastRadius: -0.1}

func ticketsRank(r *repo.Repo, args []string) error {
	w := defaultWeights
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--weights":
			if i+1 >= len(args) {
				return errors.New("tickets: --weights requires a file")
			}
			b, err := os.ReadFile(args[i+1])
			if err != nil {
				return err
			}
			w, err = score.UnmarshalWeights(b)
			if err != nil {
				return fmt.Errorf("tickets: bad weights file: %w", err)
			}
			i++
		default:
			return fmt.Errorf("tickets: unknown argument %q", args[i])
		}
	}
	all, err := deps.ScopedTickets(r)
	if err != nil {
		return err
	}
	if len(all) == 0 {
		fmt.Println("(no scoped tickets)")
		return nil
	}
	for _, rk := range score.RankTickets(r, w, all, time.Now().Unix()) {
		f := rk.Features
		fmt.Printf("%s  score %+.3f  (blast %.0f, unblocks %.0f, age %.0fs)\n",
			rk.ID.Hex()[4:16], rk.Score, f.BlastRadius, f.Unblocks, f.AgeSeconds)
	}
	return nil
}

// ticketsBacktest learns a scorer from the repository's recorded approve/veto
// decisions and reports how well it reproduces them (tickets §3.3, §3.4). It is
// the "promote a scorer only if the review passes" loop: fit, then read the
// disagreements before trusting the weights. With -o it writes the weights.
func ticketsBacktest(r *repo.Repo, args []string) error {
	epochs := 30
	var out string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--epochs":
			if i+1 >= len(args) {
				return errors.New("tickets: --epochs requires a value")
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < 1 {
				return fmt.Errorf("tickets: bad --epochs %q", args[i+1])
			}
			epochs = n
			i++
		case "-o":
			if i+1 >= len(args) {
				return errors.New("tickets: -o requires a file")
			}
			out = args[i+1]
			i++
		default:
			return fmt.Errorf("tickets: unknown argument %q", args[i])
		}
	}

	w, rep, err := score.FitFromHistory(r, epochs, time.Now().Unix())
	if err != nil {
		return err
	}
	if rep.Total == 0 {
		fmt.Println("(no recorded decisions to learn from — approve and veto some tickets first)")
		return nil
	}
	fmt.Printf("corpus   %d comparisons from recorded decisions\n", rep.Total)
	fmt.Printf("agree    %d\n", rep.Agree)
	fmt.Printf("disagree %d\n", rep.Disagree)
	fmt.Printf("weights  blast=%.3f unblocks=%.3f age=%.3g\n", w.BlastRadius, w.Unblocks, w.AgeSeconds)
	if out != "" {
		b, err := w.Marshal()
		if err != nil {
			return err
		}
		if err := os.WriteFile(out, b, 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", out)
	}
	return nil
}

func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
