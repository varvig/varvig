package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dividebyzero/claude-experiments/varvig/internal/identity"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/trust"
)

// cmdTrust inspects the repository trust store (.vcs/allowed_keys). It reads the
// working-tree copy — the file a user edits and commits — rather than a
// committed snapshot; ref-update verification is the path that reads the store
// as of a specific ref state (auth design §5.2).
func cmdTrust(args []string) error {
	sub := "list"
	rest := args
	if len(args) > 0 {
		sub, rest = args[0], args[1:]
	}
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	f, err := loadTrustFile(r)
	if err != nil {
		return err
	}
	switch sub {
	case "list":
		recs := f.Records()
		if len(recs) == 0 {
			fmt.Printf("no principals in %s\n", trust.DefaultPath)
			return nil
		}
		for _, rec := range recs {
			fmt.Printf("%s  %-12s  %-12s  %s\n", rec.Fingerprint, rec.Name, rec.Scope, rec.Right)
		}
		return nil
	case "check":
		scope := "/"
		if len(rest) > 0 {
			scope = rest[0]
		}
		id, err := identity.Resolve("", os.Getenv)
		if err != nil {
			return err
		}
		defer id.Close()
		fp := id.Fingerprint()
		recs := f.Lookup(fp)
		if len(recs) == 0 {
			fmt.Printf("%s (%s): not in the trust store\n", id.PublicKey.Comment, fp)
			return nil
		}
		promote := f.Authorized(fp, trust.RightPromote, scope)
		propose := f.Authorized(fp, trust.RightPropose, scope)
		fmt.Printf("%s (%s) at %s: promote=%v propose=%v\n",
			id.PublicKey.Comment, fp, scope, promote, propose)
		return nil
	default:
		return fmt.Errorf("trust: unknown subcommand %q (want: list, check)", sub)
	}
}

// loadTrustFile reads and parses the working-tree trust store. A missing file
// is an empty store, not an error: a repository with no allowed_keys simply has
// no promoters yet.
func loadTrustFile(r *repo.Repo) (*trust.File, error) {
	b, err := os.ReadFile(filepath.Join(r.Root(), trust.DefaultPath))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return trust.Parse(nil), nil
		}
		return nil, err
	}
	return trust.Parse(b), nil
}
