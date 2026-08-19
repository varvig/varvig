package ticket

import (
	"encoding/json"
	"sort"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/notes"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/reserved"
)

// Comment is one entry in a ticket's discussion (tickets §5.2). It is ungoverned
// data — never signed, never consulted by scoring or attestation — so a bridge
// peer can mirror tracker comments in freely without touching governance.
//
// Origin and OriginID record where a mirrored comment came from so a connector
// can suppress echo: a comment already present with the same (origin, origin_id)
// is not imported again. A comment authored natively in varvig leaves both empty.
type Comment struct {
	Author    string `json:"author"`
	Body      string `json:"body"`
	Origin    string `json:"origin,omitempty"`
	OriginID  string `json:"origin_id,omitempty"`
	Timestamp int64  `json:"ts"`
}

// AddComment appends a comment to a ticket's discussion. Comments accrete as
// notes in the reserved varvig/discussion namespace, keyed by ticket id, so
// adding one never touches the ticket's intent chain or its governance.
func AddComment(r *repo.Repo, id multihash.Multihash, c Comment, now int64) error {
	if c.Timestamp == 0 {
		c.Timestamp = now
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return err
	}
	author := c.Author
	if author == "" {
		author = "anon"
	}
	_, err = notes.New(r).Add(reserved.NoteDiscussion, id, payload, author, now)
	return err
}

// Comments returns a ticket's discussion in chronological order (oldest first).
// The note chain is newest-first, so it is reversed here; ties keep chain order.
func Comments(r *repo.Repo, id multihash.Multihash) ([]Comment, error) {
	chain, err := notes.New(r).List(reserved.NoteDiscussion, id)
	if err != nil {
		return nil, err
	}
	// chain[0] is the newest; walk it in reverse so the result is oldest-first,
	// then a stable sort by timestamp keeps that chain order for equal stamps.
	out := make([]Comment, 0, len(chain))
	for i := len(chain) - 1; i >= 0; i-- {
		var c Comment
		if err := json.Unmarshal(chain[i].Note.Payload, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Timestamp < out[j].Timestamp })
	return out, nil
}
