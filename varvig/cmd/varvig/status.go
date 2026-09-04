package main

import (
	"fmt"

	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/worktree"
)

// cmdStatus is the cheap orient command (build spec P0.3): the working tree's
// changes against the base, grouped by kind. It answers the change question; the
// scope question (which paths fall inside a declared write set) is answered once a
// task's scope is available in the CLI.
//
//	varvig status
func cmdStatus(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: varvig status")
	}
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	base, work, _, err := diffSides(r, nil)
	if err != nil {
		return err
	}
	d := worktree.Compare(base, work)
	if d.Empty() {
		fmt.Println("clean (working tree matches base)")
		return nil
	}
	group := func(label string, ps []string) {
		for _, p := range ps {
			fmt.Printf("%-9s %s\n", label, p)
		}
	}
	group("added", d.Added)
	group("modified", d.Modified)
	group("deleted", d.Removed)
	group("mode", d.ModeChanged)
	for _, rn := range d.Renamed {
		fmt.Printf("%-9s %s -> %s\n", "renamed", rn.From, rn.To)
	}
	return nil
}
