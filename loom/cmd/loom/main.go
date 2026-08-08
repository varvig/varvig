// Command loom is the single, portable Loom binary. Per design §3.1 it is a
// busybox-style multicall executable: one artifact is client, server, and
// tooling. It dispatches on either the first argument or, when invoked under a
// command-specific name (e.g. a "loom-commit" symlink), on argv[0].
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

	"github.com/dividebyzero/claude-experiments/loom/internal/affected"
	"github.com/dividebyzero/claude-experiments/loom/internal/conformance"
	"github.com/dividebyzero/claude-experiments/loom/internal/gc"
	"github.com/dividebyzero/claude-experiments/loom/internal/gitport"
	"github.com/dividebyzero/claude-experiments/loom/internal/hook"
	"github.com/dividebyzero/claude-experiments/loom/internal/merge"
	"github.com/dividebyzero/claude-experiments/loom/internal/multihash"
	"github.com/dividebyzero/claude-experiments/loom/internal/notes"
	"github.com/dividebyzero/claude-experiments/loom/internal/object"
	"github.com/dividebyzero/claude-experiments/loom/internal/p2p"
	"github.com/dividebyzero/claude-experiments/loom/internal/provenance"
	"github.com/dividebyzero/claude-experiments/loom/internal/refs"
	"github.com/dividebyzero/claude-experiments/loom/internal/repo"
	"github.com/dividebyzero/claude-experiments/loom/internal/spec"
	"github.com/dividebyzero/claude-experiments/loom/internal/worktree"
)

// commands maps a subcommand name to its handler.
var commands = map[string]func([]string) error{
	"init":        cmdInit,
	"hash-object": cmdHashObject,
	"cat-object":  cmdCatObject,
	"update-ref":  cmdUpdateRef,
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
	"clone":       cmdClone,
	"fetch":       cmdFetch,
	"push":        cmdPush,
	"note":        cmdNote,
	"hook":        cmdHook,
	"affected":    cmdAffected,
	"merge":       cmdMerge,
	"spec":        cmdSpec,
	"gc":          cmdGc,
	"conform":     cmdConform,
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
	h, ok := commands[cmd]
	if !ok {
		usage()
		fmt.Fprintf(os.Stderr, "\nunknown command %q\n", cmd)
		os.Exit(2)
	}
	if err := h(args); err != nil {
		fmt.Fprintf(os.Stderr, "loom %s: %v\n", cmd, err)
		os.Exit(1)
	}
}

