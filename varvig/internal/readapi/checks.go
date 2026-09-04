package readapi

import (
	"github.com/dividebyzero/claude-experiments/varvig/internal/check"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
)

// Verification evidence (build spec P1.3) is recorded by `varvig check` as a note
// on a proposal, and it binds to the **tree hash** it was produced against, never
// the proposal id — a proposal id is stable across edits, a tree hash is not. So
// the read surface reports, per evidence record, whether it is `current`: whether
// its tree still matches the change's tree. Stale evidence (an edit after
// checking) is visible but does not read as a pass.

// CheckCommand is one command's result within an evidence record. The full
// command output is intentionally omitted here — the summary is command + exit
// status + tool hash, so one noisy check cannot blow the context budget (§6).
type CheckCommand struct {
	Command  string `json:"command"`
	Exit     int    `json:"exit"`
	ToolHash string `json:"tool_hash,omitempty"`
}

// CheckView is one verification-evidence record for a change.
type CheckView struct {
	Tree      string         `json:"tree"`    // the tree hash this evidence was produced against
	Passed    bool           `json:"passed"`  // every command exited zero
	Current   bool           `json:"current"` // the evidence tree matches the change's current tree
	Timestamp int64          `json:"timestamp"`
	Results   []CheckCommand `json:"results,omitempty"`
}

// Checks returns the verification evidence recorded for a change, newest first as
// stored. Each record carries `current` so a reader can tell a fresh pass from
// evidence made stale by a later edit to the tree.
func (q *Query) Checks(change multihash.Multihash) ([]CheckView, error) {
	evs, err := check.List(q.r, change)
	if err != nil {
		return nil, wrap(err)
	}
	// The change's current tree, to flag stale evidence.
	currentTree := ""
	if o, err := q.r.Objects.Get(change); err == nil {
		if c, err := o.AsChange(); err == nil {
			currentTree = c.Tree.Hex()
		}
	}
	out := make([]CheckView, len(evs))
	for i, ev := range evs {
		cv := CheckView{
			Tree:      ev.Tree,
			Passed:    ev.Passed,
			Current:   ev.Tree == currentTree,
			Timestamp: ev.Timestamp,
		}
		for _, r := range ev.Results {
			cv.Results = append(cv.Results, CheckCommand{Command: r.Command, Exit: r.Exit, ToolHash: r.ToolHash})
		}
		out[i] = cv
	}
	return out, nil
}
