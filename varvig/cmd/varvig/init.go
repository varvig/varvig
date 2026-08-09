package main

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/agentrules"
	"github.com/dividebyzero/claude-experiments/varvig/internal/hook"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// cmdInit initializes a repository and, by default, writes the agent-rules files
// (VARVIG-AGENTS.md + an AGENTS.md pointer). The agent-rules step can never fail
// a repo init: on the write path every outcome is a write, a skip, or a notice,
// and the command exits 0. The inspection modes (--check/--diff/--print) do not
// create a repo and carry their own exit codes.
//
//	varvig init [dir]                      init; write both files (default)
//	varvig init --no-agent-rules [dir]     init; write no rules files
//	varvig init --agent-rules [dir]        (re)write the rules files
//	varvig init --agent-rules --check      exit 2 if stale/missing; writes nothing
//	varvig init --agent-rules --diff       print unified diffs; writes nothing
//	varvig init --agent-rules --print      print VARVIG-AGENTS.md; writes nothing
//	varvig init --agent-rules --json       machine-readable result
func cmdInit(args []string) error {
	dir := "."
	agentRules := true
	noAgentRules := false
	jsonOut := false
	mode := agentrules.ModeWrite
	modeFlag := ""

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--agent-rules":
			agentRules = true
		case "--no-agent-rules":
			noAgentRules = true
		case "--check":
			mode, modeFlag = agentrules.ModeCheck, "--check"
		case "--diff":
			mode, modeFlag = agentrules.ModeDiff, "--diff"
		case "--print":
			mode, modeFlag = agentrules.ModePrint, "--print"
		case "--json":
			jsonOut = true
		default:
			if strings.HasPrefix(a, "-") {
				return fmt.Errorf("unknown flag %q", a)
			}
			dir = a
		}
	}
	if noAgentRules {
		if modeFlag != "" {
			return fmt.Errorf("%s cannot be combined with --no-agent-rules", modeFlag)
		}
		agentRules = false
	}

	inspection := mode != agentrules.ModeWrite

	// Inspection modes never create a repo and never write. They report on the
	// files as they stand, with exit codes the CI/agent path relies on.
	if inspection {
		opts := agentrules.Options{Root: dir, Version: versionString(), Mode: mode}
		if r, err := repo.Open(dir); err == nil {
			opts.RepoPresent = true
			opts.Facts = resolveFacts(r)
		} else if !errors.Is(err, repo.ErrNotRepo) {
			return err
		}
		res, err := agentrules.Run(opts)
		if err != nil {
			return err
		}
		emit(res, jsonOut)
		if res.Exit != 0 {
			os.Exit(res.Exit)
		}
		return nil
	}

	// Write path: create the repo (idempotent — an existing repo is opened, so a
	// second `varvig init` is a no-op rather than an error), then the rules files.
	created := false
	r, err := repo.Init(dir)
	switch {
	case err == nil:
		created = true
	case errors.Is(err, repo.ErrExists):
		r, err = repo.Open(dir)
		if err != nil {
			return err
		}
	default:
		return err
	}

	if !jsonOut && created {
		fmt.Printf("initialized empty varvig repository in %s\n", r.GitDir())
	}

	if !agentRules {
		return nil // --no-agent-rules: nothing about rules is written or printed
	}

	res, runErr := agentrules.Run(agentrules.Options{
		Root: r.Root(), Version: versionString(), Mode: agentrules.ModeWrite,
		RepoPresent: true, Facts: resolveFacts(r),
	})
	if jsonOut {
		fmt.Println(res.JSON())
		return nil // never fail a repo init
	}
	// Human path: print the generated file's stderr notice (local-edit replacement
	// diagnostics), then a short summary. A rare IO fault is surfaced, not fatal.
	if res.Stderr != "" {
		fmt.Fprint(os.Stderr, res.Stderr)
	}
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "warning: agent rules could not be fully written: %v\n", runErr)
		return nil
	}
	printWriteNotice(res)
	return nil
}

// emit prints an agentrules result for the inspection modes: JSON to stdout, or
// the human stdout/stderr text the result carries.
func emit(res agentrules.Result, jsonOut bool) {
	if jsonOut {
		fmt.Println(res.JSON())
		return
	}
	if res.Stdout != "" {
		fmt.Print(res.Stdout)
	}
	if res.Stderr != "" {
		fmt.Fprint(os.Stderr, res.Stderr)
	}
}

// printWriteNotice prints the short, human summary for the default init path,
// reflecting what actually happened to each file.
func printWriteNotice(res agentrules.Result) {
	switch res.Generated {
	case "created":
		fmt.Printf("wrote %s (generated; overwritten on upgrade)\n", agentrules.GeneratedName)
	case "replaced":
		fmt.Printf("regenerated %s (replaced prior content)\n", agentrules.GeneratedName)
	case "current":
		fmt.Printf("%s already current\n", agentrules.GeneratedName)
	}
	switch res.Pointer {
	case "added":
		fmt.Printf("added varvig section to %s\n", agentrules.PointerName)
	case "present":
		fmt.Printf("%s already points to %s\n", agentrules.PointerName, agentrules.GeneratedName)
	case "skipped":
		fmt.Printf("%s already mentions %s; left as-is\n", agentrules.PointerName, agentrules.GeneratedName)
	}
	fmt.Printf("regenerate with `varvig init --agent-rules`, skip with `--no-agent-rules`\n")
}

// resolveFacts reads the repo-local facts that shape VARVIG-AGENTS.md — the
// configured acceptance gates. It reads only varvig's own metadata (the hook
// manifest), never .git, and degrades to empty (which renders an explicit TODO)
// rather than inventing defaults.
func resolveFacts(r *repo.Repo) agentrules.RepoFacts {
	cfg, err := hook.LoadManifest(r)
	if err != nil {
		return agentrules.RepoFacts{}
	}
	set := map[string]bool{}
	for _, e := range cfg.Entries {
		set[e.Event] = true
	}
	gates := make([]string, 0, len(set))
	for g := range set {
		gates = append(gates, g)
	}
	sort.Strings(gates)
	return agentrules.RepoFacts{AcceptanceGates: gates}
}
