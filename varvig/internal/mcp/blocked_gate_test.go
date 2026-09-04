package mcp

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/blocked"
)

// TestReportBlockedAggregatesBoundaryHits: several out-of-scope reads accumulate
// into the boundary-hit metric, and one varvig_report_blocked call emits a single
// blocked-on-scope report carrying all of them — never widening scope.
func TestReportBlockedAggregatesBoundaryHits(t *testing.T) {
	f := newGateFixture(t)
	gate, grant := newGate(f, "src/auth", time.Hour)

	// Three distinct out-of-scope reaches (one repeated, to prove hits dedupe).
	resps := drive(t, gate,
		call(1, "varvig_read_file", `{"path":"src/web/index.html"}`),
		call(2, "varvig_read_file", `{"path":"src/web/index.html"}`), // repeat: same boundary
		call(3, "varvig_list_tree", `{"path":"README.md"}`),
		call(4, "varvig_task_context", `{}`),
		call(5, "varvig_report_blocked", `{"need":"src/web","why":"the auth page imports the web header","unmet":"write to src/web"}`),
	)

	// task_context reports the metric.
	var ctx struct {
		BoundaryHits int `json:"boundary_hits"`
	}
	if err := json.Unmarshal(decodeTool(t, resps[3]).StructuredContent, &ctx); err != nil {
		t.Fatal(err)
	}
	if ctx.BoundaryHits != 2 {
		t.Fatalf("boundary_hits = %d, want 2 distinct boundaries (repeat collapses)", ctx.BoundaryHits)
	}

	// The report is emitted as one outcome, never widening scope.
	rb := decodeTool(t, resps[4])
	if rb.IsError {
		t.Fatalf("report_blocked errored: %s", rb.Content[0].Text)
	}
	var out struct {
		Outcome      string `json:"outcome"`
		Report       string `json:"report"`
		BoundaryHits int    `json:"boundary_hits"`
		Widened      bool   `json:"widened"`
	}
	if err := json.Unmarshal(rb.StructuredContent, &out); err != nil {
		t.Fatal(err)
	}
	if out.Outcome != "blocked_on_scope" {
		t.Errorf("outcome = %q, want blocked_on_scope", out.Outcome)
	}
	if out.Widened {
		t.Error("a task must never widen its own scope")
	}
	if out.BoundaryHits != 2 {
		t.Errorf("report carries %d hits, want 2", out.BoundaryHits)
	}

	// The scope on the live grant is unchanged — no self-widening happened: it
	// still covers its subtree and still refuses the paths the report named.
	if !grant.Covers("src/auth/login.go") || grant.Covers("src/web/index.html") {
		t.Errorf("grant scope changed (now %q); the gate must not widen scope", grant.Scopes.String())
	}

	// The report is retrievable, verified, bound to the base intent revision.
	reports, err := blocked.List(f.repo, f.base)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 {
		t.Fatalf("stored %d reports, want one aggregated outcome", len(reports))
	}
	if len(reports[0].Hits) != 2 || reports[0].Need != "src/web" {
		t.Fatalf("stored report = %+v", reports[0])
	}
	if reports[0].Author != grant.Fingerprint() {
		t.Errorf("report author = %q, want the task fingerprint", reports[0].Author)
	}
}

// TestReportBlockedRequiresNeed: a report with no need is refused — the outcome
// exists to carry a concrete ask, not to signal a vague stop.
func TestReportBlockedRequiresNeed(t *testing.T) {
	f := newGateFixture(t)
	gate, _ := newGate(f, "src/auth", time.Hour)
	resps := drive(t, gate, call(1, "varvig_report_blocked", `{"why":"stuck"}`))
	tr := decodeTool(t, resps[0])
	if !tr.IsError {
		t.Fatal("report_blocked with no need must be refused")
	}
	if code := errCode(t, tr); code != codeInvalidArgs {
		t.Errorf("code = %q, want %q", code, codeInvalidArgs)
	}
}
