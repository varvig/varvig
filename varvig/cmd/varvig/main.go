// Command varvig is the single, portable Varvig binary. Per design §3.1 it is a
// busybox-style multicall executable: one artifact is client, server, and
// tooling. It dispatches on either the first argument or, when invoked under a
// command-specific name (e.g. a "varvig-commit" symlink), on argv[0].
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/affected"
	"github.com/dividebyzero/claude-experiments/varvig/internal/attest"
	"github.com/dividebyzero/claude-experiments/varvig/internal/conformance"
	"github.com/dividebyzero/claude-experiments/varvig/internal/gc"
	"github.com/dividebyzero/claude-experiments/varvig/internal/gitport"
	"github.com/dividebyzero/claude-experiments/varvig/internal/hook"
	"github.com/dividebyzero/claude-experiments/varvig/internal/merge"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/notes"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/p2p"
	"github.com/dividebyzero/claude-experiments/varvig/internal/peercred"
	"github.com/dividebyzero/claude-experiments/varvig/internal/provenance"
	"github.com/dividebyzero/claude-experiments/varvig/internal/readapi"
	"github.com/dividebyzero/claude-experiments/varvig/internal/refs"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/spec"
	"github.com/dividebyzero/claude-experiments/varvig/internal/worktree"
)

// commands maps a subcommand name to its handler.
var commands = map[string]func([]string) error{
	"init":        cmdInit,
	"whoami":      cmdWhoami,
	"key":         cmdKey,
	"trust":       cmdTrust,
	"hash-object": cmdHashObject,
	"cat-object":  cmdCatObject,
	"update-ref":  cmdUpdateRef,
	"promote":     cmdPromote,
	"show-ref":    cmdShowRef,
	"reflog":      cmdReflog,
	"write-tree":  cmdWriteTree,
	"commit":      cmdCommit,
	"checkout":    cmdCheckout,
	"log":         cmdLog,
	"verify":      cmdVerify,
	"git-export":  cmdGitExport,
	"git-import":  cmdGitImport,
	"serve":       cmdServe,
	"read":        cmdRead,
	"clone":       cmdClone,
	"fetch":       cmdFetch,
	"push":        cmdPush,
	"note":        cmdNote,
	"attest":      cmdAttest,
	"principal":   cmdPrincipal,
	"tickets":     cmdTickets,
	"bridge":      cmdBridge,
	"hook":        cmdHook,
	"affected":    cmdAffected,
	"merge":       cmdMerge,
	"spec":        cmdSpec,
	"gc":          cmdGc,
	"conform":     cmdConform,
	"task":        cmdTask,
	"mcp":         cmdMcp,
	"daemon":      cmdDaemon,
	"version":     cmdVersion,
}

func main() {
	cmd, args := resolveCommand()
	if cmd == "" || cmd == "help" || cmd == "-h" || cmd == "--help" {
		usage()
		if cmd == "" {
			os.Exit(2)
		}
		return
	}
	// --version / -v are the conventional flag spellings of the `version`
	// command; accept them at the top level so tooling that probes a binary
	// with `--version` (release smoke tests, §7) works without knowing the
	// subcommand form.
	if cmd == "--version" || cmd == "-v" {
		_ = cmdVersion(args)
		return
	}
	h, ok := commands[cmd]
	if !ok {
		usage()
		fmt.Fprintf(os.Stderr, "\nunknown command %q\n", cmd)
		os.Exit(2)
	}
	if err := h(args); err != nil {
		fmt.Fprintf(os.Stderr, "varvig %s: %v\n", cmd, err)
		os.Exit(1)
	}
}

// resolveCommand implements multicall dispatch: if the binary is invoked under
// a name like "varvig-commit" (or just "commit"), that is the command; otherwise
// the command is the first argument.
func resolveCommand() (string, []string) {
	base := filepath.Base(os.Args[0])
	base = strings.TrimSuffix(base, ".exe")
	if name, ok := strings.CutPrefix(base, "varvig-"); ok {
		if _, known := commands[name]; known {
			return name, os.Args[1:]
		}
	}
	if _, known := commands[base]; known {
		return base, os.Args[1:]
	}
	if len(os.Args) < 2 {
		return "", nil
	}
	return os.Args[1], os.Args[2:]
}

