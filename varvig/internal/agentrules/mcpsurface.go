package agentrules

import (
	"bytes"
	"encoding/json"
)

// The MCP gate surface has three consumers beside the gate itself, and all three
// derive from GateTools so none can drift (design addendum, U2):
//
//   - VARVIG-AGENTS.md — rendered in generate.go from GateTools.
//   - mcpb/manifest.json — the MCPB directory manifest. A test in this package
//     (TestMCPBManifestMatchesRegistry) asserts its tool set equals GateTools in
//     both directions, so a gate tool cannot be added without a manifest entry,
//     or advertised in the manifest without a matching gate tool.
//   - tools/gate-tools.json — the language-neutral oracle the release smoke
//     (tools/mcp-smoke.py, copied verbatim into the varvig/plugins package)
//     asserts the built binary's live surface against. It is generated from
//     GateTools by GateToolsOracleJSON and checked in; TestGateToolsOracleGolden
//     keeps it fresh.
//
// Before this, the Python `want` set and the manifest were maintained by hand and
// fell behind a gate-tool addition twice. Sourcing both from the one registry the
// gate is drift-tested against extends the U2 guarantee past the Go boundary: a
// tool added to GateTools flows into every surface, and CI fails if one lags.

// oracleTool is one entry in the smoke oracle: the tool name and whether it is a
// write tool, so the smoke can assert the gate's readOnlyHint matches (a write
// tool is not read-only; every read tool is).
type oracleTool struct {
	Name  string `json:"name"`
	Write bool   `json:"write"`
}

// GateToolsOracleJSON renders the smoke oracle (tools/gate-tools.json) from
// GateTools. It is deterministic — registry order, HTML escaping off, a single
// trailing newline — so the checked-in file is byte-stable and the golden test
// is meaningful. Regenerate the checked-in file with
// `go test ./internal/agentrules -run Oracle -update`.
func GateToolsOracleJSON() []byte {
	doc := struct {
		Comment string       `json:"_comment"`
		Tools   []oracleTool `json:"tools"`
	}{
		Comment: "Generated from agentrules.GateTools — do not edit. " +
			"Regenerate: go test ./internal/agentrules -run Oracle -update.",
		Tools: make([]oracleTool, len(GateTools)),
	}
	for i, t := range GateTools {
		doc.Tools[i] = oracleTool{Name: t.Name, Write: t.Write}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	// Encoder.Encode appends the trailing newline; the error is unreachable for
	// this fixed, marshalable document.
	_ = enc.Encode(&doc)
	return buf.Bytes()
}
