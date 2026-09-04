package main

import (
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/task"
	"time"
)

// P0.5: repeated --scope accumulates rather than taking the last value. unionScope
// is the parse-time accumulator; task.New then unions the comma-joined result.
func TestUnionScopeAccumulates(t *testing.T) {
	// The "/" default is replaced by the first explicit scope, then unioned.
	got := unionScope(unionScope("/", "src/auth"), "src/api")
	if got != "src/auth,src/api" {
		t.Fatalf("unionScope = %q, want src/auth,src/api", got)
	}
	grant, err := task.New(got, true, time.Hour, timeNowForTest())
	if err != nil {
		t.Fatal(err)
	}
	if !grant.Covers("src/auth/login.go") || !grant.Covers("src/api/handler.go") {
		t.Fatal("both declared scopes must be granted, not just the last")
	}
	if grant.Covers("src/other/x.go") {
		t.Fatal("an undeclared path must not be granted")
	}
}

func timeNowForTest() (t time.Time) { return time.Unix(1000, 0) }
