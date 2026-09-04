package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/denylist"
)

// writeDeny writes a repo deny-list file so a gate built afterward loads it.
func writeDeny(t *testing.T, f *gateFixture, lines ...string) {
	t.Helper()
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(f.repo.GitDir(), "denylist"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestDenyListShipsEmpty: with no deny-list file, the gate loads an empty list and
// reads within scope are unaffected — the shipped state (design addendum, U5).
func TestDenyListShipsEmpty(t *testing.T) {
	f := newGateFixture(t)
	gate, _ := newGate(f, "/", time.Hour)
	if !gate.deny.Empty() {
		t.Fatalf("a repo with no deny-list file must load empty, got %s", gate.deny)
	}
	resps := drive(t, gate, call(1, "varvig_read_file", `{"path":"src/web/index.html"}`))
	if tr := decodeTool(t, resps[0]); tr.IsError {
		t.Fatalf("with an empty deny-list a normal read must succeed: %s", tr.Content[0].Text)
	}
}

// TestDenyListRefusesReadDistinctFromNotFound: a denied read fails with the
// `denied` code, never `not_found` — the ambiguity that makes sparse dangerous
// must not reappear.
func TestDenyListRefusesReadDistinctFromNotFound(t *testing.T) {
	f := newGateFixture(t)
	writeDeny(t, f, "src/web")
	gate, _ := newGate(f, "/", time.Hour)

	// A real, present, in-scope path — but deny-listed.
	resps := drive(t, gate, call(1, "varvig_read_file", `{"path":"src/web/index.html"}`))
	tr := decodeTool(t, resps[0])
	if !tr.IsError {
		t.Fatal("a deny-listed read must be refused")
	}
	if code := errCode(t, tr); code != codeDenied {
		t.Fatalf("denied read code = %q, want %q (never not_found)", code, codeDenied)
	}

	// A genuinely absent path still reports not_found — the two are distinguishable.
	resps = drive(t, gate, call(1, "varvig_read_file", `{"path":"src/auth/missing.go"}`))
	if code := errCode(t, decodeTool(t, resps[0])); code == codeDenied {
		t.Fatal("an absent (not denied) path must not report denied")
	}
}

// TestDenyListRefusesPropose: with a populated list and F5 unbuilt, a proposal
// touching a denied path is refused rather than producing a tree.
func TestDenyListRefusesPropose(t *testing.T) {
	f := newGateFixture(t)
	writeDeny(t, f, "src/web")
	gate, _ := newGate(f, "/", time.Hour)

	args, _ := json.Marshal(map[string]any{
		"message": "m",
		"files":   []map[string]any{{"path": "src/web/evil.html", "content": "x"}},
	})
	resps := drive(t, gate, call(1, "varvig_propose", string(args)))
	tr := decodeTool(t, resps[0])
	if !tr.IsError {
		t.Fatal("proposing a deny-listed path must be refused")
	}
	if code := errCode(t, tr); code != codeDenied {
		t.Fatalf("denied propose code = %q, want %q", code, codeDenied)
	}
}

// TestDenyListLoadedByGate: the gate's list reflects the repo file.
func TestDenyListLoadedByGate(t *testing.T) {
	f := newGateFixture(t)
	writeDeny(t, f, "secrets", "src/web")
	gate, _ := newGate(f, "/", time.Hour)
	if gate.deny.Empty() {
		t.Fatal("gate should have loaded the deny-list")
	}
	var _ denylist.List = gate.deny
}
