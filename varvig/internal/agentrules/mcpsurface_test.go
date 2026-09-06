package agentrules

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// moduleRoot walks up from the test's working directory (the package dir) to the
// module root — the directory holding go.mod — so these tests can reach the
// checked-in release artifacts (mcpb/manifest.json, tools/gate-tools.json) that
// live there rather than beside the package.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("module root (go.mod) not found above the test working directory")
		}
		dir = parent
	}
}

// TestGateToolsOracleGolden keeps tools/gate-tools.json — the language-neutral
// oracle the release smoke asserts the built binary's live surface against — in
// step with GateTools. The smoke (tools/mcp-smoke.py) reads this file for its
// exact tool set and write/read expectations, and the varvig/plugins package
// copies it verbatim beside the smoke, so a gate-tool change reaches every
// surface from one edit. Run with `-update` after an intentional change.
func TestGateToolsOracleGolden(t *testing.T) {
	path := filepath.Join(moduleRoot(t), "tools", "gate-tools.json")
	got := GateToolsOracleJSON()
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated %s", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read oracle (run `go test ./internal/agentrules -run Oracle -update` to create it): %v", err)
	}
	if string(want) != string(got) {
		t.Fatalf("tools/gate-tools.json is stale; if the change is intentional run "+
			"`go test ./internal/agentrules -run Oracle -update`.\nfirst diff:\n%s",
			firstDiff(string(want), string(got)))
	}
}

// TestMCPBManifestMatchesRegistry asserts the MCPB directory manifest advertises
// exactly the registry's gate tools (both directions), carries no promotion tool,
// and gives every tool a non-empty description. The manifest keeps its own
// directory-facing descriptions — only the tool SET is registry-owned — so a gate
// tool cannot be added or removed without the manifest following, which is the
// drift this closes (the manifest fell three tools behind twice before).
func TestMCPBManifestMatchesRegistry(t *testing.T) {
	path := filepath.Join(moduleRoot(t), "mcpb", "manifest.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m struct {
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}

	manifest := map[string]bool{}
	for _, mt := range m.Tools {
		if mt.Name == "" {
			t.Errorf("manifest lists a tool with no name")
			continue
		}
		if strings.TrimSpace(mt.Description) == "" {
			t.Errorf("manifest tool %q has an empty description", mt.Name)
		}
		if strings.Contains(mt.Name, "promote") {
			t.Errorf("manifest advertises %q — a *promote* tool must never exist (the gate can never promote)", mt.Name)
		}
		if manifest[mt.Name] {
			t.Errorf("manifest lists %q twice", mt.Name)
		}
		manifest[mt.Name] = true
	}

	registered := map[string]bool{}
	for _, name := range GateToolNames() {
		registered[name] = true
		if !manifest[name] {
			t.Errorf("registry lists gate tool %q but mcpb/manifest.json does not", name)
		}
	}
	for name := range manifest {
		if !registered[name] {
			t.Errorf("mcpb/manifest.json advertises %q but the registry (agentrules.GateTools) does not", name)
		}
	}
}

// TestMCPDocListsGateTools guards the hand-written design doc (MCP.md): every
// registry gate tool must be named in it. This is a presence check, not a
// generator — MCP.md is prose the doc's tool table lives in — but it still fails
// the build if the table falls behind a gate-tool addition, the same lag the
// manifest and the smoke oracle used to have.
func TestMCPDocListsGateTools(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(moduleRoot(t), "MCP.md"))
	if err != nil {
		t.Fatalf("read MCP.md: %v", err)
	}
	doc := string(raw)
	for _, name := range GateToolNames() {
		if !strings.Contains(doc, name) {
			t.Errorf("MCP.md does not mention gate tool %q — add it to the tool table", name)
		}
	}
}
