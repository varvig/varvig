package agentrules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testVersion = "v0.0.0-test"

// runIn is a convenience wrapper: run against a temp root with RepoPresent set.
func runIn(t *testing.T, root string, mode Mode, facts RepoFacts) Result {
	t.Helper()
	res, err := Run(Options{Root: root, Version: testVersion, Mode: mode, RepoPresent: true, Facts: facts})
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	return res
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunTwiceIsByteIdenticalNoWrite(t *testing.T) {
	dir := t.TempDir()
	gen := filepath.Join(dir, GeneratedName)
	ptr := filepath.Join(dir, PointerName)

	r1 := runIn(t, dir, ModeWrite, RepoFacts{})
	if r1.Generated != "created" || r1.Pointer != "added" {
		t.Fatalf("first run: generated=%q pointer=%q", r1.Generated, r1.Pointer)
	}
	genBefore, ptrBefore := read(t, gen), read(t, ptr)
	genInfo, _ := os.Stat(gen)

	r2 := runIn(t, dir, ModeWrite, RepoFacts{})
	if r2.Generated != "current" || r2.Pointer != "present" {
		t.Fatalf("second run should be a no-op: generated=%q pointer=%q", r2.Generated, r2.Pointer)
	}
	if read(t, gen) != genBefore || read(t, ptr) != ptrBefore {
		t.Fatal("second run changed file contents")
	}
	if info, _ := os.Stat(gen); !info.ModTime().Equal(genInfo.ModTime()) {
		t.Fatal("second run rewrote VARVIG-AGENTS.md (mtime changed); it should skip the write")
	}
}

func TestNoAgentsFileCreatesPointerOnly(t *testing.T) {
	dir := t.TempDir()
	res := runIn(t, dir, ModeWrite, RepoFacts{})
	if res.Pointer != "added" {
		t.Fatalf("pointer=%q, want added", res.Pointer)
	}
	got := read(t, filepath.Join(dir, PointerName))
	if got != PointerBlock() {
		t.Fatalf("AGENTS.md should contain only the pointer block, got:\n%s", got)
	}
}

func TestExistingAgentsPointerAppendedPriorBytesIntact(t *testing.T) {
	dir := t.TempDir()
	prior := "# My rules\n\nDo the thing.\n"
	mustWrite(t, filepath.Join(dir, PointerName), prior)

	res := runIn(t, dir, ModeWrite, RepoFacts{})
	if res.Pointer != "added" {
		t.Fatalf("pointer=%q, want added", res.Pointer)
	}
	got := read(t, filepath.Join(dir, PointerName))
	if !strings.HasPrefix(got, prior) {
		t.Fatalf("prior bytes not preserved verbatim at the head:\n%q", got)
	}
	if !strings.Contains(got, pointerMarker) {
		t.Fatal("pointer marker not appended")
	}
	// Exactly one blank line between prior content and the block.
	if !strings.HasPrefix(got, prior+"\n"+pointerMarker) {
		t.Fatalf("expected exactly one blank line before the block, got:\n%q", got)
	}
}

func TestPointerMarkerUntouchedAcrossUpgrade(t *testing.T) {
	dir := t.TempDir()
	// v1 writes both files.
	if _, err := Run(Options{Root: dir, Version: "v1.0.0", Mode: ModeWrite, RepoPresent: true}); err != nil {
		t.Fatal(err)
	}
	ptrPath := filepath.Join(dir, PointerName)
	genPath := filepath.Join(dir, GeneratedName)
	ptrV1, genV1 := read(t, ptrPath), read(t, genPath)

	// v2 upgrade.
	res, err := Run(Options{Root: dir, Version: "v2.0.0", Mode: ModeWrite, RepoPresent: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Pointer != "present" {
		t.Fatalf("pointer=%q, want present (never rewritten)", res.Pointer)
	}
	if read(t, ptrPath) != ptrV1 {
		t.Fatal("AGENTS.md was modified on upgrade; it must be written once and never touched")
	}
	if read(t, genPath) == genV1 {
		t.Fatal("VARVIG-AGENTS.md should change across a version upgrade (header version)")
	}
	if res.Generated != "replaced" {
		t.Fatalf("generated=%q, want replaced", res.Generated)
	}
}

func TestHandWrittenMentionNotDuplicated(t *testing.T) {
	dir := t.TempDir()
	prior := "## agents\n\nSee ./" + GeneratedName + " for rules.\n"
	mustWrite(t, filepath.Join(dir, PointerName), prior)

	res := runIn(t, dir, ModeWrite, RepoFacts{})
	if res.Pointer != "skipped" {
		t.Fatalf("pointer=%q, want skipped (mention already present)", res.Pointer)
	}
	if got := read(t, filepath.Join(dir, PointerName)); got != prior {
		t.Fatalf("AGENTS.md must be left untouched, got:\n%s", got)
	}
}

func TestNoTrailingNewlineGetsOneNoMangle(t *testing.T) {
	dir := t.TempDir()
	prior := "line one\nline two" // no trailing newline
	mustWrite(t, filepath.Join(dir, PointerName), prior)

	runIn(t, dir, ModeWrite, RepoFacts{})
	got := read(t, filepath.Join(dir, PointerName))
	if !strings.HasPrefix(got, "line one\nline two\n\n"+pointerMarker) {
		t.Fatalf("newline handling wrong; got:\n%q", got)
	}
}

func TestEmptyAndWhitespaceAgentsFile(t *testing.T) {
	for name, prior := range map[string]string{"empty": "", "whitespace": "  \n\t\n"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			mustWrite(t, filepath.Join(dir, PointerName), prior)
			res := runIn(t, dir, ModeWrite, RepoFacts{})
			if res.Pointer != "added" {
				t.Fatalf("pointer=%q, want added", res.Pointer)
			}
			if got := read(t, filepath.Join(dir, PointerName)); got != PointerBlock() {
				t.Fatalf("want just the pointer block, got:\n%q", got)
			}
		})
	}
}

func TestHandEditedGeneratedOverwrittenWithDiff(t *testing.T) {
	dir := t.TempDir()
	runIn(t, dir, ModeWrite, RepoFacts{}) // create clean
	genPath := filepath.Join(dir, GeneratedName)
	// Introduce a genuine body edit.
	edited := strings.Replace(read(t, genPath), "You hold **propose-only**", "You may do ANYTHING", 1)
	mustWrite(t, genPath, edited)

	res := runIn(t, dir, ModeWrite, RepoFacts{})
	if res.Generated != "replaced" {
		t.Fatalf("generated=%q, want replaced", res.Generated)
	}
	if res.Exit != 0 {
		t.Fatalf("exit=%d, want 0 (never fails init)", res.Exit)
	}
	if !strings.Contains(res.Stderr, "had local edits") || !strings.Contains(res.Stderr, "Diff of what was replaced") {
		t.Fatalf("expected local-edit notice + diff on stderr, got:\n%s", res.Stderr)
	}
	if read(t, genPath) == edited {
		t.Fatal("hand-edited file should have been overwritten")
	}
}

func TestCheckMissingIsStale(t *testing.T) {
	dir := t.TempDir()
	res := runIn(t, dir, ModeCheck, RepoFacts{})
	if res.Exit != 2 || res.Code != CodeStale {
		t.Fatalf("missing files under --check: exit=%d code=%s, want 2/%s", res.Exit, res.Code, CodeStale)
	}
	if _, err := os.Stat(filepath.Join(dir, GeneratedName)); !os.IsNotExist(err) {
		t.Fatal("--check must not write VARVIG-AGENTS.md")
	}
	if _, err := os.Stat(filepath.Join(dir, PointerName)); !os.IsNotExist(err) {
		t.Fatal("--check must not write AGENTS.md")
	}
}

func TestCheckStaleSurface(t *testing.T) {
	dir := t.TempDir()
	runIn(t, dir, ModeWrite, RepoFacts{})
	genPath := filepath.Join(dir, GeneratedName)
	tampered := strings.Replace(read(t, genPath), "surface ", "surface dead", 1) // corrupt the token
	mustWrite(t, genPath, tampered)

	res := runIn(t, dir, ModeCheck, RepoFacts{})
	if res.Exit != 2 || res.Code != CodeStale {
		t.Fatalf("stale surface: exit=%d code=%s, want 2/%s", res.Exit, res.Code, CodeStale)
	}
}

func TestNotARepoExit4(t *testing.T) {
	res, err := Run(Options{Root: t.TempDir(), Version: testVersion, Mode: ModeCheck, RepoPresent: false})
	if err != nil {
		t.Fatal(err)
	}
	if res.Exit != 4 || res.Code != CodeNotRepo {
		t.Fatalf("exit=%d code=%s, want 4/%s", res.Exit, res.Code, CodeNotRepo)
	}
}

func TestInspectionModesNeverWrite(t *testing.T) {
	for _, mode := range []Mode{ModeCheck, ModeDiff, ModePrint} {
		dir := t.TempDir()
		runIn(t, dir, mode, RepoFacts{})
		for _, name := range []string{GeneratedName, PointerName} {
			if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
				t.Fatalf("mode %d wrote %s; inspection modes must not write", mode, name)
			}
		}
	}
}

