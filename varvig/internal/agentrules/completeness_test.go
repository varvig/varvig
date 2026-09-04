package agentrules

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "update golden files")

// TestSurfaceCompleteness is the actual anti-drift mechanism (per the spec):
// every agent-facing surface entry must be either rendered into the file or
// explicitly excused with a NoRules reason. A new command/flag/field cannot ship
// without a rules decision.
func TestSurfaceCompleteness(t *testing.T) {
	body := renderBody(RepoFacts{})

	for _, c := range Commands {
		if c.AgentFacing == (c.NoRules != "") {
			t.Errorf("command %q: exactly one of AgentFacing / NoRules must hold (agentFacing=%v noRules=%q)",
				c.Name, c.AgentFacing, c.NoRules)
		}
		if c.AgentFacing && !strings.Contains(body, "`"+c.Name+"`") {
			t.Errorf("agent-facing command %q is not rendered into VARVIG-AGENTS.md", c.Name)
		}
		for _, f := range c.Flags {
			if f.AgentFacing == (f.NoRules != "") {
				t.Errorf("command %q flag %q: exactly one of AgentFacing / NoRules must hold", c.Name, f.Name)
			}
			if f.AgentFacing && !strings.Contains(body, "`"+f.Name+"`") {
				t.Errorf("agent-facing flag %q (%s) is not rendered", f.Name, c.Name)
			}
		}
	}

	for _, f := range IntentFields {
		if f.AgentFacing == (f.NoRules != "") {
			t.Errorf("intent field %q: exactly one of AgentFacing / NoRules must hold", f.Name)
		}
		if f.AgentFacing && !strings.Contains(body, "`"+f.Name+"`") {
			t.Errorf("agent-facing intent field %q is not rendered", f.Name)
		}
	}

	seenGate := map[string]bool{}
	for _, gt := range GateTools {
		if gt.Name == "" || gt.Summary == "" {
			t.Errorf("gate tool %+v: both Name and Summary are required", gt)
		}
		if seenGate[gt.Name] {
			t.Errorf("gate tool %q is listed twice", gt.Name)
		}
		seenGate[gt.Name] = true
		if !strings.Contains(body, "`"+gt.Name+"`") {
			t.Errorf("gate tool %q is not rendered into VARVIG-AGENTS.md", gt.Name)
		}
	}

	for _, e := range Errors {
		if !strings.Contains(body, e.Code) {
			t.Errorf("error code %q is not rendered", e.Code)
		}
	}
	for _, c := range CredentialScope {
		if !strings.Contains(body, c.Capability) {
			t.Errorf("credential capability %q is not rendered", c.Capability)
		}
	}
}

// TestLengthBudget guards the running cost: the file is read on every agent
// invocation, so if it blows past ~150 lines that signals too much is marked
// agent-facing — not a reason to raise the limit.
func TestLengthBudget(t *testing.T) {
	content, _ := Generate(testVersion, RepoFacts{})
	if n := strings.Count(content, "\n"); n > 150 {
		t.Fatalf("VARVIG-AGENTS.md is %d lines; budget is ~150 — trim agent-facing surface, do not raise the limit", n)
	}
}

// TestDeterministic asserts byte-identical output for the same inputs, and that
// the surface hash and body are independent of the version string.
func TestDeterministic(t *testing.T) {
	facts := RepoFacts{AcceptanceGates: []string{"pre-commit"}}
	a, sa := Generate("v1.2.3", facts)
	b, sb := Generate("v1.2.3", facts)
	if a != b || sa != sb {
		t.Fatal("generation is not deterministic for identical inputs")
	}
	// Different version: header differs, but body and surface must not.
	c, sc := Generate("v9.9.9", facts)
	if sc != sa {
		t.Fatalf("surface changed with version alone: %s vs %s", sc, sa)
	}
	if bodyOf(c) != bodyOf(a) {
		t.Fatal("body changed with version alone; body must be version-independent")
	}
}

// TestGolden pins the exact rendered output for a fixed version. Run with
// `-update` to regenerate after an intentional change.
func TestGolden(t *testing.T) {
	content, _ := Generate(testVersion, RepoFacts{})
	path := filepath.Join("testdata", "VARVIG-AGENTS.golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated %s", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run `go test -run TestGolden -update` to create it): %v", err)
	}
	if string(want) != content {
		t.Fatalf("generated output differs from golden; if intentional, run with -update.\n"+
			"first diff:\n%s", firstDiff(string(want), content))
	}
}

func firstDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(al) && i < len(bl); i++ {
		if al[i] != bl[i] {
			return "line " + itoaSmall(i+1) + ":\n  golden: " + al[i] + "\n  got:    " + bl[i]
		}
	}
	return "(length differs)"
}

func itoaSmall(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}
