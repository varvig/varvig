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
	Pointer   string            `json:"pointer"`   // AGENTS.md: added|present|skipped|missing
	Fanout    map[string]string `json:"fanout,omitempty"`
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
		return runCheck(opts.Root, surface, genExists, existingGen, paths), nil

	case ModeDiff:
		out := unifiedDiff(GeneratedName, existingGen, content)
		for _, tgt := range fanoutTargets(opts.Root) {
			existing, exists := readFileString(filepath.Join(opts.Root, tgt.Rel))
			if !exists && !tgt.Ensure {
				continue // a tool file we would not create; nothing to diff
			}
			_, planned, write := planPointer(exists, existing, tgt.Fresh)
			if !write {
				continue
			}
			d := unifiedDiff(tgt.Rel, existing, planned)
			if d != "" {
				if out != "" {
					out += "\n"
				}
				out += d
			}
		}
		if out == "" {
			out = "all files are current; no diff\n"
		}
		return Result{Code: CodeOK, Exit: 0, Surface: surface, Paths: paths,
			Generated: genState(genExists, existingGen, content),
			Pointer:   ptrState(ptrExists, existingPtr),
			Stdout:    out}, nil

	default: // ModeWrite
		return runWrite(opts, content, surface, genPath, paths, genExists, existingGen)
	}
}

func runCheck(root, surface string, genExists bool, existingGen string, paths map[string]string) Result {
	genStale := !genExists || readSurface(existingGen) != surface

	genField := "current"
	if !genExists {
		genField = "missing"
	} else if genStale {
		genField = "stale"
	}

	var problems []string
	if !genExists {
		problems = append(problems, fmt.Sprintf("%s is missing", GeneratedName))
	} else if genStale {
		problems = append(problems, fmt.Sprintf("%s surface %q != current %q",
			GeneratedName, readSurface(existingGen), surface))
	}

	// Pointer + fanout: AGENTS.md must carry a pointer; any tool file that
	// exists must too (an existing CLAUDE.md with no pointer means a Claude agent
	// gets no varvig rules — exactly the silent failure this guards against).
	ptrField := "present"
	fanout := map[string]string{}
	for _, tgt := range fanoutTargets(root) {
		existing, exists := readFileString(filepath.Join(root, tgt.Rel))
		switch {
		case !exists && tgt.Primary:
			ptrField = "missing"
			problems = append(problems, fmt.Sprintf("%s is missing", tgt.Rel))
		case !exists:
			// A tool file we would not create; its absence is not a problem.
			continue
		case hasPointer(existing):
			if !tgt.Primary {
				fanout[tgt.Rel] = "present"
			}
		default:
			problems = append(problems, fmt.Sprintf("%s has no pointer to %s", tgt.Rel, GeneratedName))
			if tgt.Primary {
				ptrField = "missing"
			} else {
				fanout[tgt.Rel] = "missing"
			}
		}
	}
	if len(fanout) == 0 {
		fanout = nil
	}

	if len(problems) > 0 {
		var sb strings.Builder
		sb.WriteString(CodeStale + ": agent rules are stale or missing\n")
		for _, p := range problems {
			fmt.Fprintf(&sb, "  - %s\n", p)
		}
		sb.WriteString("  do: run `varvig init --agent-rules` to regenerate.\n")
		return Result{Code: CodeStale, Exit: 2, Surface: surface, Paths: paths,
			Generated: genField, Pointer: ptrField, Fanout: fanout, Stderr: sb.String()}
	}
	return Result{Code: CodeOK, Exit: 0, Surface: surface, Paths: paths,
		Generated: "current", Pointer: "present", Fanout: fanout}
}

func runWrite(opts Options, content, surface, genPath string, paths map[string]string,
	genExists bool, existingGen string) (Result, error) {

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

	// Pointers: append the pointer exactly once to each rules file, never
	// rewriting. AGENTS.md (and a Cursor rule when its dir exists) are created if
	// absent; other tool files are linked only when they already exist.
	res.Fanout = map[string]string{}
	for _, tgt := range fanoutTargets(opts.Root) {
		p := filepath.Join(opts.Root, tgt.Rel)
		existing, exists := readFileString(p)
		if !exists && !tgt.Ensure {
			continue // never create a tool's config just to guess it is used
		}
		status, toWrite, write := planPointer(exists, existing, tgt.Fresh)
		if write {
			if err := writeAtomic(p, []byte(toWrite)); err != nil {
				return usageError(paths, err), err
			}
		}
		if tgt.Primary {
			res.Pointer = status
		} else {
			res.Fanout[tgt.Rel] = status
		}
	}
	if len(res.Fanout) == 0 {
		res.Fanout = nil
	}

	return res, nil
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
