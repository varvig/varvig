package affected

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/dividebyzero/claude-experiments/loom/internal/multihash"
)

// wasmInput is the JSON a wasm analyzer receives on stdin.
type wasmInput struct {
	Path    string `json:"path"`
	Content string `json:"content"` // base64-encoded file bytes
}

// runWasmAnalyzer feeds a file to a wasm analyzer and parses its emitted
// specifiers. The module writes one specifier per line to stdout: a line
// beginning "package " is a package import; anything else is a path import.
// Output is content-only (no repo resolution), so results cache soundly.
func runWasmAnalyzer(objs ObjectStore, p string, blobID multihash.Multihash, wa *WasmAnalyzer) ([]Specifier, error) {
	obj, err := objs.Get(blobID)
	if err != nil {
		return nil, err
	}
	content, _ := obj.BlobContent()
	in, err := json.Marshal(wasmInput{Path: p, Content: base64.StdEncoding.EncodeToString(content)})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, err := wa.Run(ctx, wa.Module, in)
	if err != nil {
		return nil, err
	}
	return parseWasmSpecs(out), nil
}

func parseWasmSpecs(out []byte) []Specifier {
	var specs []Specifier
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "package "); ok {
			specs = append(specs, Specifier{Kind: SpecPackage, Value: strings.TrimSpace(rest)})
		} else {
			specs = append(specs, Specifier{Kind: SpecPath, Value: line})
		}
	}
	return specs
}