func usage() {
	fmt.Fprint(os.Stderr, `varvig — a source control system for agents

usage:
  varvig version | --version            print the version and platform
  varvig init [dir]                     initialize a repository; also writes the
                                        agent-rules files (VARVIG-AGENTS.md +
                                        an AGENTS.md pointer)
  varvig init --no-agent-rules [dir]    initialize without the agent-rules files
  varvig init --agent-rules [opts]      (re)write the agent-rules files
                                        --check  exit 2 if stale/missing (CI)
                                        --diff   print diffs; write nothing
                                        --print  print VARVIG-AGENTS.md
                                        --json   machine-readable result
  varvig whoami                         print the active principal and fingerprint
  varvig key init --name <name>         create a fallback key (only if no SSH key)
  varvig trust [list|check [scope]]     inspect .varvig.d/allowed_keys
  varvig hash-object [-w] <file|->      hash (and optionally store) a blob
  varvig cat-object <id>                print an object's content/summary
  varvig write-tree                     store the working tree, print tree id
  varvig commit -m <msg>                commit the working tree, advance HEAD
  varvig checkout <ref|id>              materialize a change/tree into the tree
  varvig log [ref|id]                   walk the change DAG from HEAD (or arg)
  varvig verify [ref|id]                check provenance and signatures on changes
  varvig update-ref <name> <new> [old]  atomically set a ref (CAS on old)
  varvig promote <ref> <new> [opts]     move a ref via a signed ref update
                                        (--scope S, --ttl SECONDS)
  varvig show-ref [name]                list refs or resolve one
  varvig reflog <name>                  print a ref's append-only log
  varvig git-export <dir> [branch]      export HEAD to a plain git repository
  varvig git-import <dir> [branch]      import a git branch into this repository
  varvig serve <addr>                   serve this repository to peers (e.g. :9418)
  varvig serve --read-only [--socket P] serve the read-only HTTP query API
              [--tcp ADDR]              (Unix socket by default; TCP opt-in)
  varvig read <object|tree|blob|change|log|refs|proposals> [args]
                                      read via the query layer, as JSON
  varvig clone <addr> <dir> [branch]    replicate a peer's branch into a new repo
  varvig fetch <addr> [branch]          fetch a peer's branch into refs/remotes/origin
  varvig push <addr> [branch]           push a local branch to a peer (CAS lease)
  varvig note add <target> [opts]       attach a note (--ns NS, -m MSG or -f FILE)
  varvig note list <target> [--ns NS]   list notes attached to an object
  varvig attest approve <ref|id>        sign an approval bound to an intent revision
              [--strength strong|delegated] [-m rationale]
  varvig attest veto <ref|id> [-m msg]  sign a veto (blocks all descendants)
  varvig attest list <ref|id>           list attestations on an intent revision
  varvig attest status <ref|id>         derived status (--require strong|delegated|weak)
  varvig attest policy set <m.wasm>     set the promotion-policy wasm module (§2.5)
  varvig attest policy show|clear       show or remove the promotion policy
  varvig principal add --name <n> --kind human|agent|bridge [--key <hex>]
                                      register a keyholder in the org chart (§1.4)
  varvig principal list|remove <fp>     list or remove principals
  varvig tickets new -m <spec>          mint a ticket: a ref + genesis revision (§1.2)
  varvig tickets revise <ticket> -m <spec>  append an intent revision, move the ref
  varvig tickets list                   list tickets with their derived status
  varvig tickets show <ticket>          spec, scope, status, blockers for one ticket
  varvig tickets spec <ticket>          print the raw spec verbatim (for tools)
  varvig tickets status <ticket>        print the derived status word (for tools)
  varvig tickets scope <ticket>         declare/show a ticket's read+write set
              [--reads a,b] [--writes c,d]  (what makes it schedulable, §3.1)
  varvig tickets blockers <ticket>      tickets blocking this one (derived, §3.2)
  varvig tickets graph                  the derived blocking graph over all scoped tickets
  varvig tickets rank [--weights f.json] rank scoped tickets by score (§3.3)
  varvig tickets backtest [-o f.json]   learn a scorer from recorded decisions,
              [--epochs N]              report agreement, optionally save weights
  varvig bridge link <ticket>           set/show a ticket's external-tracker link
              [--system S --foreign-id ID]  (opaque system tag; §5, for peers)
  varvig bridge needs-push|mark-pushed <ticket>   outbound echo-suppression state
  varvig bridge apply-inbound <ticket> -m <spec> [--author A]  apply a tracker edit
  varvig bridge transition <ticket> <approve|veto|request-change> [-m msg]
                                      record a weak workflow transition (§5.3)
  varvig hook set <event> <module.wasm> bind a wasm hook to an event
  varvig hook list                      list configured hooks
  varvig hook run <event> [file]        run an event's hooks with input (or stdin)
  varvig affected [<base> <new>]        show files changed and their dependents
  varvig merge <ref|id>                 three-way merge another change into HEAD
  varvig spec add <task> <ref|id>       record a speculation candidate
  varvig spec list <task>               list a task's candidates and scores
  varvig spec score <task> <id> <n>     set a candidate's score
  varvig spec promote <task> [ref]      promote the best candidate onto a ref
  varvig spec prune <task> <keepK>      retention: keep top-K, drop the rest
  varvig gc [--dry-run] [--report-external] [--prune-reflog <dur> [--keep N]]
                                      sweep unreachable objects; optionally
                                      expire reflogs older than <dur> first
  varvig conform [--emit|--id]          check this build against the frozen format
  varvig daemon [--socket PATH]         run the long-running local daemon: holds task
                                        credentials in memory, serves a per-task MCP socket
  varvig daemon status                  report a running daemon's pid, uptime, task count
  varvig daemon stop                    ask a running daemon to exit
  varvig task start [--scope S] [--ttl DUR] [--base REF] [dir]
                                      mint an ephemeral, scoped, propose-only task
                                      credential and a sparse checkout of its read set
                                      (minted in the daemon when one is running)
  varvig task list                      list the daemon's live tasks
  varvig task stop <id>                 revoke a task early
  varvig mcp [--scope S] [--ttl DUR] [--base REF]
                                      MCP gate over stdio for a scoped task; relays through
                                      the daemon when one is up, else a standalone gate
                                      (reads logged into provenance; propose, never promote)
  varvig mcp --connect <task.sock>      bridge stdio to a specific per-task socket
  varvig mcp --standalone [...]         force an in-process gate (ignore any daemon)
`)
}

// cmdInit lives in init.go — it grew a flag surface (agent rules) too large for
// main.go's dispatch table neighborhood.

func cmdHashObject(args []string) error {
	write := false
	var target string
	for _, a := range args {
		if a == "-w" {
			write = true
			continue
		}
		target = a
	}
	if target == "" {
		return errors.New("usage: varvig hash-object [-w] <file|->")
	}
	var data []byte
	var err error
	if target == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(target)
	}
	if err != nil {
		return err
	}
	blob := object.NewBlob(data)
	if write {
		r, err := repo.Open(".")
		if err != nil {
			return err
		}
		id, err := r.Objects.Put(blob)
		if err != nil {
			return err
		}
		fmt.Println(id.Hex())
		return nil
	}
	id, err := blob.ID(multihash.Default)
	if err != nil {
		return err
	}
	fmt.Println(id.Hex())
	return nil
}

