package mcp

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// C1: reasoning is accepted, persisted, confirmed from storage in the response,
// and surfaced by read_change.
func TestProposePersistsReasoning(t *testing.T) {
	f := newGateFixture(t)
	gate, _ := newGate(f, "src/auth", time.Hour)
	const reasoning = "chose a helper over inlining to keep login.go small; deferred the error-wrap refactor"

	tr := decodeTool(t, drive(t, gate, call(1, "varvig_propose",
		`{"message":"add helper","reasoning":"`+reasoning+`","files":[{"path":"src/auth/helper.go","content":"package auth\n"}]}`))[0])
	if tr.IsError {
		t.Fatalf("propose errored: %s", tr.StructuredContent)
	}
	var pr struct {
		Change string `json:"change"`
		Intent struct {
			Reasoning string `json:"reasoning"`
			TaskSpec  string `json:"task_spec"`
		} `json:"intent"`
	}
	if err := json.Unmarshal(tr.StructuredContent, &pr); err != nil {
		t.Fatal(err)
	}
	if pr.Intent.Reasoning != reasoning {
		t.Fatalf("response intent.reasoning = %q; want the stored value confirmed back", pr.Intent.Reasoning)
	}

	rc := decodeTool(t, drive(t, gate, call(2, "varvig_read_change", `{"change":"`+pr.Change+`"}`))[0])
	if rc.IsError {
		t.Fatalf("read_change errored: %s", rc.StructuredContent)
	}
	if !strings.Contains(string(rc.StructuredContent), "deferred the error-wrap") {
		t.Fatalf("read_change did not surface the persisted reasoning: %s", rc.StructuredContent)
	}
}

// C0.1 / C1: an input field the gate does not model is a refusal, not a drop.
func TestGateRejectsUnknownField(t *testing.T) {
	f := newGateFixture(t)
	gate, _ := newGate(f, "src/auth", time.Hour)
	tr := decodeTool(t, drive(t, gate, call(1, "varvig_propose",
		`{"message":"m","notes":"a field the gate does not persist","files":[{"path":"src/auth/x.go","content":"package auth\n"}]}`))[0])
	if !tr.IsError {
		t.Fatal("an unknown field must be refused, not accepted and dropped")
	}
	if c := errCode(t, tr); c != codeInvalidArgs {
		t.Errorf("unknown-field code = %q, want %q", c, codeInvalidArgs)
	}
}

// C2: a wrong parameter name must error, never fall back to the base — this is
// the isolation breach where {"ref":…} returned another change presented as one's own.
func TestReadChangeWrongParamErrors(t *testing.T) {
	f := newGateFixture(t)
	gate, _ := newGate(f, "src/auth", time.Hour)
	tr := decodeTool(t, drive(t, gate, call(1, "varvig_read_change", `{"ref":"whatever"}`))[0])
	if !tr.IsError {
		t.Fatal("wrong parameter name must error, not silently return the base")
	}
}

// C2: an unresolvable hash errors; it never degrades to the base.
func TestReadChangeNonsenseHashErrors(t *testing.T) {
	f := newGateFixture(t)
	gate, _ := newGate(f, "src/auth", time.Hour)
	tr := decodeTool(t, drive(t, gate, call(1, "varvig_read_change",
		`{"change":"1e20ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}`))[0])
	if !tr.IsError {
		t.Fatal("a nonsense hash must error, not return the base")
	}
}

// C2: a valid read returns exactly the requested object, self-identifying by hash.
func TestReadChangeSelfIdentifies(t *testing.T) {
	f := newGateFixture(t)
	gate, _ := newGate(f, "src/auth", time.Hour)
	pr := decodeTool(t, drive(t, gate, call(1, "varvig_propose",
		`{"message":"m","files":[{"path":"src/auth/helper.go","content":"package auth\n"}]}`))[0])
	var p struct {
		Change string `json:"change"`
	}
	if err := json.Unmarshal(pr.StructuredContent, &p); err != nil {
		t.Fatal(err)
	}
	rc := decodeTool(t, drive(t, gate, call(2, "varvig_read_change", `{"change":"`+p.Change+`"}`))[0])
	var got struct {
		Change string `json:"change"`
	}
	if err := json.Unmarshal(rc.StructuredContent, &got); err != nil {
		t.Fatal(err)
	}
	if got.Change != p.Change {
		t.Fatalf("read_change returned %q, want the requested %q", got.Change, p.Change)
	}
}

// C3: tools/list, the handler table, and MCP.md agree — one test, both directions
// of drift, run in CI.
func TestToolSurfaceMatchesHandlersAndDoc(t *testing.T) {
	listed := map[string]bool{}
	for _, tl := range toolList {
		listed[tl["name"].(string)] = true
	}
	for name := range toolHandlers {
		if !listed[name] {
			t.Errorf("%s is handled but missing from tools/list", name)
		}
	}
	for name := range listed {
		if _, ok := toolHandlers[name]; !ok {
			t.Errorf("%s is in tools/list but has no handler", name)
		}
	}

	doc, err := os.ReadFile("../../MCP.md")
	if err != nil {
		t.Fatalf("read MCP.md: %v", err)
	}
	documented := map[string]bool{}
	for _, m := range regexp.MustCompile(`varvig_[a-z]+(?:_[a-z]+)*`).FindAllString(string(doc), -1) {
		documented[m] = true
	}
	for name := range listed {
		if !documented[name] {
			t.Errorf("%s is a real verb but undocumented in MCP.md", name)
		}
	}
	for name := range documented {
		if _, ok := toolHandlers[name]; !ok {
			t.Errorf("MCP.md names %s but the gate does not implement it", name)
		}
	}
}
