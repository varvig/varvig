package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/deps"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/score"
)

// cmdTickets inspects and declares ticket scheduling metadata (tickets §3):
//
//	varvig tickets scope <ref|id> [--reads a,b] [--writes c,d]   declare a scope
//	varvig tickets scope <ref|id>                                 show the scope
//	varvig tickets blockers <ref|id>                              tickets blocking this one
//	varvig tickets graph                                          the derived blocking graph
//	varvig tickets rank [--weights f.json]                        rank scoped tickets by score
//
// Blocking is never hand-declared: it is derived from declared read/write sets
// (§3.2), so `blockers` and `graph` are queries over scope, not a stored graph.
// `rank` is the throughput half (§3.3): a score reorders, it never gates.
func cmdTickets(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: varvig tickets <scope|blockers|graph|rank> ...")
	}
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	switch args[0] {
	case "scope":
		return ticketsScope(r, args[1:])
	case "blockers":
		return ticketsBlockers(r, args[1:])
	case "graph":
		return ticketsGraph(r)
	case "rank":
		return ticketsRank(r, args[1:])
	default:
		return fmt.Errorf("tickets: unknown subcommand %q", args[0])
	}
}

func ticketsScope(r *repo.Repo, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: varvig tickets scope <ref|id> [--reads a,b] [--writes c,d]")
	}
	ticket, err := resolve(r, args[0])
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
		s, ok, err := deps.GetScope(r, ticket)
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
	if _, err := deps.SetScope(r, ticket, deps.Scope{Reads: reads, Writes: writes}, author(), time.Now().Unix()); err != nil {
		return err
	}
	fmt.Printf("scope set for %s\n", ticket.Hex())
	return nil
}

func ticketsBlockers(r *repo.Repo, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: varvig tickets blockers <ref|id>")
	}
	ticket, err := resolve(r, args[0])
	if err != nil {
		return fmt.Errorf("tickets: cannot resolve %q: %w", args[0], err)
	}
	s, ok, err := deps.GetScope(r, ticket)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("tickets: %s has no declared scope (unschedulable)", ticket.Hex())
	}
	all, err := deps.ScopedTickets(r)
	if err != nil {
		return err
	}
	blockers := deps.Blockers(deps.Ticket{ID: ticket, Scope: s}, all)
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
