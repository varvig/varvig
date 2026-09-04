package main

import (
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/trust"
	"github.com/dividebyzero/claude-experiments/varvig/internal/worktree"
)

func editSet(edits []proposeEdit) map[string]bool {
	m := map[string]bool{}
	for _, e := range edits {
		m[e.path] = true
	}
	return m
}

func TestSelectEditsObservesEverythingInScope(t *testing.T) {
	d := worktree.TreeDiff{
		Added:    []string{"src/auth/new.go"},
		Modified: []string{"src/auth/login.go"},
		Removed:  []string{"src/auth/old.go"},
	}
	edits, err := selectEdits(d, trust.NewScopeSet("src/auth"), nil)
	if err != nil {
		t.Fatalf("selectEdits: %v", err)
	}
	got := editSet(edits)
	for _, p := range []string{"src/auth/new.go", "src/auth/login.go", "src/auth/old.go"} {
		if !got[p] {
			t.Errorf("a changed-and-forgotten path %q was dropped from the observed set", p)
		}
	}
}

func TestSelectEditsRefusesOutOfScope(t *testing.T) {
	d := worktree.TreeDiff{Modified: []string{"src/auth/login.go", "src/web/index.html"}}
	if _, err := selectEdits(d, trust.NewScopeSet("src/auth"), nil); err == nil {
		t.Fatal("a change outside the declared scope must be refused, not silently truncated")
	}
}

func TestSelectEditsExplicitMustBeObserved(t *testing.T) {
	d := worktree.TreeDiff{Modified: []string{"src/auth/login.go"}}
	if _, err := selectEdits(d, trust.NewScopeSet("/"), []string{"src/auth/never.go"}); err == nil {
		t.Fatal("an explicit path that did not change must error, not propose nothing for it")
	}
	// A valid explicit path narrows the set.
	edits, err := selectEdits(worktree.TreeDiff{
		Modified: []string{"src/auth/login.go", "src/auth/other.go"},
	}, trust.NewScopeSet("/"), []string{"src/auth/login.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(edits) != 1 || edits[0].path != "src/auth/login.go" {
		t.Fatalf("explicit narrowing = %+v", edits)
	}
}

func TestSelectEditsEmptyIsDistinctError(t *testing.T) {
	if _, err := selectEdits(worktree.TreeDiff{}, trust.NewScopeSet("/"), nil); err == nil {
		t.Fatal("an empty observed set must be a named error, not a successful empty proposal")
	}
}

func TestSelectEditsRenameSplits(t *testing.T) {
	d := worktree.TreeDiff{Renamed: []worktree.Rename{{From: "a/old.go", To: "a/new.go"}}}
	edits, err := selectEdits(d, trust.NewScopeSet("/"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var delOld, addNew bool
	for _, e := range edits {
		if e.path == "a/old.go" && e.del {
			delOld = true
		}
		if e.path == "a/new.go" && !e.del {
			addNew = true
		}
	}
	if !delOld || !addNew {
		t.Fatalf("a rename must delete the old path and write the new: %+v", edits)
	}
}
