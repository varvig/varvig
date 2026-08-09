package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/readapi"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// cmdRead is the CLI plumbing over the read query layer (auth design §7.1).
// These commands emit machine JSON — the plumbing/porcelain split adopted from
// Git — so scripts, hooks, and sandboxes never parse output meant for humans,
// and they share the *same* query layer as the HTTP server, so the two can
// never drift.
//
//	varvig read refs
//	varvig read object <hash|ref>
//	varvig read tree   <hash|ref> [path]
//	varvig read blob   <hash|ref>
//	varvig read change <hash|ref>
//	varvig read log    <hash|ref> [limit]
//	varvig read proposals [scope]
func cmdRead(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: varvig read <refs|object|tree|blob|change|log|proposals> [args]")
	}
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	q := readapi.New(r)
	sub, rest := args[0], args[1:]

	switch sub {
	case "refs":
		refs, err := q.Refs()
		if err != nil {
			return err
		}
		return emitJSON(refs)
	case "proposals":
		scope := ""
		if len(rest) > 0 {
			scope = rest[0]
		}
		props, err := q.Proposals(scope)
		if err != nil {
			return err
		}
		return emitJSON(props)
	case "object":
		id, err := resolveRead(q, rest)
		if err != nil {
			return err
		}
		info, err := q.Object(id)
		if err != nil {
			return err
		}
		return emitJSON(info)
	case "tree":
		id, err := resolveRead(q, rest)
		if err != nil {
			return err
		}
		path := ""
		if len(rest) > 1 {
			path = rest[1]
		}
		listing, err := q.Tree(id, path)
		if err != nil {
			return err
		}
		return emitJSON(listing)
	case "blob":
		id, err := resolveRead(q, rest)
		if err != nil {
			return err
		}
		content, err := q.Blob(id)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(content) // raw bytes, not JSON
		return err
	case "change":
		id, err := resolveRead(q, rest)
		if err != nil {
			return err
		}
		view, err := q.Change(id)
		if err != nil {
			return err
		}
		return emitJSON(view)
	case "log":
		id, err := resolveRead(q, rest)
		if err != nil {
			return err
		}
		limit := 0
		if len(rest) > 1 {
			if n, err := strconv.Atoi(rest[1]); err == nil {
				limit = n
			}
		}
		entries, err := q.Log(id, limit)
		if err != nil {
			return err
		}
		return emitJSON(entries)
	default:
		return fmt.Errorf("read: unknown subcommand %q", sub)
	}
}

func resolveRead(q *readapi.Query, rest []string) (multihash.Multihash, error) {
	if len(rest) < 1 {
		return nil, errors.New("read: expected a <hash|ref> argument")
	}
	return q.Resolve(rest[0])
}

func emitJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