func cmdCatObject(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: varvig cat-object <id>")
	}
	id, err := multihash.ParseHex(args[0])
	if err != nil {
		return err
	}
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	o, err := r.Objects.Get(id)
	if err != nil {
		return err
	}
	switch o.Type() {
	case object.TypeBlob:
		content, _ := o.BlobContent()
		_, err := os.Stdout.Write(content)
		return err
	case object.TypeTree:
		entries, err := o.TreeEntries()
		if err != nil {
			return err
		}
		for _, e := range entries {
			fmt.Printf("%06o %s %s\t%s\n", e.Mode, e.Kind, e.ID.Hex(), e.Name)
		}
		return nil
	case object.TypeChange:
		c, err := o.AsChange()
		if err != nil {
			return err
		}
		fmt.Printf("tree %s\n", c.Tree.Hex())
		for _, p := range c.Parents {
			fmt.Printf("parent %s\n", p.Hex())
		}
		if c.Provenance != nil {
			fmt.Printf("provenance %s\n", c.Provenance.Hex())
		}
		if _, ok := o.RawSignature(); ok {
			fmt.Printf("signed yes\n")
		}
		fmt.Printf("author %s\n", c.Author)
		fmt.Printf("timestamp %d\n", c.Timestamp)
		fmt.Printf("\n%s\n", c.Message)
		return nil
	case object.TypeProvenance:
		p, err := o.AsProvenance()
		if err != nil {
			return err
		}
		fmt.Printf("authority %s\n", p.Authority)
		fmt.Printf("model %s\n", p.Model)
		fmt.Printf("model-version %s\n", p.ModelVersion)
		fmt.Printf("sampling %s\n", p.Sampling)
		fmt.Printf("tool-permissions %s\n", strings.Join(p.ToolPermissions, ","))
		if p.ToolHash != nil {
			fmt.Printf("tool-hash %s\n", p.ToolHash.Hex())
		}
		if p.TaskSpec != "" {
			fmt.Printf("task %s\n", p.TaskSpec)
		}
		if p.ContextRead != "" {
			fmt.Printf("context %s\n", p.ContextRead)
		}
		if p.Reasoning != "" {
			fmt.Printf("reasoning %s\n", p.Reasoning)
		}
		return nil
	default:
		fmt.Printf("%s object\n", o.Type())
		return nil
	}
}

func cmdWriteTree(args []string) error {
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	id, err := worktree.WriteTree(r.Objects, r.Root())
	if err != nil {
		return err
	}
	fmt.Println(id.Hex())
	return nil
}

