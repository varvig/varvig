package agentrules

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Mode selects what Run does. Only ModeWrite touches disk.
type Mode int

const (
	ModeWrite Mode = iota // default: write VARVIG-AGENTS.md, add pointer once
	ModeCheck             // exit 2 if stale/missing; never writes (the CI entrypoint)
	ModeDiff              // print unified diffs for both files; never writes
	ModePrint             // print generated VARVIG-AGENTS.md to stdout; never writes
)

// Stable diagnostic codes (VRV-AR-###). These strings are read by agents and in
// CI logs, so they are part of the contract.
const (
	CodeOK      = "VRV-AR-000" // success, or already current
	CodeStale   = "VRV-AR-201" // stale or missing (--check)
	CodeNotRepo = "VRV-AR-404" // not a varvig repository
	CodeUsage   = "VRV-AR-500" // usage / unexpected error
)

// Options configures a run. Root is the repository working-tree root; the two
// files live directly under it. RepoPresent is whether Root is a varvig repo —
// the caller determines this (this package never reads or writes .git, nor the
// .varvig metadata beyond what the caller resolved into Facts).
type Options struct {
	Root        string
	Version     string
	Facts       RepoFacts
	Mode        Mode
	RepoPresent bool
}

// Result is the outcome, mirroring the --json shape. Stdout/Stderr carry text
// the CLI is responsible for printing (kept out of this package so it stays
// testable and does no IO of its own beyond the file writes).
type Result struct {
	Code      string            `json:"code"`
	Generated string            `json:"generated"` // created|replaced|current|stale|missing
	Pointer   string            `json:"pointer"`   // added|present|skipped|missing
	Paths     map[string]string `json:"paths"`
	Surface   string            `json:"surface"`
	Exit      int               `json:"exit"`

	Stdout string `json:"-"`
	Stderr string `json:"-"`
}

// JSON renders the machine-readable result. Deterministic: encoding/json sorts
// map keys, and there are no other unordered fields.
func (r Result) JSON() string {
	b, _ := json.Marshal(r)
	return string(b)
}

// Run executes the agent-rules command. It never returns an error for expected
// conditions — every path resolves to a Result with an exit code (nothing here
// can fail a repo init). The bool error return is reserved for truly unexpected
// IO faults during a write.
func Run(opts Options) (Result, error) {
	genPath := filepath.Join(opts.Root, GeneratedName)
	ptrPath := filepath.Join(opts.Root, PointerName)
	paths := map[string]string{"generated": genPath, "pointer": ptrPath}

	// Nothing to do — and nothing to describe — if this is not a repo.
	if !opts.RepoPresent {
		return Result{
			Code: CodeNotRepo, Exit: 4, Paths: paths,
			Stderr: CodeNotRepo + ": not a varvig repository\n" +
				"  why: the rules files describe how to drive varvig in this repo, and there is no repo here.\n" +
				"  do:  run `varvig init` first, or run this from inside a varvig repository.\n",
		}, nil
	}

	content, surface := Generate(opts.Version, opts.Facts)
	existingGen, genExists := readFileString(genPath)
	existingPtr, ptrExists := readFileString(ptrPath)

	switch opts.Mode {
	case ModePrint:
		return Result{Code: CodeOK, Exit: 0, Surface: surface, Paths: paths,
			Generated: genState(genExists, existingGen, content),
			Pointer:   ptrState(ptrExists, existingPtr),
			Stdout:    content}, nil

	case ModeCheck:
		return runCheck(surface, genExists, existingGen, ptrExists, existingPtr, paths), nil

	case ModeDiff:
		gd := unifiedDiff(GeneratedName, existingGen, content)
		var pd string
		if newPtr, changed := plannedPointer(ptrExists, existingPtr); changed {
			pd = unifiedDiff(PointerName, existingPtr, newPtr)
		}
		out := gd
		if pd != "" {
			if out != "" {
				out += "\n"
			}
			out += pd
		}
		if out == "" {
			out = "both files are current; no diff\n"
		}
		return Result{Code: CodeOK, Exit: 0, Surface: surface, Paths: paths,
			Generated: genState(genExists, existingGen, content),
			Pointer:   ptrState(ptrExists, existingPtr),
			Stdout:    out}, nil

	default: // ModeWrite
		return runWrite(opts, content, surface, genPath, ptrPath, paths,
			genExists, existingGen, ptrExists, existingPtr)
	}
}

func runCheck(surface string, genExists bool, existingGen string, ptrExists bool, existingPtr string, paths map[string]string) Result {
	genStale := !genExists || readSurface(existingGen) != surface
	ptrMissing := !(ptrExists && hasPointer(existingPtr))

	genField := "current"
	if !genExists {
		genField = "missing"
	} else if genStale {
		genField = "stale"
	}
	ptrField := "present"
	if !ptrExists {
		ptrField = "missing"
	} else if ptrMissing {
		ptrField = "missing"
	}

	if genStale || ptrMissing {
		var sb strings.Builder
		sb.WriteString(CodeStale + ": agent rules are stale or missing\n")
		if !genExists {
			fmt.Fprintf(&sb, "  - %s is missing\n", GeneratedName)
		} else if genStale {
			fmt.Fprintf(&sb, "  - %s surface %q != current %q\n", GeneratedName, readSurface(existingGen), surface)
		}
		if ptrMissing {
			fmt.Fprintf(&sb, "  - %s has no pointer to %s\n", PointerName, GeneratedName)
		}
		sb.WriteString("  do: run `varvig init --agent-rules` to regenerate.\n")
		return Result{Code: CodeStale, Exit: 2, Surface: surface, Paths: paths,
			Generated: genField, Pointer: ptrField, Stderr: sb.String()}
	}
	return Result{Code: CodeOK, Exit: 0, Surface: surface, Paths: paths,
		Generated: "current", Pointer: "present"}
}

