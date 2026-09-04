package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/worktree"
)

// proposeChange proposes a single in-scope file through the gate and returns the
// resulting change hash.
func proposeChange(t *testing.T, gate *Gate, path, content string) string {
	t.Helper()
	args, _ := json.Marshal(map[string]any{
		"message": "m",
		"files":   []map[string]any{{"path": path, "content": content}},
	})
	resps := drive(t, gate, call(1, "varvig_propose", string(args)))
	var prop struct {
		Change string `json:"change"`
	}
	if err := json.Unmarshal(decodeTool(t, resps[0]).StructuredContent, &prop); err != nil {
		t.Fatal(err)
	}
	if prop.Change == "" {
		t.Fatal("propose returned no change")
	}
	return prop.Change
}

// TestGateDiffOfProposal: varvig_diff on a proposal shows its own change against
// the base — the second feedback channel, now reachable through the gate.
func TestGateDiffOfProposal(t *testing.T) {
	f := newGateFixture(t)
	gate, _ := newGate(f, "src/auth", time.Hour)
	change := proposeChange(t, gate, "src/auth/login.go", "package auth // patched\n")

	resps := drive(t, gate, call(1, "varvig_diff", `{"change":"`+change+`"}`))
	var out struct {
		Changed   []string `json:"changed"`
		Unified   string   `json:"unified"`
		Truncated bool     `json:"truncated"`
	}
	if err := json.Unmarshal(decodeTool(t, resps[0]).StructuredContent, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Changed) != 1 || out.Changed[0] != "src/auth/login.go" {
		t.Fatalf("changed = %v, want [src/auth/login.go]", out.Changed)
	}
	if out.Unified == "" {
		t.Fatal("unified diff should be non-empty for a modified file")
	}
}

// TestGateDiffScopeConfined: a change that only touches paths outside the task's
// scope shows nothing — a diff never surfaces out-of-scope content.
func TestGateDiffScopeConfined(t *testing.T) {
	f := newGateFixture(t)
	// Propose a src/web change with a wide-open task.
	wide, _ := newGate(f, "/", time.Hour)
	change := proposeChange(t, wide, "src/web/evil.html", "<h1>x</h1>\n")

	// A task scoped to src/auth diffs that change: src/web is out of scope, so the
	// diff is empty and status is clean.
	narrow, _ := newGate(f, "src/auth", time.Hour)
	resps := drive(t, narrow,
		call(1, "varvig_diff", `{"change":"`+change+`"}`),
		call(2, "varvig_status", `{"change":"`+change+`"}`),
	)
	var d struct {
		Changed []string `json:"changed"`
		Unified string   `json:"unified"`
	}
	if err := json.Unmarshal(decodeTool(t, resps[0]).StructuredContent, &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Changed) != 0 || d.Unified != "" {
		t.Fatalf("out-of-scope change leaked into diff: changed=%v unified=%q", d.Changed, d.Unified)
	}
	var s struct {
		Clean bool `json:"clean"`
	}
	if err := json.Unmarshal(decodeTool(t, resps[1]).StructuredContent, &s); err != nil {
		t.Fatal(err)
	}
	if !s.Clean {
		t.Fatal("status must be clean when every change is out of scope")
	}
}

// TestGateStatusOfCheckout: with a bound checkout, status summarizes the working
// tree against the base, the same view the CLI gives — reachable from the gate.
func TestGateStatusOfCheckout(t *testing.T) {
	f := newGateFixture(t)
	dir := t.TempDir()
	authDir := filepath.Join(dir, "src", "auth")
	if err := worktree.Checkout(f.repo.Objects, subtreeHash(t, f, "src/auth"), authDir); err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "login.go"), []byte("package auth // edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "new.go"), []byte("package auth\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	gate, _ := newGate(f, "src/auth", time.Hour)
	gate.SetCheckout(dir)
	resps := drive(t, gate, call(1, "varvig_status", `{}`))
	var s struct {
		Clean    bool     `json:"clean"`
		Added    []string `json:"added"`
		Modified []string `json:"modified"`
	}
	if err := json.Unmarshal(decodeTool(t, resps[0]).StructuredContent, &s); err != nil {
		t.Fatal(err)
	}
	if s.Clean {
		t.Fatal("status should report the checkout's edits, not clean")
	}
	got := map[string]bool{}
	for _, p := range append(s.Added, s.Modified...) {
		got[p] = true
	}
	if !got["src/auth/new.go"] || !got["src/auth/login.go"] {
		t.Fatalf("status missed a checkout change: added=%v modified=%v", s.Added, s.Modified)
	}
}

// TestGateDiffNeedsChangeOrCheckout: with neither a change nor a checkout, diff
// explains what it needs rather than returning an empty result.
func TestGateDiffNeedsChangeOrCheckout(t *testing.T) {
	f := newGateFixture(t)
	gate, _ := newGate(f, "src/auth", time.Hour)
	resps := drive(t, gate, call(1, "varvig_diff", `{}`))
	tr := decodeTool(t, resps[0])
	if !tr.IsError {
		t.Fatal("diff with no change and no checkout must error")
	}
	if code := errCode(t, tr); code != codeInvalidArgs {
		t.Errorf("code = %q, want %q", code, codeInvalidArgs)
	}
}