func cmdCommit(args []string) error {
	var msg string
	for i := 0; i < len(args); i++ {
		if args[i] == "-m" && i+1 < len(args) {
			msg = args[i+1]
			i++
		}
	}
	if msg == "" {
		return errors.New("usage: varvig commit -m <msg>")
	}
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	treeID, err := worktree.WriteTree(r.Objects, r.Root())
	if err != nil {
		return err
	}
	headRef, err := r.Head()
	if err != nil {
		return err
	}
	var parents []multihash.Multihash
	parent, err := r.Refs.Resolve(headRef)
	if err == nil {
		parents = append(parents, parent)
	} else if !errors.Is(err, refs.ErrNotExist) {
		return err
	}

	// pre-commit hooks run in a sandbox and can veto the commit (design §3.2).
	hookInput, _ := json.Marshal(map[string]string{
		"event": hook.EventPreCommit, "message": msg, "author": author(), "tree": treeID.Hex(),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	results, err := hook.Fire(ctx, r, hook.EventPreCommit, hookInput)
	if err != nil {
		return fmt.Errorf("pre-commit hook: %w", err)
	}
	for _, res := range results {
		if !res.Allowed() {
			return fmt.Errorf("commit vetoed by pre-commit hook (exit %d): %s",
				res.ExitCode, strings.TrimSpace(string(res.Stderr)))
		}
	}

	// Provenance and signing are required on native changes (design §2.1):
	// record who/what produced the change, then sign it.
	provID, err := r.Objects.Put(object.NewProvenance(provenance.Build(author())))
	if err != nil {
		return err
	}
	change := object.NewChange(object.Change{
		Tree:       treeID,
		Parents:    parents,
		Message:    msg,
		Timestamp:  time.Now().Unix(),
		Author:     author(),
		Provenance: provID,
	})
	priv, err := provenance.LoadOrCreateIdentity(r.GitDir())
	if err != nil {
		return err
	}
	if err := provenance.Sign(change, priv); err != nil {
		return err
	}
	id, err := r.Objects.Put(change)
	if err != nil {
		return err
	}
	if err := r.Refs.CompareAndSwap(headRef, parent, id, author(), "commit"); err != nil {
		return err
	}
	// post-commit hooks are informational; their verdict does not block.
	postInput, _ := json.Marshal(map[string]string{"event": hook.EventPostCommit, "change": id.Hex()})
	_, _ = hook.Fire(ctx, r, hook.EventPostCommit, postInput)

	pub := priv.Public().(ed25519.PublicKey)
	fmt.Printf("%s %s\n  signed-by %s…\n", id.Hex(), msg, hex.EncodeToString(pub[:8]))
	return nil
}

func cmdCheckout(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: varvig checkout <ref|id>")
	}
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	id, err := resolve(r, args[0])
	if err != nil {
		return err
	}
	o, err := r.Objects.Get(id)
	if err != nil {
		return err
	}
	treeID := id
	if o.Type() == object.TypeChange {
		c, err := o.AsChange()
		if err != nil {
			return err
		}
		if !c.Materialized() {
			// An unmaterialized change (a ticket) has no tree to lay down; this
			// is a specific, named failure, not an empty working tree (D1).
			return fmt.Errorf("%w: %s", object.ErrUnmaterialized, id.Hex())
		}
		treeID = c.Tree
	}
	if err := worktree.Checkout(r.Objects, treeID, r.Root()); err != nil {
		return err
	}
	fmt.Printf("checked out %s\n", treeID.Hex())
	return nil
}

func cmdLog(args []string) error {
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	var start multihash.Multihash
	if len(args) == 1 {
		start, err = resolve(r, args[0])
	} else {
		headRef, herr := r.Head()
		if herr != nil {
			return herr
		}
		start, err = r.Refs.Resolve(headRef)
	}
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	var walk func(id multihash.Multihash) error
	walk = func(id multihash.Multihash) error {
		if seen[id.Hex()] {
			return nil
		}
		seen[id.Hex()] = true
		o, err := r.Objects.Get(id)
		if err != nil {
			return err
		}
		c, err := o.AsChange()
		if err != nil {
			return err
		}
		fmt.Printf("change %s\n", id.Hex())
		fmt.Printf("  author %s\n", c.Author)
		fmt.Printf("  date   %s\n", time.Unix(c.Timestamp, 0).UTC().Format(time.RFC3339))
		fmt.Printf("  %s\n\n", strings.ReplaceAll(c.Message, "\n", "\n  "))
		for _, p := range c.Parents {
			if err := walk(p); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(start)
}

func cmdVerify(args []string) error {
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	var start multihash.Multihash
	if len(args) == 1 {
		start, err = resolve(r, args[0])
	} else {
		headRef, herr := r.Head()
		if herr != nil {
			return herr
		}
		start, err = r.Refs.Resolve(headRef)
	}
	if err != nil {
		return err
	}

	seen := map[string]bool{}
	failures := 0
	var walk func(id multihash.Multihash) error
	walk = func(id multihash.Multihash) error {
		if seen[id.Hex()] {
			return nil
		}
		seen[id.Hex()] = true
		o, err := r.Objects.Get(id)
		if err != nil {
			return err
		}
		c, err := o.AsChange()
		if err != nil {
			return err
		}
		short := id.Hex()[4:16]
		// Git-imported changes are foreign: they carry no Varvig provenance and
		// are reported as such rather than failed.
		if _, foreign := o.Field(gitport.GitCommitBody); foreign {
			fmt.Printf("~ %s  foreign (git-imported), unsigned\n", short)
		} else {
			switch pub, verr := provenance.Verify(o); {
			case verr != nil:
				fmt.Printf("✗ %s  %v\n", short, verr)
				failures++
			case c.Provenance == nil:
				fmt.Printf("✗ %s  %v\n", short, provenance.ErrNoProvenance)
				failures++
			default:
				summary := ""
				if prov, err := r.Objects.Get(c.Provenance); err == nil {
					if p, err := prov.AsProvenance(); err == nil {
						summary = provenanceSummary(p)
					}
				}
				fmt.Printf("✓ %s  signed-by %s… %s\n", short, hex.EncodeToString(pub[:8]), summary)
			}
		}
		for _, p := range c.Parents {
			if err := walk(p); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(start); err != nil {
		return err
	}
	if failures > 0 {
		return fmt.Errorf("%d change(s) failed verification", failures)
	}
	return nil
}

func provenanceSummary(p object.Provenance) string {
	parts := []string{}
	if p.Authority != "" {
		parts = append(parts, "authority="+p.Authority)
	}
	if p.Model != "" {
		m := p.Model
		if p.ModelVersion != "" {
			m += "@" + p.ModelVersion
		}
		parts = append(parts, "model="+m)
	}
	if p.ToolHash != nil {
		parts = append(parts, "tool="+p.ToolHash.Hex()[4:16])
	}
	return strings.Join(parts, " ")
}

func cmdUpdateRef(args []string) error {
	if len(args) < 2 || len(args) > 3 {
		return errors.New("usage: varvig update-ref <name> <new> [old]")
	}
	name := args[0]
	newVal, err := parseValueOrZero(args[1])
	if err != nil {
		return fmt.Errorf("new value: %w", err)
	}
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	var oldVal multihash.Multihash
	if len(args) == 3 {
		oldVal, err = parseValueOrZero(args[2])
		if err != nil {
			return fmt.Errorf("old value: %w", err)
		}
	} else {
		cur, rerr := r.Refs.Resolve(name)
		if rerr != nil && !errors.Is(rerr, refs.ErrNotExist) {
			return rerr
		}
		oldVal = cur
	}
	// Ref updates are a trigger point: run ref-update hooks as a policy gate.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := hook.EvaluateRefUpdate(ctx, r, name, oldVal, newVal); err != nil {
		return err
	}
	if err := r.Refs.CompareAndSwap(name, oldVal, newVal, author(), "update-ref"); err != nil {
		return err
	}
	_ = hook.NotifyRefUpdate(ctx, r, name, oldVal, newVal)
	return nil
}

func cmdShowRef(args []string) error {
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	if len(args) == 1 {
		id, err := r.Refs.Resolve(args[0])
		if err != nil {
			return err
		}
		fmt.Printf("%s %s\n", id.Hex(), args[0])
		return nil
	}
	names, err := r.Refs.List()
	if err != nil {
		return err
	}
	for _, n := range names {
		id, err := r.Refs.Resolve(n)
		if err != nil {
			continue
		}
		fmt.Printf("%s %s\n", id.Hex(), n)
	}
	return nil
}

func cmdReflog(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: varvig reflog <name>")
	}
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	entries, err := r.Refs.ReadLog(args[0])
	if err != nil {
		return err
	}
	for _, e := range entries {
		fmt.Printf("%s -> %s  %s  %s\t%s\n",
			hexOrDash(e.Old), hexOrDash(e.New),
			time.Unix(0, e.UnixNS).UTC().Format(time.RFC3339Nano),
			e.Actor, e.Message)
	}
	return nil
}

func cmdGitExport(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: varvig git-export <dir> [branch]")
	}
	branch := "main"
	if len(args) > 1 {
		branch = args[1]
	}
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	headRef, err := r.Head()
	if err != nil {
		return err
	}
	head, err := r.Refs.Resolve(headRef)
	if err != nil {
		return err
	}
	gitDir := filepath.Join(args[0], ".git")
	oid, err := gitport.Export(r, gitDir, branch, head)
	if err != nil {
		return err
	}
	fmt.Printf("exported %s to %s (branch %s)\n", oid.Hex(), args[0], branch)
	return nil
}

func cmdGitImport(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: varvig git-import <dir> [branch]")
	}
	branch := "main"
	if len(args) > 1 {
		branch = args[1]
	}
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	gitDir := filepath.Join(args[0], ".git")
	id, err := gitport.Import(r, gitDir, branch)
	if err != nil {
		return err
	}
	fmt.Printf("imported branch %s as %s\n", branch, id.Hex())
	return nil
}

func cmdServe(args []string) error {
	readOnly := false
	socket, tcp, addr := "", "", ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--read-only":
			readOnly = true
		case "--socket":
			if i+1 >= len(args) {
				return errors.New("serve: --socket requires a path")
			}
			socket, i = args[i+1], i+1
		case "--tcp":
			if i+1 >= len(args) {
				return errors.New("serve: --tcp requires an address")
			}
			tcp, i = args[i+1], i+1
		default:
			addr = args[i]
		}
	}
	r, err := repo.Open(".")
	if err != nil {
		return err
	}

	if readOnly {
		return serveReadOnly(r, socket, tcp)
	}

	if addr == "" {
		return errors.New("usage: varvig serve <addr>  |  varvig serve --read-only [--socket PATH | --tcp ADDR]")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer ln.Close()
	fmt.Printf("serving %s on %s\n", r.Root(), ln.Addr())
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer conn.Close()
			if err := p2p.Serve(r, conn); err != nil && !errors.Is(err, io.EOF) {
				fmt.Fprintf(os.Stderr, "varvig serve: peer %s: %v\n", conn.RemoteAddr(), err)
			}
		}()
	}
}

