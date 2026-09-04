package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/check"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// cmdCheck runs the repository's declared verification commands over a proposal's
// tree and records the result as evidence (build spec P1.3). Evidence binds to
// the tree hash it was produced against, so an edit after checking is detectable
// and stale evidence never counts as a pass at promotion.
//
//	varvig check <proposal> [--cmd '<command>' ...]   run declared checks, record evidence
//	varvig check list <proposal>                      show recorded evidence and freshness
//
// Declared commands come from the repeated --cmd flags, or, when none are given,
// from the repo's check-command file (.varvig/check.commands, one per line;
// blank lines and # comments ignored). Each command runs in a fresh materialized
// checkout of the proposal tree; a failing command is recorded as a failure, not
// omitted.
func cmdCheck(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: varvig check <proposal> [--cmd '<command>' ...] | varvig check list <proposal>")
	}
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	if args[0] == "list" {
		if len(args) != 2 {
			return errors.New("usage: varvig check list <proposal>")
		}
		return checkList(r, args[1])
	}

	ref := args[0]
	var commands []string
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--cmd":
			if i+1 >= len(args) {
				return errors.New("check: --cmd requires a command")
			}
			commands, i = append(commands, args[i+1]), i+1
		default:
			return fmt.Errorf("check: unknown argument %q", args[i])
		}
	}

	changeID, err := resolve(r, ref)
	if err != nil {
		return fmt.Errorf("check: cannot resolve %q: %w", ref, err)
	}
	treeID, err := treeOf(r, changeID)
	if err != nil {
		return fmt.Errorf("check: %q is not a change with a tree: %w", ref, err)
	}
	if len(commands) == 0 {
		commands, err = declaredCommands(r)
		if err != nil {
			return err
		}
		if len(commands) == 0 {
			return fmt.Errorf("check: no commands declared — add %s (one command per line) or pass --cmd",
				filepath.Join(".varvig", checkCommandsFile))
		}
	}

	ev, err := check.Run(r, treeID, changeID, commands, time.Now().Unix())
	if err != nil {
		return err
	}
	if _, err := check.Attach(r, ev); err != nil {
		return err
	}
	for _, res := range ev.Results {
		status := "ok"
		if res.Exit != 0 {
			status = fmt.Sprintf("FAIL (exit %d)", res.Exit)
		}
		fmt.Printf("  %-6s %s\n", status, res.Command)
	}
	verdict := "passed"
	if !ev.Passed {
		verdict = "FAILED"
	}
	fmt.Printf("check %s over tree %s: %s\n", verdict, treeID.Hex(), changeID.Hex())
	return nil
}

// checkCommandsFile is the per-repo declaration of verification commands.
const checkCommandsFile = "check.commands"

// declaredCommands reads the repo's declared verification commands, ignoring
// blank lines and # comments. A missing file is not an error — it is simply no
// declared commands.
func declaredCommands(r *repo.Repo) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(r.GitDir(), checkCommandsFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out, nil
}

func checkList(r *repo.Repo, ref string) error {
	changeID, err := resolve(r, ref)
	if err != nil {
		return fmt.Errorf("check list: cannot resolve %q: %w", ref, err)
	}
	treeID, err := treeOf(r, changeID)
	if err != nil {
		return fmt.Errorf("check list: %q is not a change with a tree: %w", ref, err)
	}
	evs, err := check.List(r, changeID)
	if err != nil {
		return err
	}
	if len(evs) == 0 {
		fmt.Println("(no check evidence for this proposal)")
		return nil
	}
	for _, ev := range evs {
		freshness := "stale"
		if ev.Fresh(treeID) {
			freshness = "fresh"
		}
		verdict := "passed"
		if !ev.Passed {
			verdict = "failed"
		}
		fmt.Printf("%s  %s  tree %s  %s\n", freshness, verdict, ev.Tree, time.Unix(ev.Timestamp, 0).Format(time.RFC3339))
	}
	// Name the current tree so a stale line is self-explanatory.
	fmt.Printf("current tree %s\n", treeID.Hex())
	return nil
}
