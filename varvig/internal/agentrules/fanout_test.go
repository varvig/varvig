package agentrules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	t.Fatalf("stat %s: %v", path, err)
	return false
}

func TestFanoutLinksExistingToolFilesOnly(t *testing.T) {
	dir := t.TempDir()
	claude := "# Claude rules\n\nBe careful.\n"
	mustWrite(t, filepath.Join(dir, "CLAUDE.md"), claude)
	mustWrite(t, filepath.Join(dir, ".windsurfrules"), "windsurf stuff\n")

	res := runIn(t, dir, ModeWrite, RepoFacts{})

	// Existing tool files get the pointer appended, prior bytes intact.
	if res.Fanout["CLAUDE.md"] != "added" || res.Fanout[".windsurfrules"] != "added" {
		t.Fatalf("fanout=%v; want CLAUDE.md and .windsurfrules added", res.Fanout)
	}
	got := read(t, filepath.Join(dir, "CLAUDE.md"))
	if !strings.HasPrefix(got, claude) || !strings.Contains(got, pointerMarker) {
		t.Fatalf("CLAUDE.md not appended correctly:\n%s", got)
	}
	// Tool files that did not exist are NOT created — no presuming a tool.
	if exists(t, filepath.Join(dir, "GEMINI.md")) {
		t.Error("GEMINI.md should not be created when absent")
	}
	if exists(t, filepath.Join(dir, ".cursorrules")) {
		t.Error(".cursorrules should not be created when absent")
	}
	if _, ok := res.Fanout["GEMINI.md"]; ok {
		t.Error("absent tool files should not appear in fanout")
	}
}

func TestFanoutCursorRulesDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".cursor", "rules"), 0o755); err != nil {
		t.Fatal(err)
	}
	res := runIn(t, dir, ModeWrite, RepoFacts{})

	rel := filepath.Join(".cursor", "rules", "varvig.mdc")
	if res.Fanout[rel] != "added" {
		t.Fatalf("expected %s added, fanout=%v", rel, res.Fanout)
	}
	got := read(t, filepath.Join(dir, rel))
	if !strings.HasPrefix(got, "---\n") || !strings.Contains(got, "alwaysApply: true") {
		t.Fatalf("cursor mdc missing frontmatter:\n%s", got)
	}
	if !strings.Contains(got, GeneratedName) {
		t.Fatal("cursor mdc should point at the generated file")
	}
	// Legacy .cursorrules is not created just because the dir form is used.
	if exists(t, filepath.Join(dir, ".cursorrules")) {
		t.Error(".cursorrules should not be created")
	}
}

func TestFanoutCopilotAppendOnlyIfExists(t *testing.T) {
	// .github exists but no copilot file: we do not create one.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".github"), 0o755); err != nil {
		t.Fatal(err)
	}
	res := runIn(t, dir, ModeWrite, RepoFacts{})
	copilot := filepath.Join(dir, ".github", "copilot-instructions.md")
	if exists(t, copilot) {
		t.Fatal("copilot-instructions.md should not be created")
	}
	if _, ok := res.Fanout[filepath.Join(".github", "copilot-instructions.md")]; ok {
		t.Fatal("absent copilot file should not be in fanout")
	}

	// Now it exists: we append.
	mustWrite(t, copilot, "# copilot\n")
	res = runIn(t, dir, ModeWrite, RepoFacts{})
	if res.Fanout[filepath.Join(".github", "copilot-instructions.md")] != "added" {
		t.Fatalf("copilot file should be linked when present, fanout=%v", res.Fanout)
	}
	if !strings.Contains(read(t, copilot), pointerMarker) {
		t.Fatal("copilot file should carry the pointer")
	}
}

func TestFanoutSkipsHandWrittenMention(t *testing.T) {
	dir := t.TempDir()
	prior := "# Claude\n\nsee ./" + GeneratedName + "\n"
	mustWrite(t, filepath.Join(dir, "CLAUDE.md"), prior)

	res := runIn(t, dir, ModeWrite, RepoFacts{})
	if res.Fanout["CLAUDE.md"] != "skipped" {
		t.Fatalf("CLAUDE.md=%q, want skipped", res.Fanout["CLAUDE.md"])
	}
	if read(t, filepath.Join(dir, "CLAUDE.md")) != prior {
		t.Fatal("hand-written mention must be left untouched")
	}
}

func TestFanoutIdempotent(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "CLAUDE.md"), "# c\n")
	runIn(t, dir, ModeWrite, RepoFacts{})
	before := read(t, filepath.Join(dir, "CLAUDE.md"))
	res := runIn(t, dir, ModeWrite, RepoFacts{})
	if res.Fanout["CLAUDE.md"] != "present" {
		t.Fatalf("second run CLAUDE.md=%q, want present", res.Fanout["CLAUDE.md"])
	}
	if read(t, filepath.Join(dir, "CLAUDE.md")) != before {
		t.Fatal("second run modified CLAUDE.md")
	}
}

func TestCheckFlagsExistingToolFileMissingPointer(t *testing.T) {
	dir := t.TempDir()
	runIn(t, dir, ModeWrite, RepoFacts{}) // AGENTS.md + VARVIG-AGENTS.md current
	// A CLAUDE.md appears later with no pointer — a silent gap for Claude agents.
	mustWrite(t, filepath.Join(dir, "CLAUDE.md"), "# c\n")

	res := runIn(t, dir, ModeCheck, RepoFacts{})
	if res.Exit != 2 || res.Code != CodeStale {
		t.Fatalf("exit=%d code=%s, want 2/%s", res.Exit, res.Code, CodeStale)
	}
	if !strings.Contains(res.Stderr, "CLAUDE.md has no pointer") {
		t.Fatalf("check should flag CLAUDE.md, got:\n%s", res.Stderr)
	}
	if res.Fanout["CLAUDE.md"] != "missing" {
		t.Fatalf("fanout CLAUDE.md=%q, want missing", res.Fanout["CLAUDE.md"])
	}
}
