package mcp

import (
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/agentrules"
)

// TestGateSurfaceMatchesRegistry is the U2 anti-drift test: the gate's live
// tools/list and the single schema source (agentrules.GateTools, which
// VARVIG-AGENTS.md is rendered from) must agree in both directions. A tool added
// to the gate without a registry entry fails here; so does a registry entry with
// no live tool. This is what keeps tools/list, the gate implementation, and
// VARVIG-AGENTS.md from diverging (design addendum, U2).
func TestGateSurfaceMatchesRegistry(t *testing.T) {
	live := map[string]bool{}
	for _, tl := range toolList {
		live[tl["name"].(string)] = true
	}
	registered := map[string]bool{}
	for _, name := range agentrules.GateToolNames() {
		registered[name] = true
	}

	for name := range live {
		if !registered[name] {
			t.Errorf("gate exposes %q but the rules registry (agentrules.GateTools) does not list it", name)
		}
	}
	for name := range registered {
		if !live[name] {
			t.Errorf("the rules registry lists %q but the gate does not expose it", name)
		}
	}

	// The two write tools the registry flags must be the two non-readOnly tools the
	// gate advertises — capability shape, not just names, stays in sync.
	regWrite := map[string]bool{}
	for _, gt := range agentrules.GateTools {
		if gt.Write {
			regWrite[gt.Name] = true
		}
	}
	for _, tl := range toolList {
		name := tl["name"].(string)
		ann, _ := tl["annotations"].(map[string]any)
		ro, _ := ann["readOnlyHint"].(bool)
		if regWrite[name] == ro {
			t.Errorf("tool %q: registry Write=%v disagrees with the gate's readOnlyHint=%v", name, regWrite[name], ro)
		}
	}
}
