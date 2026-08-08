// Command loom is the single, portable Loom binary. Per design §3.1 it is a
// busybox-style multicall executable: one artifact is client, server, and
// tooling. Step 1 wires up the object store, refs, and reflog through a small
// set of plumbing subcommands.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/dividebyzero/claude-experiments/loom/internal/multihash"
	"github.com/dividebyzero/claude-experiments/loom/internal/object"
	"github.com/dividebyzero/claude-experiments/loom/internal/refs"
	"github.com/dividebyzero/claude-experiments/loom/internal/repo"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	if err := dispatch(cmd, args); err != nil {
		fmt.Fprintf(os.Stderr, "loom %s: %v\n", cmd, err)
		os.Exit(1)
	}
}

func dispatch(cmd string, args []string) error {
	switch cmd {
	case "init":
		return cmdInit(args)
	case "hash-object":
		return cmdHashObject(args)
	case "cat-object":
		return cmdCatObject(args)
	case "update-ref":
		return cmdUpdateRef(args)
	case "show-ref":
		return cmdShowRef(args)
	case "reflog":
		return cmdReflog(args)
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `loom — a source control system for agents

usage:
  loom init [dir]                     initialize a repository
  loom hash-object [-w] <file|->      hash (and optionally store) a blob
  loom cat-object <id>                print an object's content/summary
  loom update-ref <name> <new> [old]  atomically set a ref (CAS on old)
  loom show-ref [name]                list refs or resolve one
  loom reflog <name>                  print a ref's append-only log
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
	actor := actorFromEnv()
	if len(args) == 3 {
		oldVal, err := parseValueOrZero(args[2])
		if err != nil {
			return fmt.Errorf("old value: %w", err)
		}
		return r.Refs.CompareAndSwap(name, oldVal, newVal, actor, "update-ref")
	}
	// No expected-old given: create if absent, else overwrite current value.
	cur, err := r.Refs.Resolve(name)
	if err != nil && !errors.Is(err, refs.ErrNotExist) {
		return err
	}
	return r.Refs.CompareAndSwap(name, cur, newVal, actor, "update-ref")
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

func parseValueOrZero(s string) (multihash.Multihash, error) {
	if s == "0" || s == "" {
		return nil, nil
	}
	return multihash.ParseHex(s)
}

func actorFromEnv() string {
	if a := os.Getenv("LOOM_ACTOR"); a != "" {
		return a
	}
	if a := os.Getenv("USER"); a != "" {
		return a
	}
	return "unknown"
}

func hexOrDash(m multihash.Multihash) string {
	if m == nil {
		return "-"
	}
	return m.Hex()
}