func runWrite(opts Options, content, surface, genPath, ptrPath string, paths map[string]string,
	genExists bool, existingGen string, ptrExists bool, existingPtr string) (Result, error) {

	res := Result{Code: CodeOK, Exit: 0, Surface: surface, Paths: paths}

	// VARVIG-AGENTS.md: overwrite always, but skip the write when already
	// byte-identical (idempotency) and warn — to stderr, with the diff — when we
	// are replacing local edits.
	switch {
	case !genExists:
		if err := writeAtomic(genPath, []byte(content)); err != nil {
			return usageError(paths, err), err
		}
		res.Generated = "created"
	case existingGen == content:
		res.Generated = "current"
	default:
		res.Generated = "replaced"
		// Distinguish a clean older-version file (only the version header differs;
		// bodies match) from a genuinely edited one. The body is version-
		// independent, so equal bodies mean nothing was hand-edited.
		if bodyOf(existingGen) == bodyOf(content) {
			res.Stderr += "note: regenerated " + GeneratedName + " (version header updated).\n"
		} else {
			res.Stderr += "note: " + GeneratedName + " had local edits; they were replaced.\n" +
				"      Generated file — put team rules in " + PointerName + " instead.\n" +
				"      Diff of what was replaced:\n" +
				indent(unifiedDiff(GeneratedName, existingGen, content), "      ")
		}
		if err := writeAtomic(genPath, []byte(content)); err != nil {
			return usageError(paths, err), err
		}
	}

	// AGENTS.md: append the pointer exactly once; never rewrite.
	switch {
	case !ptrExists:
		if err := writeAtomic(ptrPath, []byte(PointerBlock())); err != nil {
			return usageError(paths, err), err
		}
		res.Pointer = "added"
	case strings.Contains(existingPtr, pointerMarker):
		res.Pointer = "present"
	case strings.Contains(existingPtr, GeneratedName):
		// A hand-written pointer already mentions the file; adding ours would be
		// duplicate clutter. Leave it untouched.
		res.Pointer = "skipped"
	default:
		if err := writeAtomic(ptrPath, []byte(appendPointer(existingPtr))); err != nil {
			return usageError(paths, err), err
		}
		res.Pointer = "added"
	}

	return res, nil
}

// plannedPointer returns what AGENTS.md would become and whether that differs
// from the current contents — used by --diff without writing.
func plannedPointer(ptrExists bool, existing string) (string, bool) {
	switch {
	case !ptrExists:
		return PointerBlock(), true
	case hasPointer(existing):
		return existing, false
	default:
		return appendPointer(existing), true
	}
}

// appendPointer appends the pointer block at the end of an existing file,
// separated by exactly one blank line, adding a trailing newline first if the
// file lacks one. An empty or whitespace-only file becomes just the block.
func appendPointer(existing string) string {
	if strings.TrimSpace(existing) == "" {
		return PointerBlock()
	}
	s := existing
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return s + "\n" + PointerBlock()
}

// genState/ptrState describe the would-be write outcome for non-writing modes.
func genState(exists bool, existing, content string) string {
	switch {
	case !exists:
		return "created"
	case existing == content:
		return "current"
	default:
		return "replaced"
	}
}

func ptrState(exists bool, existing string) string {
	switch {
	case !exists:
		return "added"
	case strings.Contains(existing, pointerMarker):
		return "present"
	case strings.Contains(existing, GeneratedName):
		return "skipped"
	default:
		return "added"
	}
}

func usageError(paths map[string]string, err error) Result {
	return Result{Code: CodeUsage, Exit: 1, Paths: paths,
		Stderr: fmt.Sprintf("%s: %v\n", CodeUsage, err)}
}

func indent(s, pad string) string {
	if s == "" {
		return s
	}
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = pad + l
	}
	return strings.Join(lines, "\n") + "\n"
}

// --- IO ---

func readFileString(path string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// writeAtomic writes data to path via a temp file in the same directory, fsync,
// and rename — so an interrupted write can never leave a half-written or
// truncated file in place; the prior file survives until the rename succeeds.
func writeAtomic(path string, data []byte) error {
	tmp, err := writeTemp(path, data)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// writeTemp writes data to a fresh temp file beside path and returns its name
// without renaming. Split out from writeAtomic so the atomicity invariant — the
// destination is untouched until the rename — is directly testable.
func writeTemp(path string, data []byte) (string, error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".varvig-agents-*.tmp")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}
