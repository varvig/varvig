// Package check runs a repository's declared verification commands over a
// proposed tree and records the result as evidence (build spec P1.3). It is the
// source §1.3's evidence invariant needed and the scheduler's score/scored had
// none: a proposal's quality is a fact produced by running its checks, not a
// number authored from nowhere.
//
// The one invariant that makes evidence trustworthy is the binding (§A4): an
// evidence record names the exact tree hash it was produced against, never the
// proposal id. A proposal id is stable across edits; a tree hash is not. So an
// edit after checking is detectable — evidence whose tree hash differs from the
// proposal's current tree is stale and does not count, and promotion must treat
// stale evidence as absent rather than as a pass. Two checks of the identical
// tree with the identical commands produce equivalent evidence, because the tree
// and the commands are all the inputs.
//
// Evidence is stored as a note on the proposal, in the reserved varvig/check
// namespace, so it pins nothing it should not, syncs to every peer by default
// (reserved namespaces are never opted out of replication), and is read back the
// same way anywhere.
package check

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/notes"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/reserved"
	"github.com/dividebyzero/claude-experiments/varvig/internal/worktree"
)

// maxOutput caps captured output per command so one noisy command cannot bloat
// the object store. Truncation is marked in the recorded output.
const maxOutput = 64 << 10

// defaultTimeout bounds a single command so a hung check cannot wedge the run.
const defaultTimeout = 10 * time.Minute

// CommandResult is the outcome of one declared command: the command verbatim,
// its exit status, its captured combined output, and the hash of the tool binary
// invoked where it could be resolved.
type CommandResult struct {
	Command  string `json:"command"`
	Exit     int    `json:"exit"`
	Output   string `json:"output"`
	ToolHash string `json:"tool_hash,omitempty"`
}

// Evidence is the recorded result of checking one tree. It binds to the tree hash
// checked (§A4), not the proposal id, so an edit after checking is detectable.
type Evidence struct {
	Tree      string          `json:"tree"`   // the proposal tree hash checked
	Change    string          `json:"change"` // the proposal this evidence is a note on
	Results   []CommandResult `json:"results"`
	Passed    bool            `json:"passed"` // every command exited zero
	Timestamp int64           `json:"timestamp"`
}

// Equivalent reports whether two evidence records agree on everything that is a
// function of the inputs — the tree checked and the per-command commands, exit
// statuses, output, and tool hashes. The timestamp is excluded, so two checks of
// the identical tree with the identical commands are equivalent (build spec
// P1.3) even though they ran at different moments.
func (e Evidence) Equivalent(o Evidence) bool {
	if e.Tree != o.Tree || e.Passed != o.Passed || len(e.Results) != len(o.Results) {
		return false
	}
	for i := range e.Results {
		if e.Results[i] != o.Results[i] {
			return false
		}
	}
	return true
}

// Run materializes treeID into a temporary working tree and runs each declared
// command in it, capturing exit status and output. It always returns an Evidence
// record — a failing command is recorded as a failure, never as an absent record
// (build spec P1.3) — so the only error Run returns is a failure to materialize
// the tree or set up the sandbox, not a command exiting non-zero.
//
// Commands are split on whitespace and executed directly (no shell), so a
// declared command is a program and its arguments; the tool binary is resolved on
// PATH and hashed where obtainable. now supplies the timestamp so a caller can
// pin the clock.
func Run(objs *repo.Repo, treeID multihash.Multihash, change multihash.Multihash, commands []string, now int64) (Evidence, error) {
	dir, err := os.MkdirTemp("", "varvig-check-")
	if err != nil {
		return Evidence{}, err
	}
	defer os.RemoveAll(dir)
	if err := worktree.Checkout(objs.Objects, treeID, dir); err != nil {
		return Evidence{}, fmt.Errorf("check: materialize tree: %w", err)
	}

	ev := Evidence{Tree: treeID.Hex(), Change: change.Hex(), Passed: true, Timestamp: now}
	for _, raw := range commands {
		cmdStr := strings.TrimSpace(raw)
		if cmdStr == "" {
			continue
		}
		res := runOne(dir, cmdStr)
		if res.Exit != 0 {
			ev.Passed = false
		}
		ev.Results = append(ev.Results, res)
	}
	// A check that declared no commands has verified nothing; that is not a pass.
	if len(ev.Results) == 0 {
		ev.Passed = false
	}
	return ev, nil
}