// serveReadOnly runs the read-only HTTP query API. It defaults to a Unix socket
// (whose 0600 mode is the authentication, auth design §7.4); TCP is an explicit
// opt-in that binds loopback only, since it crosses a trust boundary a socket
// does not (§7.4, §7.5).
func serveReadOnly(r *repo.Repo, socket, tcp string) error {
	q := readapi.New(r)
	var ln net.Listener
	var err error
	switch {
	case tcp != "":
		ln, err = net.Listen("tcp", tcp)
	default:
		if socket == "" {
			socket = filepath.Join(r.GitDir(), "read.sock")
		}
		ln, err = readapi.ListenUnix(socket)
		if err == nil {
			// SO_PEERCRED: back the 0600 socket with a kernel-attested uid check
			// (auth design §7.4). On platforms without it, the mode is the guard.
			uid := os.Getuid()
			ln = peercred.FilterListener(ln, uid, func(c peercred.Cred) {
				fmt.Fprintf(os.Stderr, "varvig serve: rejected read connection from uid %d (allow %d)\n", c.UID, uid)
			})
		}
	}
	if err != nil {
		return err
	}
	defer ln.Close()
	fmt.Printf("serving read-only API for %s on %s\n", r.Root(), ln.Addr())
	return readapi.Serve(q, ln)
}

func cmdClone(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: varvig clone <addr> <dir> [branch]")
	}
	addr, dir := args[0], args[1]
	branch := "main"
	if len(args) > 2 {
		branch = args[2]
	}
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	client, err := p2p.Dial(conn)
	if err != nil {
		return err
	}
	tip, err := refTip(client, "refs/heads/"+branch)
	if err != nil {
		return err
	}
	r, err := repo.Init(dir)
	if err != nil {
		return err
	}
	if err := client.Fetch(r.Objects, []multihash.Multihash{tip}, nil); err != nil {
		return err
	}
	if err := r.Refs.Create("refs/heads/"+branch, tip, "clone", "clone "+addr); err != nil {
		return err
	}
	// Record where the remote was, so a later push can lease against it.
	if err := r.Refs.Create("refs/remotes/origin/"+branch, tip, "clone", "clone "+addr); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(r.GitDir(), "HEAD"), []byte("ref: refs/heads/"+branch+"\n"), 0o644); err != nil {
		return err
	}
	if err := checkoutChange(r, tip); err != nil {
		return err
	}
	// Notes replicate by default (federation §4): pull the peer's notes so
	// evidence and governance state travel with the branch, not only the code.
	if err := syncNotes(client, r, false); err != nil {
		return err
	}
	fmt.Printf("cloned %s (branch %s) into %s at %s\n", addr, branch, dir, tip.Hex())
	return nil
}

func cmdFetch(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: varvig fetch <addr> [branch]")
	}
	branch := "main"
	if len(args) > 1 {
		branch = args[1]
	}
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	conn, err := net.Dial("tcp", args[0])
	if err != nil {
		return err
	}
	defer conn.Close()
	client, err := p2p.Dial(conn)
	if err != nil {
		return err
	}
	tip, err := refTip(client, "refs/heads/"+branch)
	if err != nil {
		return err
	}
	tracking := "refs/remotes/origin/" + branch
	var have []multihash.Multihash
	if cur, err := r.Refs.Resolve(tracking); err == nil {
		have = append(have, cur)
	}
	if err := client.Fetch(r.Objects, []multihash.Multihash{tip}, have); err != nil {
		return err
	}
	cur, _ := r.Refs.Resolve(tracking)
	if err := r.Refs.CompareAndSwap(tracking, cur, tip, "fetch", "fetch "+args[0]); err != nil {
		return err
	}
	if err := syncNotes(client, r, false); err != nil {
		return err
	}
	fmt.Printf("fetched %s into %s\n", tip.Hex(), tracking)
	return nil
}

