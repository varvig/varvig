package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/agentrules"
)

// TestRegistryCoversCLI is the anti-drift link between the real dispatch table
// and the agent-rules registry: every command the CLI actually exposes must have
// a registry entry (rendered or explicitly excused). Adding a command without a
// rules decision fails the build here.
func TestRegistryCoversCLI(t *testing.T) {
	registered := map[string]bool{}
	for _, c := range agentrules.Commands {
		registered[c.Name] = true
	}
	for name := range commands {
		if !registered[name] {
			t.Errorf("command %q is in the CLI dispatch table but missing from agentrules.Commands "+
				"(mark it agent_facing with a section, or excuse it with a NoRules reason)", name)
		}
	}
	// And the reverse: the registry should not describe commands that do not exist.
	for _, c := range agentrules.Commands {
		if _, ok := commands[c.Name]; !ok {
			t.Errorf("agentrules.Commands lists %q, which is not a real CLI command", c.Name)
		}
	}
}

func TestInitWritesBothByDefault(t *testing.T) {
	dir := t.TempDir()
	if err := cmdInit([]string{dir}); err != nil {
		t.Fatalf("cmdInit: %v", err)
	}
	for _, name := range []string{agentrules.GeneratedName, agentrules.PointerName} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("expected %s to be written: %v", name, err)
		}
	}
}

func TestInitNoAgentRulesWritesNothing(t *testing.T) {
	dir := t.TempDir()
	if err := cmdInit([]string{dir, "--no-agent-rules"}); err != nil {
		t.Fatalf("cmdInit: %v", err)
	}
	for _, name := range []string{agentrules.GeneratedName, agentrules.PointerName} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("--no-agent-rules must not write %s", name)
		}
	}
}