// resolveCommand implements multicall dispatch: if the binary is invoked under
// a name like "loom-commit" (or just "commit"), that is the command; otherwise
// the command is the first argument.
func resolveCommand() (string, []string) {
	base := filepath.Base(os.Args[0])
	base = strings.TrimSuffix(base, ".exe")
	if name, ok := strings.CutPrefix(base, "loom-"); ok {
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
	fmt.Fprint(os.Stderr, `loom — a source control system for agents

usage:
  loom init [dir]                     initialize a repository
  loom hash-object [-w] <file|->      hash (and optionally store) a blob
  loom cat-object <id>                print an object's content/summary
  loom write-tree                     store the working tree, print tree id
  loom commit -m <msg>                commit the working tree, advance HEAD
  loom checkout <ref|id>              materialize a change/tree into the tree
  loom log [ref|id]                   walk the change DAG from HEAD (or arg)
  loom verify [ref|id]                check provenance and signatures on changes
  loom update-ref <name> <new> [old]  atomically set a ref (CAS on old)
  loom show-ref [name]                list refs or resolve one
  loom reflog <name>                  print a ref's append-only log
  loom git-export <dir> [branch]      export HEAD to a plain git repository
  loom git-import <dir> [branch]      import a git branch into this repository
  loom serve <addr>                   serve this repository to peers (e.g. :9418)
  loom clone <addr> <dir> [branch]    replicate a peer's branch into a new repo
  loom fetch <addr> [branch]          fetch a peer's branch into refs/remotes/origin
  loom push <addr> [branch]           push a local branch to a peer (CAS lease)
  loom note add <target> [opts]       attach a note (--ns NS, -m MSG or -f FILE)
  loom note list <target> [--ns NS]   list notes attached to an object
  loom hook set <event> <module.wasm> bind a wasm hook to an event
  loom hook list                      list configured hooks
  loom hook run <event> [file]        run an event's hooks with input (or stdin)
  loom affected [<base> <new>]        show files changed and their dependents
  loom merge <ref|id>                 three-way merge another change into HEAD
  loom spec add <task> <ref|id>       record a speculation candidate
  loom spec list <task>               list a task's candidates and scores
  loom spec score <task> <id> <n>     set a candidate's score
  loom spec promote <task> [ref]      promote the best candidate onto a ref
  loom spec prune <task> <keepK>      retention: keep top-K, drop the rest
  loom gc [--dry-run] [--prune-reflog <dur> [--keep N]]
                                      sweep unreachable objects; optionally
                                      expire reflogs older than <dur> first
  loom conform [--emit|--id]          check this build against the frozen format
`)
}

func cmdInit(args []string) error {
	dir := "."
	if len(args) > 0 {
		dir = args[0]
	}
	r, err := repo.Init(dir)
	if err != nil {
		return err
	}
	fmt.Printf("initialized empty loom repository in %s\n", r.GitDir())
	return nil
}

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
		return errors.New("usage: loom hash-object [-w] <file|->")
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
		return errors.New("usage: loom cat-object <id>")
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
		return errors.New("usage: loom commit -m <msg>")
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
		return errors.New("usage: loom checkout <ref|id>")
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
		// Git-imported changes are foreign: they carry no Loom provenance and
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
		return errors.New("usage: loom update-ref <name> <new> [old]")
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
		return errors.New("usage: loom reflog <name>")
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
		return errors.New("usage: loom git-export <dir> [branch]")
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
		return errors.New("usage: loom git-import <dir> [branch]")
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
	if len(args) != 1 {
		return errors.New("usage: loom serve <addr>")
	}
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", args[0])
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
				fmt.Fprintf(os.Stderr, "loom serve: peer %s: %v\n", conn.RemoteAddr(), err)
			}
		}()
	}
}

func cmdClone(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: loom clone <addr> <dir> [branch]")
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
	fmt.Printf("cloned %s (branch %s) into %s at %s\n", addr, branch, dir, tip.Hex())
	return nil
}

func cmdFetch(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: loom fetch <addr> [branch]")
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
	fmt.Printf("fetched %s into %s\n", tip.Hex(), tracking)
	return nil
}

func cmdPush(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: loom push <addr> [branch]")
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
		treeID = c.Tree
	}
	return worktree.Checkout(r.Objects, treeID, r.Root())
}

func cmdNote(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: loom note <add|list> <target> [opts]")
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
		return errors.New("usage: loom hook <set|list|run> ...")
	}
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	switch args[0] {
	case "set":
		if len(args) != 3 {
			return errors.New("usage: loom hook set <event> <module.wasm>")
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
			return errors.New("usage: loom hook run <event> [inputfile]")
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
		return errors.New("usage: loom affected [<base> <new>]")
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
	graph, err := affected.BuildGraph(r.Objects, newTree, cache)
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
		return errors.New("usage: loom merge <ref|id>")
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
		return errors.New("usage: loom spec <add|list|score|promote|prune> <task> ...")
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
			return errors.New("usage: loom spec add <task> <ref|id>")
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
			return errors.New("usage: loom spec score <task> <id> <value>")
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
		id, err := spec.Promote(pool, r, task, ref, author())
		if err != nil {
			return err
		}
		fmt.Printf("promoted %s onto %s\n", id.Hex()[4:16], ref)
		return nil
	case "prune":
		if len(args) != 3 {
			return errors.New("usage: loom spec prune <task> <keepK>")
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
	pruneReflog := ""
	keep := 1 // by default, expiry always retains each ref's most recent move
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dry-run", "-n":
			dryRun = true
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
	for _, k := range []string{"LOOM_AUTHOR", "LOOM_ACTOR", "USER"} {
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