func cmdPush(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: varvig push <addr> [branch]")
	}
	branch := "main"
	if len(args) > 1 {
		branch = args[1]
	}
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	name := "refs/heads/" + branch
	local, err := r.Refs.Resolve(name)
	if err != nil {
		return fmt.Errorf("nothing to push: %w", err)
	}
	// The lease is what we last observed the remote to be — the remote-tracking
	// ref — NOT a fresh query. If the peer has moved since, the CAS is rejected
	// rather than silently overwriting the peer's work (force-with-lease, §2).
	// Enforcing that the new tip descends from the lease is left to the merge
	// step; for now the lease alone guards against unseen concurrent changes.
	tracking := "refs/remotes/origin/" + branch
	var old multihash.Multihash
	if cur, err := r.Refs.Resolve(tracking); err == nil {
		old = cur
	}
	conn, err := net.Dial("tcp", args[0])
	if err != nil {
		return err
	}
	defer conn.Close()
	client, err := p2p.Dial(conn)
	if err != nil {
		return err
	}
	if err := client.Push(r.Objects, name, old, local); err != nil {
		return err
	}
	// Advance our record of the remote to what we just pushed.
	prev, _ := r.Refs.Resolve(tracking)
	_ = r.Refs.CompareAndSwap(tracking, prev, local, "push", "update tracking after push")
	// Notes replicate by default (federation §4): push our notes alongside.
	if err := syncNotes(client, r, true); err != nil {
		return err
	}
	fmt.Printf("pushed %s to %s (%s)\n", local.Hex(), args[0], name)
	return nil
}

// refTip runs ListRefs and returns the id advertised for name.
func refTip(client *p2p.Client, name string) (multihash.Multihash, error) {
	refsList, err := client.ListRefs()
	if err != nil {
		return nil, err
	}
	for _, ref := range refsList {
		if ref.Name == name {
			return multihash.Multihash(ref.ID), nil
		}
	}
	return nil, fmt.Errorf("peer has no %s", name)
}

func checkoutChange(r *repo.Repo, id multihash.Multihash) error {
	o, err := r.Objects.Get(id)
	if err != nil {
		return err
	}
	treeID := id
	if o.Type() == object.TypeChange {
		c, err := o.AsChange()
		if err != nil {
			return err
		}
		if !c.Materialized() {
			return fmt.Errorf("%w: %s", object.ErrUnmaterialized, id.Hex())
		}
		treeID = c.Tree
	}
	return worktree.Checkout(r.Objects, treeID, r.Root())
}

