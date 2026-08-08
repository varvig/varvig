// Command loom is the single, portable Loom binary. Per design §3.1 it is a
// busybox-style multicall executable: one artifact is client, server, and
// tooling. It dispatches on either the first argument or, when invoked under a
// command-specific name (e.g. a "loom-commit" symlink), on argv[0].
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dividebyzero/claude-experiments/loom/internal/gitport"
	"github.com/dividebyzero/claude-experiments/loom/internal/multihash"
	"github.com/dividebyzero/claude-experiments/loom/internal/object"
	"github.com/dividebyzero/claude-experiments/loom/internal/refs"
	"github.com/dividebyzero/claude-experiments/loom/internal/repo"
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
	"git-export":  cmdGitExport,
	"git-import":  cmdGitImport,
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
  loom update-ref <name> <new> [old]  atomically set a ref (CAS on old)
  loom show-ref [name]                list refs or resolve one
  loom reflog <name>                  print a ref's append-only log
  loom git-export <dir> [branch]      export HEAD to a plain git repository
  loom git-import <dir> [branch]      import a git branch into this repository
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
		fmt.Printf("author %s\n", c.Author)
		fmt.Printf("timestamp %d\n", c.Timestamp)
		fmt.Printf("\n%s\n", c.Message)
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
	change := object.NewChange(object.Change{
		Tree:      treeID,
		Parents:   parents,
		Message:   msg,
		Timestamp: time.Now().Unix(),
		Author:    author(),
	})
	id, err := r.Objects.Put(change)
	if err != nil {
		return err
	}
	if err := r.Refs.CompareAndSwap(headRef, parent, id, author(), "commit"); err != nil {
		return err
	}
	fmt.Printf("%s %s\n", id.Hex(), msg)
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
	if len(args) == 3 {
		oldVal, err := parseValueOrZero(args[2])
		if err != nil {
			return fmt.Errorf("old value: %w", err)
		}
		return r.Refs.CompareAndSwap(name, oldVal, newVal, author(), "update-ref")
	}
	cur, err := r.Refs.Resolve(name)
	if err != nil && !errors.Is(err, refs.ErrNotExist) {
		return err
	}
	return r.Refs.CompareAndSwap(name, cur, newVal, author(), "update-ref")
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
