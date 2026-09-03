package mcp

import (
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/readapi"
	"github.com/dividebyzero/claude-experiments/varvig/internal/task"
)

// readLog wraps the query layer so that every hash a tool resolves is folded
// into the task's read set — the gate is the only component that knows what a
// task actually read, and that read log becomes part of the change a proposal
// produces (MCP spec §5, auth §8.2).
//
// It is implemented here, in the query-layer call path, rather than in each
// tool handler, "or the tenth tool added will forget" (§5). The hash is
// recorded, not the path — paths are ambiguous across bases; hashes are not —
// and it is recorded on every call, including ones that error after resolving:
// the id passed in is always recorded before the underlying lookup can fail.
type readLog struct {
	q     *readapi.Query
	reads *task.ReadSet
}

func newReadLog(q *readapi.Query, reads *task.ReadSet) *readLog {
	return &readLog{q: q, reads: reads}
}

func (rl *readLog) record(hashes ...string) {
	for _, h := range hashes {
		rl.reads.Record(h)
	}
}

// Tree records the resolved-against id (even if the listing then fails) and, on
// success, the subtree root, the listing hash, and every entry hash.
func (rl *readLog) Tree(id multihash.Multihash, path string) (readapi.TreeListing, error) {
	rl.record(id.Hex())
	l, err := rl.q.Tree(id, path)
	if err != nil {
		return l, err
	}
	rl.record(l.Root, l.Hash)
	for _, e := range l.Entries {
		rl.record(e.Hash)
	}
	return l, nil
}

// Blob records the blob id and returns its content.
func (rl *readLog) Blob(id multihash.Multihash) ([]byte, error) {
	rl.record(id.Hex())
	return rl.q.Blob(id)
}

// Change records the change id and, on success, its tree.
func (rl *readLog) Change(id multihash.Multihash) (readapi.ChangeView, error) {
	rl.record(id.Hex())
	v, err := rl.q.Change(id)
	if err != nil {
		return v, err
	}
	rl.record(v.Hash, v.Tree)
	return v, nil
}

// Log records the start id and every change hash the walk returns.
func (rl *readLog) Log(start multihash.Multihash, limit int) ([]readapi.LogEntryView, error) {
	rl.record(start.Hex())
	entries, err := rl.q.Log(start, limit)
	if err != nil {
		return entries, err
	}
	for _, e := range entries {
		rl.record(e.Hash)
	}
	return entries, nil
}

// Resolve records the resolved hash on success. A ref or partial hash that
// resolves to nothing records nothing.
func (rl *readLog) Resolve(refOrHash string) (multihash.Multihash, error) {
	id, err := rl.q.Resolve(refOrHash)
	if err != nil {
		return nil, err
	}
	rl.record(id.Hex())
	return id, nil
}

// Tickets records each listed ticket's id and head revision.
func (rl *readLog) Tickets() ([]readapi.TicketView, error) {
	views, err := rl.q.Tickets()
	if err != nil {
		return views, err
	}
	for _, v := range views {
		rl.record(v.ID, v.Head)
	}
	return views, nil
}

// Ticket records the ticket id (even if the read then fails) and, on success,
// its head revision.
func (rl *readLog) Ticket(id multihash.Multihash) (readapi.TicketDetail, error) {
	rl.record(id.Hex())
	d, err := rl.q.Ticket(id)
	if err != nil {
		return d, err
	}
	rl.record(d.Head)
	rl.record(d.Implementers...) // the commits behind the derived status
	return d, nil
}

// TicketArtifacts records each artifact's content hash and its producing change.
func (rl *readLog) TicketArtifacts(id multihash.Multihash) ([]readapi.ArtifactView, error) {
	arts, err := rl.q.TicketArtifacts(id)
	if err != nil {
		return arts, err
	}
	for _, a := range arts {
		rl.record(a.ContentHash)
		if a.ProducedBy != "" {
			rl.record(a.ProducedBy)
		}
	}
	return arts, nil
}