func cmdNote(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: varvig note <add|list> <target> [opts]")
	}
	sub, rest := args[0], args[1:]
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	store := notes.New(r)
	switch sub {
	case "add":
		target, err := resolve(r, rest[0])
		if err != nil {
			return err
		}
		ns := "default"
		var payload []byte
		for i := 1; i < len(rest); i++ {
			switch rest[i] {
			case "--ns":
				if i+1 < len(rest) {
					ns = rest[i+1]
					i++
				}
			case "-m":
				if i+1 < len(rest) {
					payload = []byte(rest[i+1])
					i++
				}
			case "-f":
				if i+1 < len(rest) {
					payload, err = os.ReadFile(rest[i+1])
					if err != nil {
						return err
					}
					i++
				}
			}
		}
		if !r.Objects.Has(target) {
			return fmt.Errorf("no such object %s", target.Hex())
		}
		id, err := store.Add(ns, target, payload, author(), time.Now().Unix())
		if err != nil {
			return err
		}
		fmt.Printf("note %s attached to %s (%s)\n", id.Hex(), target.Hex(), ns)
		return nil
	case "list":
		target, err := resolve(r, rest[0])
		if err != nil {
			return err
		}
		ns := ""
		for i := 1; i < len(rest); i++ {
			if rest[i] == "--ns" && i+1 < len(rest) {
				ns = rest[i+1]
				i++
			}
		}
		var namespaces []string
		if ns != "" {
			namespaces = []string{ns}
		} else {
			namespaces, err = store.Namespaces(target)
			if err != nil {
				return err
			}
		}
		for _, n := range namespaces {
			entries, err := store.List(n, target)
			if err != nil {
				return err
			}
			for _, e := range entries {
				fmt.Printf("[%s] %s by %s at %s\n  %s\n", n, e.ID.Hex()[4:16], e.Note.Author,
					time.Unix(e.Note.Timestamp, 0).UTC().Format(time.RFC3339), string(e.Note.Payload))
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown note subcommand %q", sub)
	}
}

func cmdHook(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: varvig hook <set|list|run> ...")
	}
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	switch args[0] {
	case "set":
		if len(args) != 3 {
			return errors.New("usage: varvig hook set <event> <module.wasm>")
		}
		wasmModule, err := os.ReadFile(args[2])
		if err != nil {
			return err
		}
		id, err := hook.SetHook(r, args[1], wasmModule, author())
		if err != nil {
			return err
		}
		fmt.Printf("hook %s -> %s\n", args[1], id.Hex())
		return nil
	case "list":
		cfg, err := hook.LoadManifest(r)
		if err != nil {
			return err
		}
		for _, e := range cfg.Entries {
			fmt.Printf("%s\t%s\n", e.Event, e.Module.Hex())
		}
		return nil
	case "run":
		if len(args) < 2 {
			return errors.New("usage: varvig hook run <event> [inputfile]")
		}
		var input []byte
		if len(args) > 2 {
			input, err = os.ReadFile(args[2])
		} else {
			input, err = io.ReadAll(os.Stdin)
		}
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		results, err := hook.Fire(ctx, r, args[1], input)
		if err != nil {
			return err
		}
		if len(results) == 0 {
			fmt.Printf("no hooks bound to %q\n", args[1])
		}
		for i, res := range results {
			fmt.Printf("hook %d: exit=%d\n", i, res.ExitCode)
			if len(res.Stdout) > 0 {
				fmt.Printf("  stdout: %s\n", strings.TrimRight(string(res.Stdout), "\n"))
			}
			if len(res.Stderr) > 0 {
				fmt.Printf("  stderr: %s\n", strings.TrimRight(string(res.Stderr), "\n"))
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown hook subcommand %q", args[0])
	}
}

func cmdAffected(args []string) error {
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	var baseTree, newTree multihash.Multihash
	switch len(args) {
	case 0:
		// Default: what the tip change affected relative to its first parent.
		headRef, herr := r.Head()
		if herr != nil {
			return herr
		}
		head, herr := r.Refs.Resolve(headRef)
		if herr != nil {
			return herr
		}
		newTree, err = treeOf(r, head)
		if err != nil {
			return err
		}
		obj, err := r.Objects.Get(head)
		if err != nil {
			return err
		}
		if c, err := obj.AsChange(); err == nil && len(c.Parents) > 0 {
			if baseTree, err = treeOf(r, c.Parents[0]); err != nil {
				return err
			}
		}
	case 2:
		base, err := resolve(r, args[0])
		if err != nil {
			return err
		}
		newRev, err := resolve(r, args[1])
		if err != nil {
			return err
		}
		if baseTree, err = treeOf(r, base); err != nil {
			return err
		}
		if newTree, err = treeOf(r, newRev); err != nil {
			return err
		}
	default:
		return errors.New("usage: varvig affected [<base> <new>]")
	}

	diff, err := affected.DiffTrees(r.Objects, baseTree, newTree)
	if err != nil {
		return err
	}
	changed := diff.Changed()

	cache, err := affected.NewDiskCache(filepath.Join(r.GitDir(), "index", "deps"))
	if err != nil {
		return err
	}
	wasm, err := wasmAnalyzers(r)
	if err != nil {
		return err
	}
	graph, err := affected.BuildGraph(r.Objects, newTree, affected.Options{Cache: cache, Wasm: wasm})
	if err != nil {
		return err
	}
	impacted := graph.Affected(changed)

	fmt.Printf("changed (%d):\n", len(changed))
	for _, p := range changed {
		fmt.Printf("  %s\n", p)
	}
	fmt.Printf("affected (%d):\n", len(impacted))
	for _, p := range impacted {
		marker := " "
		if !contains(changed, p) {
			marker = "+" // pulled in transitively via the dependency graph
		}
		fmt.Printf("  %s %s\n", marker, p)
	}
	return nil
}

func cmdMerge(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: varvig merge <ref|id>")
	}
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	headRef, err := r.Head()
	if err != nil {
		return err
	}
	ours, err := r.Refs.Resolve(headRef)
	if err != nil {
		return fmt.Errorf("no HEAD to merge into: %w", err)
	}
	theirs, err := resolve(r, args[0])
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// No live model in this environment: textual merge only (regenerator nil).
	// The regeneration path (§1.2) is exercised via the library interface.
	res, err := merge.Merge(ctx, r.Objects, ours, theirs, nil)
	if err != nil {
		return err
	}

	// Materialize the merged tree so results (and any conflict markers) are
	// visible in the working tree.
	if err := worktree.Checkout(r.Objects, res.Tree, r.Root()); err != nil {
		return err
	}

	fmt.Printf("merge base %s\n", shortOrNone(res.Base))
	fmt.Printf("  clean:%d  text-merged:%d  regenerated:%d  conflicts:%d\n",
		len(res.Clean), len(res.TextMerged), len(res.Regenerated), len(res.Conflicts))
	for _, c := range res.Conflicts {
		fmt.Printf("  CONFLICT %s (%s)\n", c.Path, c.Reason)
	}
	if !res.Resolved() {
		return fmt.Errorf("merge left %d unresolved conflict(s); markers written to the working tree", len(res.Conflicts))
	}
	if err := r.Refs.CompareAndSwap(headRef, ours, res.Change, author(), "merge"); err != nil {
		return err
	}
	fmt.Printf("merged %s into %s -> %s\n", theirs.Hex()[4:16], headRef, res.Change.Hex())
	return nil
}

func cmdSpec(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: varvig spec <add|list|score|promote|prune> <task> ...")
	}
	sub, task := args[0], args[1]
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	pool := spec.Open(r.GitDir())
	switch sub {
	case "add":
		if len(args) != 3 {
			return errors.New("usage: varvig spec add <task> <ref|id>")
		}
		id, err := resolve(r, args[2])
		if err != nil {
			return err
		}
		if err := pool.Add(task, id, time.Now().Unix()); err != nil {
			return err
		}
		fmt.Printf("added candidate %s to task %s\n", id.Hex()[4:16], task)
		return nil
	case "list":
		entries, err := pool.List(task)
		if err != nil {
			return err
		}
		for _, e := range entries {
			score := "unscored"
			if e.Scored {
				score = fmt.Sprintf("%.4g", e.Score)
			}
			fmt.Printf("%s  %s\n", e.Change.Hex()[4:16], score)
		}
		return nil
	case "score":
		if len(args) != 4 {
			return errors.New("usage: varvig spec score <task> <id> <value>")
		}
		id, err := multihash.ParseHex(args[2])
		if err != nil {
			return err
		}
		v, err := strconv.ParseFloat(args[3], 64)
		if err != nil {
			return err
		}
		return pool.SetScore(task, id, v)
	case "promote":
		ref := ""
		if len(args) == 3 {
			ref = args[2]
		} else {
			ref, err = r.Head()
			if err != nil {
				return err
			}
		}
		// The promotion checkpoint is on by default (tickets §4, M1): the veto
		// gate is always applied, plus the repository's policy wasm module if
		// one is configured (refs/varvig/policy, §2.5). Constraints stack — any
		// one refusing is decisive (§3.3).
		policies := []attest.Policy{attest.VetoGate{}}
		if wp, ok, perr := attest.LoadPolicy(r); perr != nil {
			return perr
		} else if ok {
			policies = append(policies, wp)
		}
		id, err := spec.PromoteWithPolicy(pool, r, task, ref, author(), attest.AllOf(policies...))
		if err != nil {
			return err
		}
		fmt.Printf("promoted %s onto %s\n", id.Hex()[4:16], ref)
		return nil
	case "prune":
		if len(args) != 3 {
			return errors.New("usage: varvig spec prune <task> <keepK>")
		}
		k, err := strconv.Atoi(args[2])
		if err != nil {
			return err
		}
		removed, err := pool.Prune(task, k)
		if err != nil {
			return err
		}
		fmt.Printf("pruned %d candidate(s) from task %s\n", len(removed), task)
		return nil
	default:
		return fmt.Errorf("unknown spec subcommand %q", sub)
	}
}

