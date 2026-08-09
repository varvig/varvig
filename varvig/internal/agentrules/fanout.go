package agentrules

import (
	"os"
	"path/filepath"
	"strings"
)

// Multi-tool fanout: several agent harnesses each read their own rules file
// (AGENTS.md, CLAUDE.md, Cursor/Windsurf/Copilot configs). Rather than copy the
// rules into each — copies diverge — every one gets the same short pointer block
// aimed at the single canonical generated file. That is the whole point of the
// pointer design: one source of truth, many doors to it.

// PointerTarget is one rules file that should carry a pointer to VARVIG-AGENTS.md.
type PointerTarget struct {
	// Rel is the path relative to the repo root (may include a subdirectory).
	Rel string
	// Primary marks AGENTS.md, the cross-tool standard. Its status is reported
	// as the top-level `pointer`; the rest go under `fanout`.
	Primary bool
	// Ensure creates the file when absent. Only files we are willing to presume
	// (AGENTS.md; a Cursor rule when its rules dir already exists) set this — we
	// never create a tool's config just to guess the user uses that tool.
	Ensure bool
	// Fresh is the content written when the file is created from scratch.
	Fresh string
}

// fanoutTargets is the ordered set of pointer files for a repo. AGENTS.md is
// always ensured. Tool-specific files are linked only when they already exist,
// so a repo that does not use Cursor never grows a `.cursorrules`. The one
// exception is Cursor's per-topic rules directory: when `.cursor/rules/` exists
// the user clearly uses Cursor, and a dedicated rule file is the idiom there, so
// we add one.
func fanoutTargets(root string) []PointerTarget {
	ts := []PointerTarget{
		{Rel: PointerName, Primary: true, Ensure: true, Fresh: PointerBlock()},
		{Rel: "CLAUDE.md", Fresh: PointerBlock()},
		{Rel: "GEMINI.md", Fresh: PointerBlock()},
		{Rel: ".cursorrules", Fresh: PointerBlock()},
		{Rel: ".windsurfrules", Fresh: PointerBlock()},
		{Rel: filepath.Join(".github", "copilot-instructions.md"), Fresh: PointerBlock()},
	}
	if dirExists(filepath.Join(root, ".cursor", "rules")) {
		ts = append(ts, PointerTarget{
			Rel: filepath.Join(".cursor", "rules", "varvig.mdc"), Ensure: true, Fresh: mdcPointer(),
		})
	}
	return ts
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// planPointer decides the outcome for one target from its current state, using
// the same detection everywhere: our marker or any literal mention of the
// generated file means "already pointed" and the file is left untouched.
//
//	added   — created fresh, or the pointer appended to an existing file
//	present — our marker is already there
//	skipped — a hand-written mention already links the file; do not duplicate
func planPointer(exists bool, existing, fresh string) (status, content string, write bool) {
	switch {
	case !exists:
		return "added", fresh, true
	case strings.Contains(existing, pointerMarker):
		return "present", "", false
	case strings.Contains(existing, GeneratedName):
		return "skipped", "", false
	default:
		return "added", appendPointer(existing), true
	}
}
