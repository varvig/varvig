package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/mcp"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/readapi"
	"github.com/dividebyzero/claude-experiments/varvig/internal/refs"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/task"
	"github.com/dividebyzero/claude-experiments/varvig/internal/worktree"
)

// cmdMcp serves the MCP gate over stdio (auth design §8). It is a subcommand of
// the core binary, not a client: agents are the primary user, a sandbox needs
// MCP on every task, and read-logging into provenance is a core write concern
// (§8). The gate holds no authority of its own — it mints a scoped, propose-only,
// expiring task credential and does no more than that credential allows.
//
//	varvig mcp [--scope S] [--ttl DUR] [--base REF] [--propose-only]
//
// stdin/stdout carry the JSON-RPC stream; all human-facing output goes to stderr
// so it cannot corrupt the protocol channel.
func cmdMcp(args []string) error {
	scope := "/"
	ttl := time.Hour
	base := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--scope":
			if i+1 >= len(args) {
				return errors.New("mcp: --scope requires a value")
			}
			scope, i = args[i+1], i+1
		case "--ttl":
			if i+1 >= len(args) {
				return errors.New("mcp: --ttl requires a value")
			}
			d, err := time.ParseDuration(args[i+1])
			if err != nil {
				return fmt.Errorf("mcp: bad --ttl: %w", err)
			}
			ttl, i = d, i+1
		case "--base":
			if i+1 >= len(args) {
				return errors.New("mcp: --base requires a value")
			}
			base, i = args[i+1], i+1
		case "--propose-only":
			// The only mode a task key has at v1; accepted for clarity.
		default:
			return fmt.Errorf("mcp: unknown argument %q", args[i])
		}
	}

	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	baseID, err := resolveBase(r, base)
	if err != nil {
		return err
	}
	grant, err := task.New(scope, true, ttl, time.Now())
	if err != nil {
		return err
	}

	// Announce the credential on stderr — stdout is the JSON-RPC channel.
	fmt.Fprintf(os.Stderr, "varvig mcp: task %s scope %s key %s (expires %s)\n",
		grant.ID, grant.Scope, grant.Fingerprint(),
		time.Unix(grant.NotAfter, 0).Format(time.Kitchen))
	fmt.Fprintf(os.Stderr, "varvig mcp: base %s; propose-only, cannot promote\n", hexOrNone(baseID))

	gate := mcp.NewGate(r, grant, baseID)
	return gate.Serve(stdio{})
}

// cmdTask manages task credentials (auth design §6). Only `task start` exists at
// v1: mint an ephemeral key, carve out the scoped sparse checkout (the read set),
// and print how to serve the MCP gate for it.
//
//	varvig task start [--scope S] [--ttl DUR] [--base REF] [dir]
func cmdTask(args []string) error {
	if len(args) < 1 || args[0] != "start" {
		return errors.New("usage: varvig task start [--scope S] [--ttl DUR] [--base REF] [dir]")
	}
	scope := "/"
	ttl := time.Hour
	base := ""
	dir := ""
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--scope":
			if i+1 >= len(rest) {
				return errors.New("task start: --scope requires a value")
			}
			scope, i = rest[i+1], i+1
		case "--ttl":
			if i+1 >= len(rest) {
				return errors.New("task start: --ttl requires a value")
			}
			d, err := time.ParseDuration(rest[i+1])
			if err != nil {
				return fmt.Errorf("task start: bad --ttl: %w", err)
			}
			ttl, i = d, i+1
		case "--base":
			if i+1 >= len(rest) {
				return errors.New("task start: --base requires a value")
			}
			base, i = rest[i+1], i+1
		case "--propose-only":
			// The only mode at v1.
		default:
			dir = rest[i]
		}
	}

	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	baseID, err := resolveBase(r, base)
	if err != nil {
		return err
	}
	grant, err := task.New(scope, true, ttl, time.Now())
	if err != nil {
		return err
	}
	if dir == "" {
		dir = "./task-" + grant.ID
	}

	// Sparse checkout of the read set: scope equals the checkout equals the API's
	// visibility (§6.2). We materialize only the scope subtree, at its
	// repo-relative path, so proposed paths line up with what the gate enforces.
	checkoutDesc := "(none — empty repo)"
	if baseID != nil {
		q := readapi.New(r)
		scopePath := trimScope(scope)
		listing, err := q.Tree(baseID, scopePath)
		if err != nil {
			return fmt.Errorf("task start: cannot resolve scope %q: %w", scope, err)
		}
		subTree, err := multihash.ParseHex(listing.Hash)
		if err != nil {
			return err
		}
		dest := dir
		if scopePath != "" {
			dest = filepath.Join(dir, filepath.FromSlash(scopePath))
		}
		if err := worktree.Checkout(r.Objects, subTree, dest); err != nil {
			return err
		}
		checkoutDesc = dest
	}

	fmt.Printf("task %s\n", grant.ID)
	fmt.Printf("  scope     %s\n", grant.Scope)
	fmt.Printf("  key       %s  (ephemeral, expires %s)\n",
		grant.Fingerprint(), time.Unix(grant.NotAfter, 0).Format(time.Kitchen))
	fmt.Printf("  base      %s\n", hexOrNone(baseID))
	fmt.Printf("  checkout  %s\n", checkoutDesc)
	fmt.Printf("  mcp       varvig mcp --scope %s --ttl %s   # serve the gate over stdio\n", scope, ttl)
	fmt.Fprintln(os.Stderr, "note: propose-only — a task key can create proposals but never promote a ref")
	fmt.Fprintln(os.Stderr, "note: at v1 the ephemeral key lives with its process; `varvig mcp` mints its own scoped key")
	return nil
}

// resolveBase resolves the task's base state: an explicit ref/hash, else HEAD.
// A repository with no commits yet resolves to a nil base (an empty tree), which
// the gate handles.
func resolveBase(r *repo.Repo, ref string) (multihash.Multihash, error) {
	if ref != "" {
		return resolve(r, ref)
	}
	headRef, err := r.Head()
	if err != nil {
		return nil, err
	}
	id, err := r.Refs.Resolve(headRef)
	if errors.Is(err, refs.ErrNotExist) {
		return nil, nil
	}
	return id, err
}

func trimScope(scope string) string {
	s := scope
	for len(s) > 0 && s[0] == '/' {
		s = s[1:]
	}
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	if s == "." {
		return ""
	}
	return s
}

func hexOrNone(m multihash.Multihash) string {
	if m == nil {
		return "(none)"
	}
	return m.Hex()
}

// stdio is an io.ReadWriter bridging the process's stdin and stdout, for the MCP
// stdio transport.
type stdio struct{}

func (stdio) Read(b []byte) (int, error)  { return os.Stdin.Read(b) }
func (stdio) Write(b []byte) (int, error) { return os.Stdout.Write(b) }