func cmdGc(args []string) error {
	dryRun := false
	reportExternal := false
	pruneReflog := ""
	keep := 1 // by default, expiry always retains each ref's most recent move
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dry-run", "-n":
			dryRun = true
		case "--report-external":
			reportExternal = true
		case "--prune-reflog":
			if i+1 < len(args) {
				pruneReflog = args[i+1]
				i++
			}
		case "--keep":
			if i+1 < len(args) {
				k, err := strconv.Atoi(args[i+1])
				if err != nil {
					return err
				}
				keep = k
				i++
			}
		}
	}
	r, err := repo.Open(".")
	if err != nil {
		return err
	}

	// Reflog expiry is opt-in and destructive to undo beyond the retained
	// window; it is what lets GC reclaim objects pinned only by old reflog
	// entries (design §1.5 vs §2). Without it, GC preserves universal undo.
	if pruneReflog != "" {
		dur, err := time.ParseDuration(pruneReflog)
		if err != nil {
			return fmt.Errorf("bad --prune-reflog duration: %w", err)
		}
		cutoff := time.Now().Add(-dur).UnixNano()
		removed, err := r.Refs.ExpireAll(keep, cutoff)
		if err != nil {
			return err
		}
		fmt.Printf("reflog: expired %d entr%s older than %s (kept last %d each)\n",
			removed, plural(removed), pruneReflog, keep)
	}

	rep, err := gc.Collect(r, spec.Open(r.GitDir()), dryRun)
	if err != nil {
		return err
	}
	verb := "deleted"
	if dryRun {
		verb = "would delete"
	}
	fmt.Printf("roots:%d scanned:%d kept:%d %s:%d\n", rep.Roots, rep.Scanned, rep.Kept, verb, rep.Deleted)

	// --report-external surfaces external artifacts whose last reachable
	// referent went away this pass (federation §1.3). varvig only reports;
	// deleting the bytes from a registry is the operator's call.
	if reportExternal {
		fmt.Printf("external-unreachable:%d\n", len(rep.ExternalUnreachable))
		for _, a := range rep.ExternalUnreachable {
			locs := strings.Join(a.Locators, " ")
			fmt.Printf("  %s\t%s\t%s\n", a.ContentHash.Hex(), a.MediaType, locs)
		}
	}
	return nil
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

func cmdConform(args []string) error {
	// The conformance suite is about the binary and the frozen format, not any
	// repository, so this command opens nothing.
	for _, a := range args {
		switch a {
		case "--emit":
			b, err := conformance.CanonicalJSON(conformance.Build())
			if err != nil {
				return err
			}
			os.Stdout.Write(b)
			return nil
		case "--id":
			b, err := conformance.CanonicalJSON(conformance.Build())
			if err != nil {
				return err
			}
			fmt.Println(conformance.SuiteID(b).Hex())
			return nil
		}
	}
	fails := conformance.Verify(conformance.Golden())
	if len(fails) > 0 {
		for _, f := range fails {
			fmt.Fprintf(os.Stderr, "  FAIL %s\n", f)
		}
		return fmt.Errorf("conformance: %d failure(s) against suite %s", len(fails), conformance.GoldenSuiteID)
	}
	g := conformance.Golden()
	n := len(g.Generated.Objects) + len(g.Generated.Multihash) + len(g.Generated.Wire) + len(g.RoundTrip)
	fmt.Printf("conformant: %d vectors pass suite %s\n", n, conformance.GoldenSuiteID)
	return nil
}

func shortOrNone(m multihash.Multihash) string {
	if m == nil {
		return "(none)"
	}
	return m.Hex()[4:16]
}

// wasmAnalyzers builds language analyzers from hook-manifest entries named
// "analyze:<ext>" (design §3.3). Each binds a file extension to a wasm module
// that runs in the same sandbox as hooks.
func wasmAnalyzers(r *repo.Repo) ([]affected.WasmAnalyzer, error) {
	cfg, err := hook.LoadManifest(r)
	if err != nil {
		return nil, err
	}
	runner := func(ctx context.Context, module, input []byte) ([]byte, error) {
		res, err := hook.Run(ctx, module, input)
		if err != nil {
			return nil, err
		}
		if !res.Allowed() {
			return nil, fmt.Errorf("analyzer exited %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
		}
		return res.Stdout, nil
	}
	var out []affected.WasmAnalyzer
	for _, e := range cfg.Entries {
		ext, ok := strings.CutPrefix(e.Event, "analyze:")
		if !ok {
			continue
		}
		obj, err := r.Objects.Get(e.Module)
		if err != nil {
			return nil, err
		}
		mod, _ := obj.BlobContent()
		out = append(out, affected.WasmAnalyzer{Ext: ext, Module: mod, ID: e.Module, Run: runner})
	}
	return out, nil
}

func treeOf(r *repo.Repo, id multihash.Multihash) (multihash.Multihash, error) {
	obj, err := r.Objects.Get(id)
	if err != nil {
		return nil, err
	}
	if obj.Type() == object.TypeChange {
		c, err := obj.AsChange()
		if err != nil {
			return nil, err
		}
		return c.Tree, nil
	}
	return id, nil
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// resolve interprets a string as a ref name or, failing that, an object id.
func resolve(r *repo.Repo, s string) (multihash.Multihash, error) {
	if id, err := r.Refs.Resolve(s); err == nil {
		return id, nil
	}
	if id, err := r.Refs.Resolve("refs/heads/" + s); err == nil {
		return id, nil
	}
	return multihash.ParseHex(s)
}

func parseValueOrZero(s string) (multihash.Multihash, error) {
	if s == "0" || s == "" {
		return nil, nil
	}
	return multihash.ParseHex(s)
}

func author() string {
	for _, k := range []string{"VARVIG_AUTHOR", "VARVIG_ACTOR", "USER"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return "unknown"
}

func hexOrDash(m multihash.Multihash) string {
	if m == nil {
		return "-"
	}
	return m.Hex()
}