func TestAcceptanceGatesRenderAndTODO(t *testing.T) {
	// No gates -> explicit TODO, never a silent omission.
	c0, _ := Generate(testVersion, RepoFacts{})
	if !strings.Contains(c0, "## Acceptance gates") || !strings.Contains(c0, "TODO:") {
		t.Fatal("empty gates must render an explicit TODO section")
	}
	// Gates present -> listed, no TODO.
	c1, _ := Generate(testVersion, RepoFacts{AcceptanceGates: []string{"pre-commit"}, TestCommand: "go test ./..."})
	if strings.Contains(c1, "TODO:") {
		t.Fatal("configured gates should not render a TODO")
	}
	if !strings.Contains(c1, "gate: `pre-commit`") || !strings.Contains(c1, "tests: `go test ./...`") {
		t.Fatalf("gates/tests not rendered:\n%s", c1)
	}
}

func TestInterruptedWriteLeavesPriorIntact(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, GeneratedName)
	mustWrite(t, dst, "OLD CONTENT\n")

	// Simulate a crash between temp write and rename: writeTemp returns before
	// the rename, so the destination must still hold the prior bytes.
	tmp, err := writeTemp(dst, []byte("NEW CONTENT\n"))
	if err != nil {
		t.Fatal(err)
	}
	if read(t, dst) != "OLD CONTENT\n" {
		t.Fatal("destination was modified before rename; atomicity violated")
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Fatalf("temp file should exist: %v", err)
	}
}

func TestJSONShape(t *testing.T) {
	dir := t.TempDir()
	res := runIn(t, dir, ModeWrite, RepoFacts{})
	js := res.JSON()
	for _, want := range []string{`"code":"VRV-AR-000"`, `"generated":"created"`, `"pointer":"added"`, `"surface":`, `"exit":0`} {
		if !strings.Contains(js, want) {
			t.Errorf("json missing %s: %s", want, js)
		}
	}
}
