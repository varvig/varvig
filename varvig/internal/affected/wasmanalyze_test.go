package affected

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/hook"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
)

// rubyAnalyzerSrc is a wasm analyzer for a Ruby-like language: it reads the
// {path, content} JSON on stdin and emits each `require_relative "X"` target as
// a path specifier. It demonstrates §3.3 — a new language learned without
// recompiling Varvig.
const rubyAnalyzerSrc = `package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"regexp"
)

type in struct {
	Path    string ` + "`json:\"path\"`" + `
	Content string ` + "`json:\"content\"`" + `
}

func main() {
	b, _ := io.ReadAll(os.Stdin)
	var i in
	json.Unmarshal(b, &i)
	c, _ := base64.StdEncoding.DecodeString(i.Content)
	re := regexp.MustCompile(` + "`" + `require_relative\s+['"]([^'"]+)['"]` + "`" + `)
	for _, m := range re.FindAllStringSubmatch(string(c), -1) {
		os.Stdout.WriteString(m[1] + "\n")
	}
}
`

func buildWASI(t *testing.T, src string) []byte {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module a\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "m.wasm")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build wasm fixture: %v\n%s", err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestWasmAnalyzer(t *testing.T) {
	module := buildWASI(t, rubyAnalyzerSrc)
	id, _ := multihash.Sum(multihash.BLAKE3, module)

	r, tree := treeFromFiles(t, map[string]string{
		"app.rb":  "require_relative \"./lib\"\nputs Lib.hello\n",
		"lib.rb":  "module Lib\nend\n",
		"solo.rb": "puts 1\n",
	})

	wa := WasmAnalyzer{
		Ext:    ".rb",
		Module: module,
		ID:     id,
		Run: func(ctx context.Context, mod, input []byte) ([]byte, error) {
			res, err := hook.Run(ctx, mod, input)
			if err != nil {
				return nil, err
			}
			return res.Stdout, nil
		},
	}

	g, err := BuildGraph(r.Objects, tree, Options{Wasm: []WasmAnalyzer{wa}})
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	if got := g.Deps("app.rb"); !reflect.DeepEqual(got, []string{"lib.rb"}) {
		t.Fatalf("app.rb deps = %v, want [lib.rb]", got)
	}
	got := g.Affected([]string{"lib.rb"})
	if !reflect.DeepEqual(got, []string{"app.rb", "lib.rb"}) {
		t.Fatalf("affected = %v, want [app.rb lib.rb]", got)
	}
	if contains(got, "solo.rb") {
		t.Fatal("unrelated .rb wrongly affected")
	}
}

func TestWasmAnalyzerCacheKeyedByModule(t *testing.T) {
	module := buildWASI(t, rubyAnalyzerSrc)
	id, _ := multihash.Sum(multihash.BLAKE3, module)
	r, tree := treeFromFiles(t, map[string]string{
		"app.rb": "require_relative \"./lib\"\n",
		"lib.rb": "x\n",
	})
	cache := NewMemCache()
	run := func(ctx context.Context, mod, input []byte) ([]byte, error) {
		res, err := hook.Run(ctx, mod, input)
		if err != nil {
			return nil, err
		}
		return res.Stdout, nil
	}
	wa := WasmAnalyzer{Ext: ".rb", Module: module, ID: id, Run: run}

	// First build populates the cache under the combined (blob, module) key.
	if _, err := BuildGraph(r.Objects, tree, Options{Cache: cache, Wasm: []WasmAnalyzer{wa}}); err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	files, err := FlattenTree(r.Objects, tree)
	if err != nil {
		t.Fatalf("FlattenTree: %v", err)
	}
	blobID := files["app.rb"]
	if _, ok := cache.Get(combinedKey(blobID, id)); !ok {
		t.Fatal("wasm analysis not cached under the combined key")
	}
	// The plain blob key must be untouched, so a builtin re-analysis of the same
	// content would not collide with the wasm result.
	if _, ok := cache.Get(blobID); ok {
		t.Fatal("wasm result leaked into the plain blob cache key")
	}
}