func runOne(dir, cmdStr string) CommandResult {
	fields := strings.Fields(cmdStr)
	res := CommandResult{Command: cmdStr}
	if len(fields) == 0 {
		res.Exit = -1
		return res
	}
	if hash, ok := toolHash(fields[0]); ok {
		res.ToolHash = hash
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, fields[0], fields[1:]...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	res.Output = capOutput(buf.Bytes())
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		res.Exit = -1
		res.Output += "\n[varvig check: command timed out]"
	case err == nil:
		res.Exit = 0
	default:
		if ee, ok := err.(*exec.ExitError); ok {
			res.Exit = ee.ExitCode()
		} else {
			res.Exit = -1
			res.Output += "\n[varvig check: " + err.Error() + "]"
		}
	}
	return res
}

// toolHash resolves a command's program on PATH and returns the multihash of its
// bytes, where obtainable — an unresolvable or unreadable program simply yields
// no hash rather than failing the check.
func toolHash(program string) (string, bool) {
	path, err := exec.LookPath(program)
	if err != nil {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	mh, err := multihash.Sum(multihash.SHA2_256, data)
	if err != nil {
		return "", false
	}
	return mh.Hex(), true
}

func capOutput(b []byte) string {
	if len(b) > maxOutput {
		return string(b[:maxOutput]) + "\n[varvig check: output truncated]"
	}
	return string(b)
}

// Attach records evidence as a note on the proposal it checked, in the reserved
// varvig/check namespace. The note target is the proposal change id (so the
// evidence is listable by proposal), while the tree hash the evidence carries is
// what freshness is judged against.
func Attach(r *repo.Repo, ev Evidence) (multihash.Multihash, error) {
	change, err := multihash.ParseHex(ev.Change)
	if err != nil {
		return nil, fmt.Errorf("check: bad change hash: %w", err)
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return nil, err
	}
	return notes.New(r).Add(reserved.NoteCheck, change, payload, "check", ev.Timestamp)
}

// List returns the evidence recorded for a proposal, newest first.
func List(r *repo.Repo, change multihash.Multihash) ([]Evidence, error) {
	chain, err := notes.New(r).List(reserved.NoteCheck, change)
	if err != nil {
		return nil, err
	}
	var out []Evidence
	for _, n := range chain {
		var ev Evidence
		if json.Unmarshal(n.Note.Payload, &ev) == nil {
			out = append(out, ev)
		}
	}
	return out, nil
}

// Fresh reports whether evidence was produced against currentTree — the freshness
// test at the heart of the staleness rule (§A4).
func (e Evidence) Fresh(currentTree multihash.Multihash) bool {
	return e.Tree == currentTree.Hex()
}

// PromotionState is the promotion-time verdict on a proposal's evidence: whether
// fresh passing evidence exists, and what the freshest evidence says if not.
type PromotionState int

const (
	// NoEvidence: the proposal has never been checked. Evidence is opt-in per
	// proposal, so this does not by itself block promotion.
	NoEvidence PromotionState = iota
	// FreshPass: fresh evidence exists for the current tree and passed.
	FreshPass
	// FreshFail: fresh evidence exists for the current tree and a command failed.
	FreshFail
	// Stale: evidence exists but only for a different tree — it must be treated as
	// absent, never as a pass, and promotion names the staleness.
	Stale
)

// Promotion classifies a proposal's evidence against its current tree. It returns
// the state and, for a stale result, the tree hash the stale evidence was
// produced against, so a refusal can name exactly what moved.
func Promotion(r *repo.Repo, change, currentTree multihash.Multihash) (PromotionState, string, error) {
	evs, err := List(r, change)
	if err != nil {
		return NoEvidence, "", err
	}
	if len(evs) == 0 {
		return NoEvidence, "", nil
	}
	// evs is newest-first; prefer the freshest verdict for the current tree.
	for _, ev := range evs {
		if ev.Fresh(currentTree) {
			if ev.Passed {
				return FreshPass, "", nil
			}
			return FreshFail, "", nil
		}
	}
	return Stale, evs[0].Tree, nil
}
